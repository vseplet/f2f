// Package secrets is a Doppler-style secret store: a vault (project) holds
// environments (dev/staging/prod…), and each environment holds key/value
// secrets AND its own share list — so prod can be shared to one channel and
// dev to another, independently. Environments are flat (no inheritance): each
// is an independent, independently-shared set, which keeps access predictable.
//
// Unlike chat/notes it does NOT ride the block log — secrets must be mutable
// and truly deletable (the append-only log would keep every old plaintext value
// forever and replicate it un-deletably). So this is a plain per-camp sqlite
// store with real UPDATE/DELETE.
//
// Sharing model ("owner-served pull"): each peer stores the vaults it owns. An
// environment shared to a channel is served to that channel's members on demand
// over the bus (secrets.list) — never replicated. Members read each other's
// shared environments live; only the owner edits its own.
//
// NOTE: values are stored in cleartext at rest. The tiered encryption / keywrap
// / passkey-unlock design lives in docs/SECRETS.md and is layered on later.
package secrets

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (no cgo)

	"github.com/vseplet/f2f/source/helper/clog"
	"github.com/vseplet/f2f/source/helper/mesh/bus"
	"github.com/vseplet/f2f/source/helper/mesh/engine"
)

// TypeList is the bus message type for "list a channel's shared environments".
const TypeList = "secrets.list"

// Vault is a named project; it groups environments and is not shared directly.
type Vault struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Updated int64  `json:"updated"`
}

// Environment is a named config within a vault (dev/staging/prod). It carries
// its own secrets and its own share list (channel/DM bids).
type Environment struct {
	ID       string   `json:"id"`
	VaultID  string   `json:"vault_id"`
	Name     string   `json:"name"`
	Channels []string `json:"channels"`
	Updated  int64    `json:"updated"`
}

// Secret is one key/value entry inside an environment.
type Secret struct {
	ID      string `json:"id"`
	EnvID   string `json:"env_id"`
	Name    string `json:"name"`
	Value   string `json:"value"`
	Updated int64  `json:"updated"`
}

// OwnedEnv is an environment plus its secrets.
type OwnedEnv struct {
	Env     Environment `json:"env"`
	Secrets []Secret    `json:"secrets"`
}

// OwnedVault is a vault with its (filtered) environments, tagged with the peer
// that owns it. Used for the merged channel view (own + remote members').
type OwnedVault struct {
	Owner        string     `json:"owner"`      // owner pub ("" = us)
	OwnerName    string     `json:"owner_name"` // human label
	Mine         bool       `json:"mine"`       // editable (we own it)
	Vault        Vault      `json:"vault"`
	Environments []OwnedEnv `json:"environments"`
}

func marshalChannels(ch []string) string {
	if ch == nil {
		ch = []string{}
	}
	b, _ := json.Marshal(ch)
	return string(b)
}

