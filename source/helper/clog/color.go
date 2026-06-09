package clog

// Terminal colouring for the console sink ONLY. The log file and the UI
// tap must stay plain — ANSI escapes would corrupt `cat`/grep on the file
// and show up as garbage in the web log stream. So colour is not baked
// into the formatted line (which fans out to every sink); instead it's
// applied by a writer that wraps just os.Stderr in the MultiWriter.
//
// Each line is "<time> LEVEL component message"; we tint the LEVEL word
// by severity and leave the rest untouched, so subsystem and message stay
// readable. Disabled automatically when stderr isn't a terminal (piped to
// a file or another process) and when NO_COLOR is set (https://no-color.org).
import (
	"io"
	"os"
	"regexp"
)

const (
	cReset  = "\x1b[0m"
	cGray   = "\x1b[90m"
	cGreen  = "\x1b[32m"
	cYellow = "\x1b[33m"
	cRed    = "\x1b[1;31m"
)

// levelColor maps each severity word to its ANSI colour.
var levelColor = map[string]string{
	"DEBUG": cGray,
	"INFO":  cGreen,
	"WARN":  cYellow,
	"ERROR": cRed,
}

// levelWord matches the first severity token on a line (the level column
// sits before the message, so the first match is always the real level).
var levelWord = regexp.MustCompile(`\b(DEBUG|INFO|WARN|ERROR)\b`)

// colorWriter wraps the real stderr and tints the level word on its way out.
type colorWriter struct{ w io.Writer }

func (c colorWriter) Write(p []byte) (int, error) {
	out := levelWord.ReplaceAllFunc(p, func(m []byte) []byte {
		return append(append([]byte(levelColor[string(m)]), m...), cReset...)
	})
	// Report the original length so the log package sees a complete write.
	if _, err := c.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// consoleSink returns the writer to use for the terminal mirror: a
// colourising wrapper when stderr is an interactive terminal and colour
// isn't suppressed, else plain stderr.
func consoleSink() io.Writer {
	if colorEnabled() {
		return colorWriter{w: os.Stderr}
	}
	return os.Stderr
}

// colorEnabled reports whether to emit ANSI colour on the console.
// F2F_COLOR forces it (=1) or disables it (=0); otherwise colour is on
// only for an interactive stderr with NO_COLOR unset.
func colorEnabled() bool {
	switch os.Getenv("F2F_COLOR") {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	}
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
