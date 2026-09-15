package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHashFilesStable(t *testing.T) {
	a := Files{"nginx.conf": "x", "conf.d/a.conf": "y"}
	b := Files{"conf.d/a.conf": "y", "nginx.conf": "x"}
	if HashFiles(a) != HashFiles(b) {
		t.Fatal("hash depends on map order")
	}
	if HashFiles(a) == HashFiles(Files{"nginx.conf": "xconf.d/a.conf", "": "y"}) {
		t.Fatal("hash must separate path and content")
	}
	if !validHash(HashFiles(a)) {
		t.Fatal("hash must be valid")
	}
}

func TestSafeRelPath(t *testing.T) {
	for p, want := range map[string]bool{
		"nginx.conf": true, "conf.d/hosts/a.conf": true, "htpasswd/abc": true,
		"": false, "/etc/passwd": false, "../x": false, "a/../../x": false, "a//b": false, ".hidden": false, "a/./b": false,
	} {
		if got := safeRelPath(p); got != want {
			t.Errorf("safeRelPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestReleasesActivateRollbackPrune(t *testing.T) {
	root := t.TempDir()
	r := &releases{root: root, keep: 3}
	if err := r.init(); err != nil {
		t.Fatal(err)
	}
	if r.current() != "" {
		t.Fatal("expected no current release")
	}
	var hashes []string
	for i := 0; i < 6; i++ {
		files := Files{"nginx.conf": strings.Repeat("line\n", i+1), "conf.d/x.conf": "server {}\n"}
		h := HashFiles(files)
		if _, _, err := r.write(h, files); err != nil {
			t.Fatal(err)
		}
		if err := r.activate(h); err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, h)
		if r.current() != h {
			t.Fatalf("current = %s, want %s", r.current(), h)
		}
	}
	// current is a relative symlink into releases/.
	target, err := os.Readlink(filepath.Join(root, "current"))
	if err != nil || target != filepath.Join("releases", hashes[5]) {
		t.Fatalf("symlink target %q err %v", target, err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "current", "conf.d", "x.conf")); err != nil || string(b) != "server {}\n" {
		t.Fatalf("read through symlink: %q %v", b, err)
	}
	if got := r.lines(hashes[5]); got != 7 {
		t.Fatalf("lines = %d, want 7", got)
	}
	if r.previous() != hashes[4] {
		t.Fatalf("previous = %s, want %s", r.previous(), hashes[4])
	}
	prev, err := r.rollback()
	if err != nil || prev != hashes[4] || r.current() != hashes[4] {
		t.Fatalf("rollback -> %s (%v), current %s", prev, err, r.current())
	}
	if r.previous() != hashes[3] {
		t.Fatalf("previous after rollback = %s, want %s", r.previous(), hashes[3])
	}

	r.prune()
	entries, _ := os.ReadDir(filepath.Join(root, "releases"))
	if len(entries) > r.keep+1 {
		t.Fatalf("prune kept %d releases", len(entries))
	}
	if _, err := os.Stat(r.dir(r.current())); err != nil {
		t.Fatal("prune removed the current release")
	}

	// revert after a failed activation restores prev and drops the entry.
	files := Files{"nginx.conf": "bad\n"}
	h := HashFiles(files)
	r.write(h, files)
	r.activate(h)
	cur := hashes[4]
	if err := r.revert(h, cur); err != nil {
		t.Fatal(err)
	}
	if r.current() != cur {
		t.Fatalf("revert: current %s, want %s", r.current(), cur)
	}
	if hist := r.history(); hist[len(hist)-1] != cur {
		t.Fatalf("history tail %v", hist)
	}
}

func TestReleasesRejectUnsafePaths(t *testing.T) {
	r := &releases{root: t.TempDir(), keep: 3}
	r.init()
	if _, _, err := r.write("deadbeef00", Files{"../escape": "x"}); err == nil {
		t.Fatal("expected error for unsafe path")
	}
}

const procTCP = `  sl  local_address                         remote_address                        st   tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0050 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1001 0
   1: 0100007F:1FA0 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1002 0
   2: 0100007F:1FA0 0200007F:C350 01 00000000:00000000 00:00000000 00000000     0        0 1003 0
`

const procTCP6 = `  sl  local_address                         remote_address                        st   tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:01BB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2001 0
   1: 00000000000000000000000001000000:0050 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2002 0
`

const procUDP = `  sl  local_address                         remote_address                        st   tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode ref pointer drops
  1: 00000000:01BB 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 3001 2 0000000000000000 0
  2: 0100000A:D431 08080808:0035 07 00000000:00000000 00:00000000 00000000     0        0 3002 2 0000000000000000 0
`

func TestParseProcNet(t *testing.T) {
	tcp := parseProcNet(strings.NewReader(procTCP), "tcp", false)
	if len(tcp) != 2 || tcp[0].addr != "0.0.0.0" || tcp[0].port != 80 || tcp[1].addr != "127.0.0.1" || tcp[1].port != 8096 || tcp[1].inode != "1002" {
		t.Fatalf("tcp = %+v", tcp)
	}
	tcp6 := parseProcNet(strings.NewReader(procTCP6), "tcp", true)
	if len(tcp6) != 2 || tcp6[0].addr != "::" || tcp6[0].port != 443 || tcp6[1].addr != "::1" || tcp6[1].port != 80 {
		t.Fatalf("tcp6 = %+v", tcp6)
	}
	udp := parseProcNet(strings.NewReader(procUDP), "udp", false)
	if len(udp) != 1 || udp[0].port != 443 {
		t.Fatalf("udp = %+v", udp)
	}
}

func TestListListenersFromFakeProc(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "net"), 0o755)
	os.WriteFile(filepath.Join(root, "net", "tcp"), []byte(procTCP), 0o644)
	os.WriteFile(filepath.Join(root, "net", "udp"), []byte(procUDP), 0o644)
	ls := listListeners(root)
	if len(ls) != 3 {
		t.Fatalf("listeners = %+v", ls)
	}
	if ls[0].Port != 80 || ls[1].Port != 443 || ls[1].Proto != "udp" || ls[2].Port != 8096 {
		t.Fatalf("order = %+v", ls)
	}
}

func TestRingLog(t *testing.T) {
	r := newRingLog(3)
	for i := 0; i < 10; i++ {
		r.add("stderr", strings.Repeat("x", i+1))
	}
	got := r.since(time.Time{}, 0)
	if len(got) != 3 || got[2].Text != "xxxxxxxxxx" {
		t.Fatalf("since = %+v", got)
	}
	m := r.mark()
	r.add("stderr", "nginx: [emerg] bind() to 0.0.0.0:443 failed (98: Address already in use)")
	r.add("stdout", "noise")
	after := r.after(m)
	if len(after) != 2 {
		t.Fatalf("after = %+v", after)
	}
	if e := errorLines(after); len(e) != 1 || !strings.Contains(e[0].Text, "bind()") {
		t.Fatalf("errorLines = %+v", e)
	}
}

func TestReaperRun(t *testing.T) {
	r := newReaper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.loop(ctx)
	out, err := r.run(5*time.Second, "sh", "-c", "echo hello; echo oops >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, "oops") {
		t.Fatalf("out = %q", out)
	}
	if _, err := r.run(200*time.Millisecond, "sleep", "5"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout err = %v", err)
	}
}
