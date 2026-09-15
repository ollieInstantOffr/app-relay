package edge

import (
	"fmt"
	"io"
	"path/filepath"
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

// errorLog writes nginx-style lines ("2026/09/14 14:22:07 [error] message",
// UTC) to stderr, which the agent reads for the reload markers, and to
// <LogDir>/error.log.
type errorLog struct {
	min    level
	stderr *logSink

	mu   sync.RWMutex
	dir  string
	file *logSink
}

func newErrorLog(stderr io.Writer) *errorLog {
	e := &errorLog{min: levelNotice}
	if stderr != nil {
		e.stderr = newWriterSink(stderr)
	}
	return e
}

// setDir switches error.log to dir ("" = stderr only).
func (e *errorLog) setDir(dir string) {
	e.mu.Lock()
	if dir == e.dir {
		e.mu.Unlock()
		return
	}
	old := e.file
	e.dir, e.file = dir, nil
	if dir != "" {
		e.file = newFileSink(filepath.Join(dir, "error.log"))
	}
	e.mu.Unlock()
	old.close()
}

func (e *errorLog) enabled(l level) bool { return e != nil && l <= e.min }

func (e *errorLog) logf(l level, format string, args ...any) {
	if !e.enabled(l) {
		return
	}
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	e.write(l, msg)
}

func (e *errorLog) write(l level, msg string) {
	msg = strings.NewReplacer("\r", " ", "\n", " ").Replace(msg)
	e.mu.RLock()
	file := e.file
	for _, sink := range [2]*logSink{e.stderr, file} {
		if sink == nil {
			continue
		}
		b := getLine()
		*b = time.Now().UTC().AppendFormat(*b, "2006/01/02 15:04:05")
		*b = append(*b, " ["...)
		*b = append(*b, l.String()...)
		*b = append(*b, "] "...)
		*b = append(*b, msg...)
		*b = append(*b, '\n')
		sink.write(b)
	}
	e.mu.RUnlock()
}

// sync writes a lifecycle line and waits until it reached stderr and the file:
// the agent waits for these markers.
func (e *errorLog) sync(l level, format string, args ...any) {
	e.logf(l, format, args...)
	e.flush()
}

func (e *errorLog) flush() {
	e.mu.RLock()
	file := e.file
	e.mu.RUnlock()
	file.flush() // error.log first: the agent reacts as soon as stderr shows a marker
	e.stderr.flush()
}

func (e *errorLog) close() {
	e.mu.Lock()
	file := e.file
	e.file, e.dir = nil, ""
	e.mu.Unlock()
	file.close()
	e.stderr.close()
}

func (e *errorLog) dropped() uint64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var n uint64
	if e.stderr != nil {
		n += e.stderr.dropped.Load()
	}
	if e.file != nil {
		n += e.file.dropped.Load()
	}
	return n
}

// stdlibWriter adapts the error log for log.Logger users (http.Server,
// ReverseProxy). Their messages (TLS handshake errors, client resets) are
// routine noise, logged at info like nginx does.
type stdlibWriter struct{ e *errorLog }

func (w stdlibWriter) Write(p []byte) (int, error) {
	if w.e.enabled(levelInfo) {
		w.e.write(levelInfo, strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
}
