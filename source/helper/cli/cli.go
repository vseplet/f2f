package cli

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/vseplet/f2f/source/helper/config"
	"github.com/vseplet/f2f/source/helper/identity"
)

// Interactive reports whether stdin/stdout are a terminal — i.e. whether
// the huh picker can run. False under launchd, pipes, or `f2f up`.
func Interactive() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
}

// RunCamp dispatches the `f2f camp …` subcommands. Each runs and the
// process exits — camp management never brings the transport up; the
// user runs `sudo f2f` for that.
func RunCamp(store *config.Store, args []string) error {
	m := NewManager(store)
	if len(args) == 0 {
		return campUsage()
	}
	switch args[0] {
	case "ls", "list":
		return m.cmdList()
	case "new", "create":
		return m.cmdNew(args[1:])
	case "join":
		return m.cmdJoin(args[1:])
	case "use", "switch":
		return m.cmdUse(args[1:])
	case "rm", "remove", "forget":
		return m.cmdRemove(args[1:])
	case "invite":
		return m.cmdInvite(args[1:])
	case "-h", "--help", "help":
		return campUsage()
	default:
		return fmt.Errorf("unknown camp command %q\n\n%s", args[0], campUsageText)
	}
}

const campUsageText = `usage: f2f camp <command>

  ls                       list known camps (★ = last used)
  new [label] [--name N]   create a new camp
  join [token] [--name N]  join a camp from an invite token
  use <id|label>           mark a camp as last-used
  rm <id|label>            forget a camp (deletes its keys + data)
  invite [id|label] [--ttl 24h]
                           mint an invite token for a camp you own

With no label/token, 'new' and 'join' prompt interactively.`

func campUsage() error {
	fmt.Println(campUsageText)
	return nil
}

func (m *Manager) cmdList() error {
	st, err := m.List()
	if err != nil {
		return err
	}
	if len(st.KnownCamps) == 0 {
		fmt.Println("no camps yet — run 'f2f camp new' or 'sudo f2f' to create one")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "\tLABEL\tNAME\tCAMP_ID")
	for _, kc := range st.KnownCamps {
		mark := " "
		if kc.ID == st.LastCampID {
			mark = "★"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", mark, identity.CampLabel(kc.ID), kc.Name, kc.ID)
	}
	return w.Flush()
}

func (m *Manager) cmdNew(args []string) error {
	fs := flag.NewFlagSet("camp new", flag.ContinueOnError)
	name := fs.String("name", "", "your display name in the camp")
	if err := fs.Parse(args); err != nil {
		return err
	}
	label := fs.Arg(0)
	// Both supplied on the command line → non-interactive create.
	// Otherwise fall back to the interactive form (when a TTY exists).
	if label != "" && *name != "" {
		c, _, err := m.Create(label, *name)
		if err != nil {
			return err
		}
		return reportCreated(c.CampID)
	}
	if !Interactive() {
		return fmt.Errorf("camp new: provide a label and --name (no terminal for interactive prompt)")
	}
	c, _, err := m.createInteractive()
	if err != nil || c == nil {
		return err
	}
	return reportCreated(c.CampID)
}

func (m *Manager) cmdJoin(args []string) error {
	fs := flag.NewFlagSet("camp join", flag.ContinueOnError)
	name := fs.String("name", "", "your display name in the camp")
	if err := fs.Parse(args); err != nil {
		return err
	}
	token := fs.Arg(0)
	if token != "" && *name != "" {
		id, err := m.Join(token, *name)
		if err != nil {
			return err
		}
		return reportJoined(id)
	}
	if !Interactive() {
		return fmt.Errorf("camp join: provide a token and --name (no terminal for interactive prompt)")
	}
	c, _, err := m.joinInteractive()
	if err != nil || c == nil {
		return err
	}
	return reportJoined(c.CampID)
}

func (m *Manager) cmdUse(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: f2f camp use <id|label>")
	}
	id, err := m.Use(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("last-used camp set to %s — run 'sudo f2f' to bring it up\n", identity.CampLabel(id))
	return nil
}

func (m *Manager) cmdRemove(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: f2f camp rm <id|label>")
	}
	id, err := m.Remove(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("forgot camp %s (keys and data deleted)\n", identity.CampLabel(id))
	return nil
}

func (m *Manager) cmdInvite(args []string) error {
	fs := flag.NewFlagSet("camp invite", flag.ContinueOnError)
	ttl := fs.Duration("ttl", 24*time.Hour, "how long the invite stays valid")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ref := fs.Arg(0)
	if ref == "" {
		// default to the last-used camp
		st, err := m.List()
		if err != nil {
			return err
		}
		if st.LastCampID == "" {
			return fmt.Errorf("camp invite: no camp specified and no last-used camp")
		}
		ref = st.LastCampID
	}
	token, err := m.Invite(ref, *ttl)
	if err != nil {
		return err
	}
	fmt.Println(token)
	return nil
}

func reportCreated(id string) error {
	fmt.Printf("created camp %s\nrun 'sudo f2f' to bring it up\n", identity.CampLabel(id))
	return nil
}

func reportJoined(id string) error {
	fmt.Printf("joined camp %s\nrun 'sudo f2f' to bring it up\n", identity.CampLabel(id))
	return nil
}
