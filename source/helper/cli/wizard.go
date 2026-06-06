package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/vseplet/f2f/source/helper/config"
	"github.com/vseplet/f2f/source/helper/identity"
)

// Sentinel option values for the picker — prefixed with NUL so they can
// never collide with a real camp_id.
const (
	actCreate = "\x00create"
	actJoin   = "\x00join"
)

// SelectCamp decides which camp to bring up. When interactive, it shows
// the huh picker (choose a known camp, create a new one, or join via
// invite). When not (no TTY, or `f2f up`), it auto-selects the last-used
// camp. Returns (nil, nil, nil) when there is nothing to start or the
// user quit the picker — the caller then runs idle until the user picks
// a camp from the UI or restarts.
func (m *Manager) SelectCamp(interactive bool) (*config.Camp, *identity.Identity, error) {
	if !interactive {
		st, err := m.List()
		if err != nil {
			return nil, nil, err
		}
		if st.LastCampID == "" {
			return nil, nil, nil
		}
		return m.LoadForStart(st.LastCampID)
	}
	return m.chooseCamp()
}

func (m *Manager) chooseCamp() (*config.Camp, *identity.Identity, error) {
	st, err := m.List()
	if err != nil {
		return nil, nil, err
	}

	opts := make([]huh.Option[string], 0, len(st.KnownCamps)+2)
	for _, kc := range st.KnownCamps {
		title := identity.CampLabel(kc.ID)
		if kc.Name != "" {
			title = fmt.Sprintf("%s  (as %s)", title, kc.Name)
		}
		opts = append(opts, huh.NewOption(title, kc.ID))
	}
	opts = append(opts,
		huh.NewOption("➕  Create a new camp", actCreate),
		huh.NewOption("🔗  Join with an invite", actJoin),
	)

	sel := st.LastCampID
	if sel == "" {
		sel = actCreate
	}
	err = huh.NewSelect[string]().
		Title("f2f — choose a camp").
		Options(opts...).
		Value(&sel).
		Run()
	if err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return nil, nil, nil
		}
		return nil, nil, err
	}

	switch sel {
	case actCreate:
		return m.createInteractive()
	case actJoin:
		return m.joinInteractive()
	default:
		if _, err := m.Use(sel); err != nil {
			return nil, nil, err
		}
		return m.LoadForStart(sel)
	}
}

// createInteractive collects a label + display name and provisions the
// camp. Returns (nil, nil, nil) if the user aborts the form.
func (m *Manager) createInteractive() (*config.Camp, *identity.Identity, error) {
	var label, name string
	err := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Camp label").
			Description("short name, characters [A-Za-z0-9_.-]").
			Value(&label).
			Validate(func(s string) error {
				if !ValidLabel(strings.TrimSpace(s)) {
					return fmt.Errorf("only letters, digits and _ . -")
				}
				return nil
			}),
		huh.NewInput().
			Title("Your display name in this camp").
			Value(&name).
			Validate(required),
	)).Run()
	if err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	return m.Create(label, name)
}

// joinInteractive collects an invite token + display name and records
// the camp. Returns (nil, nil, nil) if the user aborts the form.
func (m *Manager) joinInteractive() (*config.Camp, *identity.Identity, error) {
	var token, name string
	err := huh.NewForm(huh.NewGroup(
		huh.NewText().
			Title("Invite token").
			Description("paste the token your camp owner sent you").
			Value(&token).
			Validate(func(s string) error {
				if _, err := identity.ParseInvite(s); err != nil {
					return err
				}
				return nil
			}),
		huh.NewInput().
			Title("Your display name in this camp").
			Value(&name).
			Validate(required),
	)).Run()
	if err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	id, err := m.Join(token, name)
	if err != nil {
		return nil, nil, err
	}
	return m.LoadForStart(id)
}

func required(s string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("required")
	}
	return nil
}
