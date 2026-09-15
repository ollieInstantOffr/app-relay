package balancer

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HAProxy message levels on stderr.
const (
	lvlNotice  = "NOTICE"
	lvlWarning = "WARNING"
	lvlAlert   = "ALERT"
)

// logger writes everything to one stream (stderr), like HAProxy with
// `log stderr format raw`:
//   - process messages: "[NOTICE]   (<pid>) : started hash=r1"
//   - traffic lines (httplog / tcplog) and nothing else on the line.
//
// Writes go through a bounded queue drained by a background goroutine, so
// the data path never blocks on the pipe; lines are dropped (and counted)
// when the queue is full.
type logger struct {
	sink *logSink
	pid  int
}

func newLogger(w io.Writer) *logger {
	l := &logger{pid: os.Getpid()}
	if w != nil {
		l.sink = newSink(w)
	}
	return l
}

var lineBreaks = strings.NewReplacer("\r", " ", "\n", " ")

func (l *logger) msg(level, format string, args ...any) {
	if l == nil || l.sink == nil {
		return
	}
	text := format
	if len(args) > 0 {
		text = fmt.Sprintf(format, args...)
	}
	b := getLine()
	*b = append(*b, '[')
	*b = append(*b, level...)
	*b = append(*b, ']')
	for i := len(level) + 2; i < 10; i++ {
		*b = append(*b, ' ')
	}
	*b = append(*b, " ("...)
	*b = strconv.AppendInt(*b, int64(l.pid), 10)
	*b = append(*b, ") : "...)
	*b = append(*b, lineBreaks.Replace(text)...)
	*b = append(*b, '\n')
	l.sink.write(b)
}

func (l *logger) notice(format string, args ...any)  { l.msg(lvlNotice, format, args...) }
func (l *logger) warning(format string, args ...any) { l.msg(lvlWarning, format, args...) }
func (l *logger) alert(format string, args ...any)   { l.msg(lvlAlert, format, args...) }

// sync writes a lifecycle marker and waits until it reached the stream: the
// agent waits for these lines.
func (l *logger) sync(level, format string, args ...any) {
	l.msg(level, format, args...)
	l.flush()
}

// traffic queues a raw log line (must end in '\n'); it takes ownership of b.
func (l *logger) traffic(b *[]byte) {
	if l == nil || l.sink == nil {
		putLine(b)
		return
	}
	l.sink.write(b)
}

func (l *logger) enabled() bool { return l != nil && l.sink != nil }

func (l *logger) flush() {
	if l != nil {
		l.sink.flush()
	}
}

func (l *logger) close() {
	if l != nil {
		l.sink.close()
	}
}

func (l *logger) dropped() uint64 {
	if l == nil || l.sink == nil {
		return 0
	}
	return l.sink.dropped.Load()
}

// ---------------------------------------------------------------- sink

type logSink struct {
	out     io.Writer
	queue   chan *[]byte
	flushes chan chan struct{}
	done    chan struct{}
	dropped atomic.Uint64

	mu     sync.RWMutex
	closed bool
}

const logQueueSize = 16384

var linePool = sync.Pool{New: func() any {
	b := make([]byte, 0, 512)
	return &b
}}

func getLine() *[]byte { return linePool.Get().(*[]byte) }

func putLine(b *[]byte) {
	if cap(*b) > 64<<10 {
		return
	}
	*b = (*b)[:0]
	linePool.Put(b)
}

func newSink(w io.Writer) *logSink {
	s := &logSink{out: w, queue: make(chan *[]byte, logQueueSize), flushes: make(chan chan struct{}), done: make(chan struct{})}
	go s.run()
	return s
}

func (s *logSink) write(line *[]byte) {
	if s == nil {
		putLine(line)
		return
	}
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		putLine(line)
		return
	}
	select {
	case s.queue <- line:
	default:
		s.dropped.Add(1)
		putLine(line)
	}
	s.mu.RUnlock()
}

func (s *logSink) flush() {
	if s == nil {
		return
	}
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return
	}
	ch := make(chan struct{})
	s.mu.RUnlock()
	select {
	case s.flushes <- ch:
		<-ch
	case <-s.done:
	case <-time.After(5 * time.Second):
	}
}

func (s *logSink) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.queue)
	s.mu.Unlock()
	<-s.done
}

func (s *logSink) run() {
	defer close(s.done)
	bw := bufio.NewWriterSize(s.out, 64<<10)
	drain := func() {
		for n := len(s.queue); n > 0; n-- {
			b, ok := <-s.queue
			if !ok {
				return
			}
			bw.Write(*b)
			putLine(b)
		}
	}
	for {
		select {
		case b, ok := <-s.queue:
			if !ok {
				bw.Flush()
				return
			}
			bw.Write(*b)
			putLine(b)
			drain()
			bw.Flush()
		case ch := <-s.flushes:
			drain()
			bw.Flush()
			close(ch)
		}
	}
}
