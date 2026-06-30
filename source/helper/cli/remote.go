package cli

// `f2f remote` — interactive TUI to choose which channels may open THIS node's
// terminal (shell) and desktop (VNC). It is a thin HTTP client to the already-
// running helper's loopback API: the live exposure state lives in that process,
// so editing config directly would not apply until restart. Handy on a headless
// VPS where opening the web UI means an SSH tunnel.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
)

const defaultRemoteBind = "127.0.0.1:2202"

type remoteChannel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type remoteExposure struct {
	Terminal []string `json:"terminal"`
	Desktop  []string `json:"desktop"`
}

// RunRemote drives the exposure picker and applies the result.
func RunRemote(args []string) error {
	fs := flag.NewFlagSet("f2f remote", flag.ExitOnError)
	bind := fs.String("bind", defaultRemoteBind, "loopback address of the running helper")
	_ = fs.Parse(args)
	base := "http://" + *bind

	chans, err := fetchChannels(base)
	if err != nil {
		return fmt.Errorf("can't reach helper at %s (is it running?): %w", *bind, err)
	}
	cur, err := fetchExposure(base)
	if err != nil {
		return err
	}

	sort.Slice(chans, func(i, j int) bool { return chans[i].Name < chans[j].Name })
	// Normalize into a deduped list with the camp-wide "general" offered exactly
	// once at the top. /api/channels returns only created blocks, but an older peer
	// can also surface a "general" entry — collapsing both into the canonical
	// GeneralBID avoids two options sharing value "general" (huh can't toggle a
	// duplicate-valued option, which read as "can't select general").
	seen := map[string]bool{"general": true}
	dedup := []remoteChannel{{ID: "general", Name: "general"}}
	for _, c := range chans {
		if c.Name == "general" || c.ID == "" || seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		dedup = append(dedup, c)
	}
	chans = dedup

	// huh pre-selects from the bound slice and writes the result back into it —
	// per-option .Selected() does NOT compose with .Value() (the empty binding
	// wins and toggles never persist). So seed the bindings with current exposure.
	pickTerm := append([]string{}, cur.Terminal...)
	pickDesk := append([]string{}, cur.Desktop...)
	termOpts := make([]huh.Option[string], len(chans))
	deskOpts := make([]huh.Option[string], len(chans))
	for i, c := range chans {
		label := "#" + c.Name
		termOpts[i] = huh.NewOption(label, c.ID)
		deskOpts[i] = huh.NewOption(label, c.ID)
	}
	form := huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("terminal — каналы, которым открыт мой shell (space — выбрать, enter — применить)").
			Options(termOpts...).
			Value(&pickTerm),
		huh.NewMultiSelect[string]().
			Title("desktop — каналы, которым открыт мой рабочий стол").
			Options(deskOpts...).
			Value(&pickDesk),
	))
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			fmt.Println("отменено, без изменений")
			return nil
		}
		return err
	}

	next := remoteExposure{Terminal: nonNil(pickTerm), Desktop: nonNil(pickDesk)}
	if err := postExposure(base, next); err != nil {
		return err
	}
	name := nameMap(chans)
	fmt.Println("сохранено:")
	fmt.Printf("  terminal: %s\n", labelList(next.Terminal, name))
	fmt.Printf("  desktop:  %s\n", labelList(next.Desktop, name))
	return nil
}

func remoteHTTP() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

func fetchChannels(base string) ([]remoteChannel, error) {
	resp, err := remoteHTTP().Get(base + "/api/channels")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GET /api/channels: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out []remoteChannel
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func fetchExposure(base string) (remoteExposure, error) {
	var ex remoteExposure
	resp, err := remoteHTTP().Get(base + "/api/remote/exposure")
	if err != nil {
		return ex, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return ex, fmt.Errorf("GET /api/remote/exposure: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return ex, json.NewDecoder(resp.Body).Decode(&ex)
}

func postExposure(base string, ex remoteExposure) error {
	body, _ := json.Marshal(ex)
	resp, err := remoteHTTP().Post(base+"/api/remote/exposure", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST /api/remote/exposure: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

func nameMap(cs []remoteChannel) map[string]string {
	m := make(map[string]string, len(cs))
	for _, c := range cs {
		m[c.ID] = c.Name
	}
	return m
}

func labelList(ids []string, name map[string]string) string {
	if len(ids) == 0 {
		return "(никому)"
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		if n := name[id]; n != "" {
			parts[i] = "#" + n
		} else {
			parts[i] = id
		}
	}
	return strings.Join(parts, ", ")
}
