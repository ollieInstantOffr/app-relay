package logs

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
)

// tailState is persisted (kv) after the lines up to Offset were committed.
type tailState struct {
	Inode  uint64 `json:"inode"`
	Offset int64  `json:"offset"`
}

const (
	maxLineBytes  = 1 << 20 // longer lines are skipped
	readChunk     = 64 << 10
	maxBatchLines = 500
)

// tailer follows one log file by polling. It survives the file not existing
// yet, rotation (rename/re-create → inode change) and truncation
// (copytruncate → size shrinks), and never emits partial lines.
//
// On the very first run (no saved state) it starts at the end of an existing
// file; a file that appears later is read from the beginning; a saved state
// resumes at the saved offset when the inode still matches.
type tailer struct {
	path  string
	emit  func(lines [][]byte, st tailState)
	saved *tailState
	first bool

	f        *os.File
	inode    uint64
	readPos  int64
	partial  []byte
	skipping bool
	buf      []byte
}

func newTailer(path string, saved *tailState, emit func([][]byte, tailState)) *tailer {
	return &tailer{path: path, saved: saved, first: saved == nil, emit: emit, buf: make([]byte, readChunk)}
}

func (t *tailer) close() {
	if t.f != nil {
		t.f.Close()
		t.f = nil
	}
}

func (t *tailer) state() tailState {
	return tailState{Inode: t.inode, Offset: t.readPos - int64(len(t.partial))}
}

// poll reads everything new since the last call.
func (t *tailer) poll() error {
	fi, err := os.Stat(t.path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		// Moved away and not re-created yet: keep draining the old descriptor.
		t.first = false
		if t.f != nil {
			return t.read()
		}
		return nil
	}
	ino := fileInode(fi)
	switch {
	case t.f == nil:
		if err := t.open(fi, ino); err != nil {
			return err
		}
	case ino != t.inode:
		// Rotated: finish the old file, then follow the new one from the start.
		if err := t.read(); err != nil {
			return err
		}
		t.close()
		t.partial, t.skipping = nil, false
		t.saved, t.first = nil, false
		if err := t.open(fi, ino); err != nil {
			return err
		}
	case fi.Size() < t.readPos:
		// Truncated in place.
		if _, err := t.f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		t.readPos = 0
		t.partial, t.skipping = nil, false
	}
	return t.read()
}

func (t *tailer) open(fi os.FileInfo, ino uint64) error {
	f, err := os.Open(t.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var start int64
	switch {
	case t.saved != nil && t.saved.Inode == ino && t.saved.Offset <= fi.Size():
		start = t.saved.Offset
	case t.saved != nil:
		start = 0 // rotated or truncated while we were not running
	case t.first:
		start = fi.Size()
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		f.Close()
		return err
	}
	t.f, t.inode, t.readPos = f, ino, start
	t.saved, t.first = nil, false
	t.emit(nil, t.state()) // persist the starting position
	return nil
}

func (t *tailer) read() error {
	var lines [][]byte
	for {
		n, err := t.f.Read(t.buf)
		if n > 0 {
			t.readPos += int64(n)
			data := t.buf[:n]
			for len(data) > 0 {
				i := bytes.IndexByte(data, '\n')
				if i < 0 {
					if t.skipping {
						break
					}
					if len(t.partial)+len(data) > maxLineBytes {
						t.partial, t.skipping = nil, true
						break
					}
					t.partial = append(t.partial, data...)
					break
				}
				seg := data[:i]
				data = data[i+1:]
				if t.skipping {
					t.skipping = false
					continue
				}
				var line []byte
				if len(t.partial) > 0 {
					if len(t.partial)+len(seg) <= maxLineBytes {
						line = append(t.partial, seg...)
					}
					t.partial = nil
				} else {
					line = append([]byte(nil), seg...)
				}
				line = bytes.TrimRight(line, "\r")
				if len(bytes.TrimSpace(line)) > 0 {
					lines = append(lines, line)
				}
			}
			if len(lines) >= maxBatchLines {
				t.emit(lines, t.state())
				lines = nil
			}
		}
		if err == io.EOF || (err == nil && n == 0) {
			break
		}
		if err != nil {
			return err
		}
	}
	if len(lines) > 0 {
		t.emit(lines, t.state())
	}
	return nil
}
