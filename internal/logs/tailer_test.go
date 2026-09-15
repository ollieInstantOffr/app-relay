package logs

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type collector struct {
	lines []string
	state tailState
	emits int
}

func (c *collector) emit(lines [][]byte, st tailState) {
	for _, l := range lines {
		c.lines = append(c.lines, string(l))
	}
	c.state = st
	c.emits++
}

func (c *collector) take() []string {
	out := c.lines
	c.lines = nil
	return out
}

func appendFile(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(data); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func poll(t *testing.T, tl *tailer) {
	t.Helper()
	if err := tl.poll(); err != nil {
		t.Fatal(err)
	}
}

func expect(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestTailerStartsAtEndOnFirstRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	appendFile(t, path, "old-1\nold-2\n")
	c := &collector{}
	tl := newTailer(path, nil, c.emit)
	defer tl.close()

	poll(t, tl)
	expect(t, c.take())
	appendFile(t, path, "new-1\nnew-2\n")
	poll(t, tl)
	expect(t, c.take(), "new-1", "new-2")
	fi, _ := os.Stat(path)
	if c.state.Offset != fi.Size() || c.state.Inode != fileInode(fi) {
		t.Fatalf("state %+v, size %d", c.state, fi.Size())
	}
}

func TestTailerPartialLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	appendFile(t, path, "")
	c := &collector{}
	tl := newTailer(path, nil, c.emit)
	defer tl.close()
	poll(t, tl)

	appendFile(t, path, `{"a":`)
	poll(t, tl)
	expect(t, c.take())
	if c.state.Offset != 0 {
		t.Fatalf("offset must not include the partial line: %+v", c.state)
	}
	appendFile(t, path, "1}\r\n\n{\"b\":2}\n")
	poll(t, tl)
	expect(t, c.take(), `{"a":1}`, `{"b":2}`)
}

func TestTailerRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	appendFile(t, path, "")
	c := &collector{}
	tl := newTailer(path, nil, c.emit)
	defer tl.close()
	poll(t, tl)

	appendFile(t, path, "a\n")
	poll(t, tl)
	expect(t, c.take(), "a")

	// logrotate: rename, the writer still appends to the old file, then reopens.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path+".1", "b\n")
	poll(t, tl) // file missing: drains the old descriptor
	expect(t, c.take(), "b")
	appendFile(t, path+".1", "c\n")
	appendFile(t, path, "d\n")
	poll(t, tl)
	expect(t, c.take(), "c", "d")
}

func TestTailerTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	appendFile(t, path, "")
	c := &collector{}
	tl := newTailer(path, nil, c.emit)
	defer tl.close()
	poll(t, tl)
	appendFile(t, path, "line-1\nline-2\n")
	poll(t, tl)
	expect(t, c.take(), "line-1", "line-2")

	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, "x\n")
	poll(t, tl)
	expect(t, c.take(), "x")
}

func TestTailerResumeFromSavedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	appendFile(t, path, "l1\n")
	fi, _ := os.Stat(path)
	saved := tailState{Inode: fileInode(fi), Offset: fi.Size()}
	appendFile(t, path, "l2\nl3\n")

	c := &collector{}
	tl := newTailer(path, &saved, c.emit)
	poll(t, tl)
	tl.close()
	expect(t, c.take(), "l2", "l3")

	// A saved state for another inode (rotated while stopped) restarts at 0.
	other := tailState{Inode: saved.Inode + 12345, Offset: 3}
	c = &collector{}
	tl = newTailer(path, &other, c.emit)
	poll(t, tl)
	tl.close()
	expect(t, c.take(), "l1", "l2", "l3")
}

func TestTailerFileCreatedLater(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stream-access.log")
	c := &collector{}
	tl := newTailer(path, nil, c.emit)
	defer tl.close()
	poll(t, tl)
	expect(t, c.take())
	appendFile(t, path, "first\nsecond\n")
	poll(t, tl)
	expect(t, c.take(), "first", "second")
}

func TestTailerSkipsOversizedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	appendFile(t, path, "")
	c := &collector{}
	tl := newTailer(path, nil, c.emit)
	defer tl.close()
	poll(t, tl)
	big := make([]byte, maxLineBytes+10)
	for i := range big {
		big[i] = 'x'
	}
	appendFile(t, path, string(big)+"\nok\n")
	poll(t, tl)
	expect(t, c.take(), "ok")
}
