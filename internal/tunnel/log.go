package tunnel

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Log levels in nginx order (lower is more severe).
type level int

const (
	levelEmerg level = iota
	levelAlert
	levelCrit
	levelError
	levelWarn
	levelNotice
	levelInfo
	levelDebug
)

var levelNames = [...]string{"emerg", "alert", "crit", "error", "warn", "notice", "info", "debug"}

func (l level) String() string { return levelNames[l] }

// logger writes nginx-style lines ("2026/09/16 14:22:07 [notice] message",
// UTC) to stderr, which the agent reads for the reload markers. Writes are
// synchronous: the engine logs lifecycle events and rate-limited problems,
// never per stream.
type logger struct {
	mu  sync.Mutex
	out io.Writer
	min level
}

func newLogger(w io.Writer) *logger {
	return &logger{out: w, min: levelNotice}
}

var lineBreaks = strings.NewReplacer("\r", " ", "\n", " ")

func (l *logger) enabled(lv level) bool { return l != nil && l.out != nil && lv <= l.min }

func (l *logger) logf(lv level, format string, args ...any) {
	if !l.enabled(lv) {
		return
	}
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	b := make([]byte, 0, 32+len(msg))
	b = time.Now().UTC().AppendFormat(b, "2006/01/02 15:04:05")
	b = append(b, " ["...)
	b = append(b, lv.String()...)
	b = append(b, "] "...)
	b = append(b, lineBreaks.Replace(msg)...)
	b = append(b, '\n')
	l.mu.Lock()
	l.out.Write(b)
	l.mu.Unlock()
}

func (l *logger) errorf(format string, args ...any)  { l.logf(levelError, format, args...) }
func (l *logger) warnf(format string, args ...any)   { l.logf(levelWarn, format, args...) }
func (l *logger) noticef(format string, args ...any) { l.logf(levelNotice, format, args...) }
func (l *logger) infof(format string, args ...any)   { l.logf(levelInfo, format, args...) }

// rateLimit lets one message through per interval and counts the rest.
type rateLimit struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int
}

// allow reports whether to log now and how many messages were suppressed
// since the last one that was logged.
func (r *rateLimit) allow(interval time.Duration) (bool, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if !r.last.IsZero() && now.Sub(r.last) < interval {
		r.suppressed++
		return false, 0
	}
	n := r.suppressed
	r.last, r.suppressed = now, 0
	return true, n
}
