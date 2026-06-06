// Package clog centralises process logging.
//
// The standard library log.* stream is routed to a file (plus any extra
// sinks such as the UI tap). The terminal only sees that stream when the
// process is started with console mirroring on. Console() is the escape
// hatch for lines the user must see regardless of that setting — the
// startup banner, fatal errors — and always reaches both the terminal
// and the log file.
//
// Existing log.Printf call sites don't change: Init just redirects the
// default logger's output.
package clog

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
)

var (
	mu         sync.Mutex
	fileW      *os.File    // current log file; nil until Init
	toConsole  bool        // log.* also mirrored to stderr
	extraSinks []io.Writer // additional sinks (preserved across SwitchTo)
)

// Init opens (creating/appending) the bootstrap log file at path, routes
// log.* to it plus the UI tap and any extra sinks, and mirrors to stderr
// only when console is true. The returned Closer should be closed on
// shutdown. SwitchTo can later re-point the file (e.g. to a per-camp log).
func Init(path string, console bool, extra ...io.Writer) (io.Closer, error) {
	mu.Lock()
	defer mu.Unlock()
	f, err := openLog(path)
	if err != nil {
		return nil, err
	}
	fileW = f
	toConsole = console
	extraSinks = extra
	applyOutputLocked()
	return closerFunc(closeCurrent), nil
}

// SwitchTo re-points the log to a new file (creating/appending), preserving
// the tap/console/extra sinks, and closes the previous file. Used to move
// from the bootstrap log to the per-camp log once a camp is selected.
func SwitchTo(path string) error {
	mu.Lock()
	defer mu.Unlock()
	f, err := openLog(path)
	if err != nil {
		return err
	}
	old := fileW
	fileW = f
	applyOutputLocked()
	if old != nil {
		_ = old.Close()
	}
	return nil
}

func openLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("log dir %q: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file %q: %w", path, err)
	}
	chownToInvoker(path)
	return f, nil
}

// applyOutputLocked rebuilds the log MultiWriter from the current sinks.
func applyOutputLocked() {
	sinks := []io.Writer{io.Writer(fileW), tap}
	sinks = append(sinks, extraSinks...)
	if toConsole {
		sinks = append(sinks, os.Stderr)
	}
	log.SetOutput(io.MultiWriter(sinks...))
}

func closeCurrent() error {
	mu.Lock()
	defer mu.Unlock()
	if fileW == nil {
		return nil
	}
	err := fileW.Close()
	fileW = nil
	return err
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// Console prints a line the user must see — to the terminal and into the
// log record — independent of whether log.* is file-only. Use for the
// startup banner and critical/fatal messages.
func Console(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if fileW == nil {
		// Before Init the default logger still targets stderr; print once.
		fmt.Fprintln(os.Stderr, msg)
		return
	}
	if !toConsole {
		fmt.Fprintln(os.Stderr, msg) // log.* isn't on stderr — surface it
	}
	log.Print(msg) // record in file/tap (+stderr when toConsole)
}

// Fatal reports a message via Console and exits non-zero.
func Fatal(format string, args ...any) {
	Console("FATAL: "+format, args...)
	os.Exit(1)
}

// chownToInvoker hands the file to $SUDO_USER (we run under sudo), so the
// log stays readable/manageable from the user's account. Best-effort.
func chownToInvoker(path string) {
	su := os.Getenv("SUDO_USER")
	if su == "" {
		return
	}
	u, err := user.Lookup(su)
	if err != nil {
		return
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	_ = os.Chown(path, uid, gid)
}
