package agent

import (
	"bufio"
	"io"
	"strings"
	"sync"
	"time"
)

// ringLog keeps the most recent engine output lines in memory.
type ringLog struct {
	mu    sync.Mutex
	max   int
	lines []seqLine
	seq   uint64
}

type seqLine struct {
	seq uint64
	LogLine
}

func newRingLog(max int) *ringLog { return &ringLog{max: max} }

func (r *ringLog) add(stream, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	r.lines = append(r.lines, seqLine{seq: r.seq, LogLine: LogLine{At: time.Now().UTC(), Stream: stream, Text: text}})
	if len(r.lines) > 2*r.max {
		n := copy(r.lines, r.lines[len(r.lines)-r.max:])
		r.lines = r.lines[:n]
	}
}

// mark returns the sequence number of the newest line.
func (r *ringLog) mark() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq
}

// after returns lines newer than seq.
func (r *ringLog) after(seq uint64) []LogLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []LogLine{}
	for _, l := range r.lines {
		if l.seq > seq {
			out = append(out, l.LogLine)
		}
	}
	return out
}

// since returns up to limit of the newest lines at or after t (chronological).
func (r *ringLog) since(t time.Time, limit int) []LogLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	start := len(r.lines) - r.max
	if start < 0 {
		start = 0
	}
	out := []LogLine{}
	for _, l := range r.lines[start:] {
		if !t.IsZero() && l.At.Before(t) {
			continue
		}
		out = append(out, l.LogLine)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// pump copies lines from rd into the log until EOF.
func (r *ringLog) pump(rd io.ReadCloser, stream string) {
	defer rd.Close()
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line != "" {
			r.add(stream, line)
		}
	}
	io.Copy(io.Discard, rd)
}

// joinLines renders log lines as plain text.
func joinLines(lines []LogLine) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Text)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// errorLines filters lines that look like engine errors or warnings.
func errorLines(lines []LogLine) []LogLine {
	out := []LogLine{}
	for _, l := range lines {
		t := l.Text
		if strings.Contains(t, "[emerg]") || strings.Contains(t, "[alert]") || strings.Contains(t, "[crit]") ||
			strings.Contains(t, "[error]") || strings.Contains(t, "[ALERT]") || strings.Contains(t, "[WARNING]") ||
			strings.Contains(t, "[warn]") {
			out = append(out, l)
		}
	}
	return out
}
