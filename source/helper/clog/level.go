package clog

// Leveled, component-tagged logging on top of the standard log stream.
//
// Every line is shaped the same way:
//
//	15:04:05.000000 INFO  camp      peer alice/a1b2c3d4 entered roster
//	└── time (log pkg) ┘ └lvl┘ └ source ┘ └────── message ──────┘
//
// The time prefix comes from the standard logger (Ltime|Lmicroseconds set
// in main); we prepend a fixed-width LEVEL and a fixed-width source tag so
// columns line up and `grep` on a level or a subsystem just works. The
// source tag answers "which service/package emitted this" — pass the
// subsystem name ("camp", "bus", "awg", "drop", …), not a free-form string.
//
// Output still flows through the same MultiWriter Init built (file + UI tap
// + optional console), so the UI log stream and on-disk log get the new
// shape for free.
import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
)

// Level orders the four severities. Debug is the chattiest; Error the rarest.
type Level int32

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// minLevel is the lowest level that gets emitted. Lines below it are
// dropped before formatting. Defaults to Info; F2F_LOG overrides it.
var minLevel atomic.Int32

func init() { minLevel.Store(int32(ParseLevel(os.Getenv("F2F_LOG")))) }

// ParseLevel maps a string (debug/info/warn/error, case-insensitive) to a
// Level. Anything unrecognised — including "" — is Info.
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "dbg", "trace":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error", "err":
		return LevelError
	default:
		return LevelInfo
	}
}

// SetLevel changes the minimum emitted level at runtime (e.g. from a UI
// toggle). Safe to call concurrently with logging.
func SetLevel(l Level) { minLevel.Store(int32(l)) }

// Enabled reports whether a line at level l would be emitted. Use it to
// guard expensive argument computation: if clog.Enabled(clog.LevelDebug) {…}.
func Enabled(l Level) bool { return l >= Level(minLevel.Load()) }

// tag returns the fixed 5-char column for a level so messages align.
func (l Level) tag() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO "
	case LevelWarn:
		return "WARN "
	case LevelError:
		return "ERROR"
	default:
		return "?????"
	}
}

// emit formats and writes one leveled line, or drops it if below minLevel.
// source is padded to 8 cols (longest subsystem names — "identity",
// "firewall" — are 8); longer tags just overflow without breaking parsing.
func emit(l Level, source, format string, args ...any) {
	if !Enabled(l) {
		return
	}
	log.Printf("%s %-8s %s", l.tag(), source, fmt.Sprintf(format, args...))
}

// Debug logs verbose diagnostics — suppressed unless F2F_LOG=debug.
func Debug(source, format string, args ...any) { emit(LevelDebug, source, format, args...) }

// Info logs normal operational events — the default visible level.
func Info(source, format string, args ...any) { emit(LevelInfo, source, format, args...) }

// Warn logs recoverable problems worth attention but not failures.
func Warn(source, format string, args ...any) { emit(LevelWarn, source, format, args...) }

// Error logs failures — an operation didn't complete.
func Error(source, format string, args ...any) { emit(LevelError, source, format, args...) }