func parseChannels(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Service is the secret store.
type Service struct {
	eng      *engine.Engine
	campDir  func(campID string) string
	bus      *bus.Service
	isMember func(bid, pub string) bool // injected channel-membership predicate

	mu     sync.Mutex
	db     *sql.DB
	dir    string
	campID string
}

// New constructs the service. campDir resolves a camp's directory.
func New(eng *engine.Engine, campDir func(campID string) string, b *bus.Service) *Service {
	return &Service{eng: eng, campDir: campDir, bus: b}
}

// SetMembershipCheck wires the channel-membership predicate (channels.IsMember).
func (s *Service) SetMembershipCheck(fn func(bid, pub string) bool) { s.isMember = fn }

// Register wires the bus handler. Call once after constructing the bus.
func (s *Service) Register() { s.bus.Handle(TypeList, s.handleList) }

// Start binds the store to a camp's directory. Safe to call on each camp join.
func (s *Service) Start(campID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.campID = campID
}

func (s *Service) selfPub() string { return s.eng.Status().IdentityPub }

// conn opens (lazily, dir-keyed) the per-camp secrets.db.
func (s *Service) conn() (*sql.DB, error) {
	dir := ""
	if s.campID != "" {
		dir = s.campDir(s.campID)
	}
	if dir == "" {
		return nil, errors.New("secrets: not in a camp")
	}
	if s.db != nil && s.dir == dir {
		return s.db, nil
	}
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
	path := filepath.Join(dir, "secrets.db")
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("secrets: open %s: %w", path, err)
	}
	if _, err := d.Exec(schema); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("secrets: schema: %w", err)
	}
	migrate(d)
	s.db, s.dir = d, dir
	return d, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS vaults (
  id      TEXT PRIMARY KEY,
  name    TEXT NOT NULL,
  updated INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS environments (
  id       TEXT PRIMARY KEY,
  vault_id TEXT NOT NULL,
  name     TEXT NOT NULL,
  channels TEXT NOT NULL DEFAULT '[]',
  updated  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS secrets (
  id      TEXT PRIMARY KEY,
  env_id  TEXT NOT NULL DEFAULT '',
  name    TEXT NOT NULL,
  value   TEXT NOT NULL,
  updated INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_env_vault ON environments(vault_id);`
// idx_secrets_env is created in migrate(), AFTER the env_id column exists — a
// pre-environments db has no env_id yet, so indexing it here would fail.

func columnExists(d *sql.DB, table, col string) bool {
	rows, err := d.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk) == nil && name == col {
			return true
		}
	}
	return false
}

// migrate folds a pre-environments db (vaults carried channels, secrets keyed by
// vault_id) into the vault → environment → secret shape: each vault without an
// environment gets a "default" one carrying its old channels, and its secrets
// are repointed to it. Idempotent; safe to run on every open.
func migrate(d *sql.DB) {
	_, _ = d.Exec(`ALTER TABLE secrets ADD COLUMN env_id TEXT NOT NULL DEFAULT ''`)
	_, _ = d.Exec(`CREATE INDEX IF NOT EXISTS idx_secrets_env ON secrets(env_id)`)
	legacyChan := columnExists(d, "vaults", "channels")
	legacyVaultID := columnExists(d, "secrets", "vault_id")
	rows, err := d.Query(`SELECT id FROM vaults`)
	if err != nil {
		return
	}
	var vids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			vids = append(vids, id)
		}
	}
	rows.Close()
	for _, vid := range vids {
		var n int
		if d.QueryRow(`SELECT COUNT(*) FROM environments WHERE vault_id=?`, vid).Scan(&n); n > 0 {
			continue
		}
		ch := "[]"
		if legacyChan {
			_ = d.QueryRow(`SELECT channels FROM vaults WHERE id=?`, vid).Scan(&ch)
		}
		eid := newID()
		_, _ = d.Exec(`INSERT INTO environments(id,vault_id,name,channels,updated) VALUES(?,?,?,?,?)`,
			eid, vid, "default", ch, time.Now().Unix())
		if legacyVaultID {
			_, _ = d.Exec(`UPDATE secrets SET env_id=? WHERE vault_id=? AND (env_id='' OR env_id IS NULL)`, eid, vid)
		}
	}
	// Drop the legacy NOT-NULL vault_id column (secrets are keyed by env_id now);
	// otherwise inserts that omit it fail. After the backfill above. Ignored if
	// already gone / fresh table.
	if legacyVaultID {
		_, _ = d.Exec(`DROP INDEX IF EXISTS idx_secrets_vault`) // else DROP COLUMN refuses
		_, _ = d.Exec(`ALTER TABLE secrets DROP COLUMN vault_id`)
	}
}

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ---- vaults ----

// CreateVault adds an empty vault (project) and returns its id.
func (s *Service) CreateVault(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.conn()
	if err != nil {
		return "", err
	}
	id := newID()
	_, err = d.Exec(`INSERT INTO vaults(id,name,updated) VALUES(?,?,?)`, id, name, time.Now().Unix())
	return id, err
}

// RenameVault changes a vault's name.
func (s *Service) RenameVault(id, name string) error {
	return s.exec(`UPDATE vaults SET name=?,updated=? WHERE id=?`, name, time.Now().Unix(), id)
}

// DeleteVault drops a vault, its environments and all their secrets.
func (s *Service) DeleteVault(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.conn()
	if err != nil {
		return err
	}
	if _, err := d.Exec(`DELETE FROM secrets WHERE env_id IN (SELECT id FROM environments WHERE vault_id=?)`, id); err != nil {
		return err
	}
	if _, err := d.Exec(`DELETE FROM environments WHERE vault_id=?`, id); err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM vaults WHERE id=?`, id)
	return err
}

// ---- environments ----

// CreateEnv adds an environment to a vault, optionally pre-shared, returns its id.
func (s *Service) CreateEnv(vaultID, name string, channels []string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.conn()
	if err != nil {
		return "", err
	}
	id := newID()
	_, err = d.Exec(`INSERT INTO environments(id,vault_id,name,channels,updated) VALUES(?,?,?,?,?)`,
		id, vaultID, name, marshalChannels(channels), time.Now().Unix())
	return id, err
}

// RenameEnv changes an environment's name.
func (s *Service) RenameEnv(id, name string) error {
	return s.exec(`UPDATE environments SET name=?,updated=? WHERE id=?`, name, time.Now().Unix(), id)
}

// SetEnvChannels replaces the set of channels an environment is shared to.
func (s *Service) SetEnvChannels(id string, channels []string) error {
	return s.exec(`UPDATE environments SET channels=?,updated=? WHERE id=?`, marshalChannels(channels), time.Now().Unix(), id)
}

// DeleteEnv drops an environment and its secrets.
func (s *Service) DeleteEnv(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.conn()
	if err != nil {
		return err
	}
	if _, err := d.Exec(`DELETE FROM secrets WHERE env_id=?`, id); err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM environments WHERE id=?`, id)
	return err
}

// ---- secrets ----

// AddSecret inserts a key/value into an environment and returns its id.
func (s *Service) AddSecret(envID, name, value string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.conn()
	if err != nil {
		return "", err
	}
	id := newID()
	_, err = d.Exec(`INSERT INTO secrets(id,env_id,name,value,updated) VALUES(?,?,?,?,?)`,
		id, envID, name, value, time.Now().Unix())
	return id, err
}

// SetSecret updates a secret's name/value.
func (s *Service) SetSecret(id, name, value string) error {
	return s.exec(`UPDATE secrets SET name=?,value=?,updated=? WHERE id=?`, name, value, time.Now().Unix(), id)
}

// DeleteSecret removes a secret.
func (s *Service) DeleteSecret(id string) error {
	return s.exec(`DELETE FROM secrets WHERE id=?`, id)
}

func (s *Service) exec(q string, args ...any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, err := s.conn()
	if err != nil {
		return err
	}
	_, err = d.Exec(q, args...)
	return err
}

// ---- reads ----

func (s *Service) allVaults(d *sql.DB) ([]Vault, error) {
	rows, err := d.Query(`SELECT id,name,updated FROM vaults ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Vault
	for rows.Next() {
		var v Vault
		if err := rows.Scan(&v.ID, &v.Name, &v.Updated); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Service) envsOf(d *sql.DB, vaultID string) ([]Environment, error) {
	rows, err := d.Query(`SELECT id,vault_id,name,channels,updated FROM environments WHERE vault_id=? ORDER BY name`, vaultID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Environment
	for rows.Next() {
		var e Environment
		var ch string
		if err := rows.Scan(&e.ID, &e.VaultID, &e.Name, &ch, &e.Updated); err != nil {
			return nil, err
		}
		e.Channels = parseChannels(ch)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Service) secretsOf(d *sql.DB, envID string) ([]Secret, error) {
	rows, err := d.Query(`SELECT id,env_id,name,value,updated FROM secrets WHERE env_id=? ORDER BY name`, envID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Secret
	for rows.Next() {
		var sec Secret
		if err := rows.Scan(&sec.ID, &sec.EnvID, &sec.Name, &sec.Value, &sec.Updated); err != nil {
			return nil, err
		}
		out = append(out, sec)
	}
	return out, rows.Err()
}

// ownedTree builds the vault→env→secret tree for our vaults. If channel != "",
// only environments shared to that channel are included (and vaults with none
// are dropped); "" returns everything.
func (s *Service) ownedTree(channel string) ([]OwnedVault, error) {
	d, err := s.conn()
	if err != nil {
		return nil, err
	}
	vaults, err := s.allVaults(d)
	if err != nil {
		return nil, err
	}
	out := make([]OwnedVault, 0, len(vaults))
	for _, v := range vaults {
		envs, err := s.envsOf(d, v.ID)
		if err != nil {
			return nil, err
		}
		oenvs := make([]OwnedEnv, 0, len(envs))
		for _, e := range envs {
			if channel != "" && !contains(e.Channels, channel) {
				continue
			}
			secs, err := s.secretsOf(d, e.ID)
			if err != nil {
				return nil, err
			}
			oenvs = append(oenvs, OwnedEnv{Env: e, Secrets: secs})
		}
		if channel != "" && len(oenvs) == 0 {
			continue
		}
		out = append(out, OwnedVault{Mine: true, Vault: v, Environments: oenvs})
	}
	return out, nil
}

// Local returns the full vault→env→secret tree we own.
func (s *Service) Local() ([]OwnedVault, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ownedTree("")
}

// ---- bus: owner-served pull of shared environments ----

type listReq struct {
	Channel string `json:"channel"` // channel bid
}

// handleList serves OUR environments shared to the requested channel (with
// their vault + secrets), but only to a peer that is a member of it.
func (s *Service) handleList(fromPub string, payload []byte) ([]byte, error) {
	var req listReq
	if err := json.Unmarshal(payload, &req); err != nil || req.Channel == "" {
		return nil, fmt.Errorf("secrets: bad list request")
	}
	if s.isMember == nil || !s.isMember(req.Channel, fromPub) {
		return nil, fmt.Errorf("secrets: not a member")
	}
	s.mu.Lock()
	owned, err := s.ownedTree(req.Channel)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return json.Marshal(owned)
}

// FetchChannel returns, for a channel, the environments shared to it by every
// member: ours plus each reachable member's, pulled live over the bus. Remote
// ones are read-only.
func (s *Service) FetchChannel(ctx context.Context, bid string) []OwnedVault {
	self := s.selfPub()
	s.mu.Lock()
	out, _ := s.ownedTree(bid)
	s.mu.Unlock()
	for i := range out {
		out[i].Owner = self
		out[i].OwnerName = "you"
	}
	if s.isMember == nil {
		return out
	}
	payload, _ := json.Marshal(listReq{Channel: bid})
	for _, pub := range s.bus.Peers() {
		if pub == self || !s.isMember(bid, pub) {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		resp, err := s.bus.Request(rctx, pub, TypeList, payload)
		cancel()
		if err != nil {
			clog.Debug("secrets", "list from %s: %v", s.bus.Label(pub), err)
			continue
		}
		var remote []OwnedVault
		if json.Unmarshal(resp, &remote) != nil {
			continue
		}
		for i := range remote {
			remote[i].Owner = pub
			remote[i].OwnerName = s.bus.Label(pub)
			remote[i].Mine = false
		}
		out = append(out, remote...)
	}
	return out
}
