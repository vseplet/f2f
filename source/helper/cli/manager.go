// Package cli owns the camp lifecycle: creating, listing, selecting,
// joining (via invite), removing camps, and minting invites. This is the
// management layer the engine used to carry inline — the engine now only
// brings transport up for an already-provisioned camp+identity that this
// package hands it.
//
// Two entry points:
//
//   - RunCamp(store, args): the `f2f camp …` subcommands (run and exit).
//   - SelectCamp / LoadForStart: used by the orchestrator in main to
//     decide which camp to bring up, returning the loaded config +
//     identity for engine.Start.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vseplet/f2f/source/helper/config"
	"github.com/vseplet/f2f/source/helper/identity"
)

// Manager wraps the config Store with camp-lifecycle operations.
type Manager struct {
	store *config.Store
}

// NewManager returns a Manager over store.
func NewManager(store *config.Store) *Manager {
	return &Manager{store: store}
}

// List returns the global state — last-used camp_id plus the roster of
// every known camp.
func (m *Manager) List() (*config.State, error) {
	return m.store.LoadState()
}

// Resolve maps a user-supplied reference (a full camp_id, or a human
// label) to a known camp_id. Exact id match wins; otherwise the label is
// matched against the suffix of each known camp_id. Returns an error
// when nothing matches or a label is ambiguous.
func (m *Manager) Resolve(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("empty camp reference")
	}
	st, err := m.store.LoadState()
	if err != nil {
		return "", err
	}
	var byLabel []string
	for _, kc := range st.KnownCamps {
		if kc.ID == ref {
			return kc.ID, nil
		}
		if identity.CampLabel(kc.ID) == ref {
			byLabel = append(byLabel, kc.ID)
		}
	}
	switch len(byLabel) {
	case 1:
		return byLabel[0], nil
	case 0:
		return "", fmt.Errorf("no known camp matches %q", ref)
	default:
		return "", fmt.Errorf("label %q is ambiguous (%d camps); use the full camp_id", ref, len(byLabel))
	}
}

// Create provisions a brand-new camp: it generates the Ed25519 identity,
// derives camp_id = <pub>_<label>, writes the keypair under
// /var/lib/f2f/identity/<camp_id>/, writes the per-camp config, and marks
// the camp as last-used. Returns the loaded config + identity ready for
// engine.Start. (This is the create path the engine used to run inline.)
func (m *Manager) Create(label, name string) (*config.Camp, *identity.Identity, error) {
	label = strings.TrimSpace(label)
	name = strings.TrimSpace(name)
	if !ValidLabel(label) {
		return nil, nil, fmt.Errorf("label must match [A-Za-z0-9_.-]+ (got %q)", label)
	}
	if name == "" {
		return nil, nil, fmt.Errorf("a display name is required")
	}
	id, err := identity.Generate()
	if err != nil {
		return nil, nil, fmt.Errorf("generate identity: %w", err)
	}
	campID := id.PubHex() + "_" + label
	if err := id.Save(identity.DirFor(campID)); err != nil {
		return nil, nil, fmt.Errorf("save identity: %w", err)
	}
	c := config.NewCamp(campID, name)
	c.Identity.Pub = id.PubHex()
	c.Identity.Fingerprint = id.Fingerprint()
	if err := m.store.SaveCamp(campID, c); err != nil {
		return nil, nil, fmt.Errorf("save camp config: %w", err)
	}
	if err := m.markKnown(campID, name); err != nil {
		return nil, nil, err
	}
	return c, id, nil
}

// Join records a camp from an owner-signed invite token. The token is
// fully verified (signature + expiry) by identity.ParseInvite before we
// write anything. We persist a per-camp config with our chosen name and
// mark the camp last-used; our own identity is generated lazily on the
// first Start (we are not the owner, so we have no key yet). Returns the
// camp_id we joined.
func (m *Manager) Join(token, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("a display name is required")
	}
	body, err := identity.ParseInvite(token)
	if err != nil {
		return "", err
	}
	if existing, _ := m.store.SnapshotCamp(body.CampID); existing == nil {
		c := config.NewCamp(body.CampID, name)
		if err := m.store.SaveCamp(body.CampID, c); err != nil {
			return "", fmt.Errorf("save camp config: %w", err)
		}
	} else {
		// Already known — just refresh our display name.
		if err := m.store.UpdateCamp(body.CampID, func(c *config.Camp) {
			c.Identity.Name = name
		}); err != nil {
			return "", err
		}
	}
	if err := m.markKnown(body.CampID, name); err != nil {
		return "", err
	}
	return body.CampID, nil
}

