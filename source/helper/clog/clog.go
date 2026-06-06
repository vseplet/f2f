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
	"strconv"
)

var (
	fileW     io.Writer // log file; nil until Init
	toConsole bool      // log.* also mirrored to stderr
)

// Init opens (creating/appending) the log file at path, routes log.* to
// it plus any extra sinks, and mirrors to stderr only when console is
// true. The returned Closer should be closed on shutdown. The file is
// chowned to $SUDO_USER so the user can read it after a sudo run.
func Init(path string, console bool, extra ...io.Writer) (io.Closer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file %q: %w", path, err)
	}
	chownToInvoker(path)

	fileW = f
	toConsole = console
	// file + the UI tap (always) + any extra sinks; stderr only on console.
	sinks := append([]io.Writer{io.Writer(f), tap}, extra...)
	if console {
		sinks = append(sinks, os.Stderr)
	}
	log.SetOutput(io.MultiWriter(sinks...))
	return f, nil
}

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
