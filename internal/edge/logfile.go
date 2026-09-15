package edge

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// logSink writes lines from a bounded queue on a background goroutine, so
// request handling never blocks on disk or pipe I/O. When the queue is full
// lines are dropped and counted. File sinks reopen their path when the file
// was renamed or deleted, because Relay tails and rotates the logs.
type logSink struct {
	path string    // "" for a fixed writer
	out  io.Writer // fixed writer (stderr)

	queue   chan *[]byte
	flushes chan chan struct{}
	done    chan struct{}
	dropped atomic.Uint64

	mu     sync.RWMutex
	closed bool
}

const logQueueSize = 16384

var linePool = sync.Pool{New: func() any {
	b := make([]byte, 0, 1024)
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

func newFileSink(path string) *logSink {
	s := &logSink{path: path}
	s.start()
	return s
}

func newWriterSink(w io.Writer) *logSink {
	s := &logSink{out: w}
	s.start()
	return s
}

func (s *logSink) start() {
	s.queue = make(chan *[]byte, logQueueSize)
	s.flushes = make(chan chan struct{})
	s.done = make(chan struct{})
	go s.run()
}

// write queues a line (ending in '\n') and takes ownership of the buffer.
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

// flush waits until everything queued so far has been written.
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
	var (
		f  *os.File
		bw *bufio.Writer
	)
	if s.out != nil {
		bw = bufio.NewWriterSize(s.out, 64<<10)
	}
	open := func() {
		if s.path == "" {
			return
		}
		if f != nil {
			bw.Flush()
			f.Close()
			f, bw = nil, nil
		}
		os.MkdirAll(filepath.Dir(s.path), 0o755)
		nf, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err != nil {
			return
		}
		f, bw = nf, bufio.NewWriterSize(nf, 64<<10)
	}
	open()
	writeLine := func(b *[]byte) {
		if bw == nil {
			s.dropped.Add(1)
		} else {
			bw.Write(*b)
		}
		putLine(b)
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case b, ok := <-s.queue:
			if !ok {
				if bw != nil {
					bw.Flush()
				}
				if f != nil {
					f.Close()
				}
				return
			}
			writeLine(b)
			// Drain whatever is already queued, then flush once: tailers see
			// lines promptly without a syscall per line under load.
			for n := len(s.queue); n > 0; n-- {
				b, ok := <-s.queue
				if !ok {
					break
				}
				writeLine(b)
			}
			if bw != nil {
				bw.Flush()
			}
		case ch := <-s.flushes:
			for n := len(s.queue); n > 0; n-- {
				b, ok := <-s.queue
				if !ok {
					break
				}
				writeLine(b)
			}
			if bw != nil {
				bw.Flush()
			}
			close(ch)
		case <-ticker.C:
			if s.path != "" && (f == nil || rotated(f, s.path)) {
				open()
			}
		}
	}
}

// rotated reports whether path no longer refers to the open file.
func rotated(f *os.File, path string) bool {
	cur, err := f.Stat()
	if err != nil {
		return true
	}
	onDisk, err := os.Stat(path)
	if err != nil {
		return true
	}
	return !os.SameFile(cur, onDisk)
}