// Use marks an existing camp as last-used so the next bare start (or
// `f2f up`) brings it up. Accepts an id or a label.
func (m *Manager) Use(ref string) (string, error) {
	id, err := m.Resolve(ref)
	if err != nil {
		return "", err
	}
	name := ""
	if c, _ := m.store.SnapshotCamp(id); c != nil {
		name = c.Identity.Name
	}
	if err := m.markKnown(id, name); err != nil {
		return "", err
	}
	return id, nil
}

// Remove forgets a camp entirely: deletes its identity keypair, its
// per-camp directory (config + logs + DBs + shared/drops), and its entry
// in the known-camps roster. Clears last_camp_id if it pointed here.
func (m *Manager) Remove(ref string) (string, error) {
	id, err := m.Resolve(ref)
	if err != nil {
		return "", err
	}
	// Per-camp directory under ~/.f2f/<id>. Computed from Dir() rather
	// than CampDir() so we don't recreate it as a side effect.
	_ = os.RemoveAll(filepath.Join(m.store.Dir(), id))
	_ = os.RemoveAll(identity.DirFor(id))
	st, err := m.store.LoadState()
	if err != nil {
		return "", err
	}
	kept := st.KnownCamps[:0]
	for _, kc := range st.KnownCamps {
		if kc.ID != id {
			kept = append(kept, kc)
		}
	}
	st.KnownCamps = kept
	if st.LastCampID == id {
		st.LastCampID = ""
	}
	if err := m.store.SaveState(st); err != nil {
		return "", err
	}
	return id, nil
}

// Invite mints an owner-signed bearer token for an existing camp. Only
// the camp owner (who holds the identity that derived the camp_id) can
// produce a token the server will accept — so this loads that identity.
func (m *Manager) Invite(ref string, ttl time.Duration) (string, error) {
	id, err := m.Resolve(ref)
	if err != nil {
		return "", err
	}
	idt, err := identity.LoadOrGenerate(identity.DirFor(id))
	if err != nil {
		return "", fmt.Errorf("load identity: %w", err)
	}
	return idt.GenerateInvite(id, ttl)
}

// LoadForStart loads the per-camp config and identity for campID and
// returns them ready for engine.Start. It mirrors the identity's
// pub/fingerprint into the config (so the UI can show them offline) when
// they drift — the one config write that used to live in engine.Start.
func (m *Manager) LoadForStart(campID string) (*config.Camp, *identity.Identity, error) {
	c, err := m.store.SnapshotCamp(campID)
	if err != nil {
		return nil, nil, err
	}
	if c == nil {
		return nil, nil, fmt.Errorf("no config for camp %q", campID)
	}
	idt, err := identity.LoadOrGenerate(identity.DirFor(campID))
	if err != nil {
		return nil, nil, fmt.Errorf("identity %s: %w", campID, err)
	}
	pub, fp := idt.PubHex(), idt.Fingerprint()
	if c.Identity.Pub != pub || c.Identity.Fingerprint != fp {
		if err := m.store.UpdateCamp(campID, func(cc *config.Camp) {
			cc.Identity.Pub = pub
			cc.Identity.Fingerprint = fp
		}); err != nil {
			return nil, nil, err
		}
		c.Identity.Pub = pub
		c.Identity.Fingerprint = fp
	}
	return c, idt, nil
}

// Touch marks campID as last-used and ensures it appears in the
// known-camps roster (refreshing name when given). Exported for callers
// that already hold a resolved camp_id (e.g. the web start handler on a
// resume) and just need the state-file bookkeeping.
func (m *Manager) Touch(campID, name string) error {
	return m.markKnown(campID, name)
}

// markKnown sets last_camp_id to id and upserts it into the known-camps
// roster (refreshing name when given). The state-file bookkeeping the
// engine used to do in Start.
func (m *Manager) markKnown(id, name string) error {
	st, err := m.store.LoadState()
	if err != nil {
		return err
	}
	st.LastCampID = id
	found := false
	for i := range st.KnownCamps {
		if st.KnownCamps[i].ID == id {
			if name != "" {
				st.KnownCamps[i].Name = name
			}
			found = true
			break
		}
	}
	if !found {
		st.KnownCamps = append(st.KnownCamps, config.KnownCamp{ID: id, Name: name})
	}
	return m.store.SaveState(st)
}

// ValidLabel reports whether label uses only the characters the camp
// server's name regex accepts, so the derived camp_id = <pub>_<label>
// stays acceptable. (Moved here from the engine's create path.)
func ValidLabel(label string) bool {
	if label == "" {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		ok := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '_' || c == '.' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}
