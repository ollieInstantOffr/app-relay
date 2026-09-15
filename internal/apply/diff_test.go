package apply

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// apply edits to a and check we get b.
func replay(t *testing.T, a, b []string, edits []edit) {
	t.Helper()
	var got []string
	for _, e := range edits {
		switch e.op {
		case '=':
			if a[e.a] != b[e.b] {
				t.Fatalf("equal edit mismatch %q vs %q", a[e.a], b[e.b])
			}
			got = append(got, b[e.b])
		case '+':
			got = append(got, b[e.b])
		}
	}
	if strings.Join(got, "\n") != strings.Join(b, "\n") {
		t.Fatalf("replay mismatch:\n%v\nwant\n%v", got, b)
	}
	// deletions + equals must cover a in order
	idx := 0
	for _, e := range edits {
		if e.op == '=' || e.op == '-' {
			if e.a != idx {
				t.Fatalf("old index out of order: %d want %d", e.a, idx)
			}
			idx++
		}
	}
	if idx != len(a) {
		t.Fatalf("covered %d of %d old lines", idx, len(a))
	}
}

func TestMyersSmall(t *testing.T) {
	cases := []struct{ a, b string }{
		{"", ""},
		{"a", ""},
		{"", "a"},
		{"a\nb\nc", "a\nb\nc"},
		{"a\nb\nc", "a\nx\nc"},
		{"a\nb\nc\nd", "b\nc\nd\ne"},
		{"x\ny", "a\nb\nc"},
		{"a\nb\na\nb\na", "b\na\nb\na\nb"},
	}
	for _, c := range cases {
		a, b := splitLines(c.a), splitLines(c.b)
		replay(t, a, b, myers(a, b))
	}
}

func TestMyersRandomMinimal(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	alphabet := []string{"a", "b", "c", "d"}
	for iter := 0; iter < 300; iter++ {
		a := make([]string, r.Intn(12))
		b := make([]string, r.Intn(12))
		for i := range a {
			a[i] = alphabet[r.Intn(len(alphabet))]
		}
		for i := range b {
			b[i] = alphabet[r.Intn(len(alphabet))]
		}
		edits := myers(a, b)
		replay(t, a, b, edits)
		changes := 0
		for _, e := range edits {
			if e.op != '=' {
				changes++
			}
		}
		if want := len(a) + len(b) - 2*lcs(a, b); changes != want {
			t.Fatalf("not minimal: %d changes, want %d (%v → %v)", changes, want, a, b)
		}
	}
}

func lcs(a, b []string) int {
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else {
				dp[i][j] = max(dp[i-1][j], dp[i][j-1])
			}
		}
	}
	return dp[len(a)][len(b)]
}

func TestDiffTextHunks(t *testing.T) {
	var old, nw []string
	for i := 1; i <= 40; i++ {
		old = append(old, fmt.Sprintf("line %d", i))
	}
	nw = append(nw, old...)
	nw[4] = "line 5 changed"                                          // near top
	nw = append(nw[:30], append([]string{"inserted"}, nw[30:]...)...) // far away → second hunk
	fd := diffText(strings.Join(old, "\n")+"\n", strings.Join(nw, "\n")+"\n")
	if fd.Added != 2 || fd.Removed != 1 {
		t.Fatalf("added %d removed %d", fd.Added, fd.Removed)
	}
	var hunks []string
	for _, l := range fd.Lines {
		if l.Type == "hunk" {
			hunks = append(hunks, l.Text)
		}
	}
	if len(hunks) != 2 || hunks[0] != "@@ -2,7 +2,7 @@" || hunks[1] != "@@ -28,6 +28,7 @@" {
		t.Fatalf("hunks = %v", hunks)
	}
	for _, l := range fd.Lines {
		switch l.Type {
		case "del":
			if l.OldNo != 5 || l.Text != "line 5" {
				t.Errorf("del = %+v", l)
			}
		case "add":
			if !(l.NewNo == 5 && l.Text == "line 5 changed") && !(l.NewNo == 31 && l.Text == "inserted") {
				t.Errorf("add = %+v", l)
			}
		}
	}
}

func TestDiffFileSets(t *testing.T) {
	old := map[string]string{"nginx.conf": "a\nb\n", "conf.d/hosts/x.conf": "server {}\n", "same.conf": "z\n"}
	nw := map[string]string{"nginx.conf": "a\nc\n", "conf.d/hosts/y.conf": "server {}\n", "same.conf": "z\n", "haproxy.cfg": "global\n"}
	fds := diffFileSets(old, nw)
	var paths []string
	for _, f := range fds {
		paths = append(paths, f.Path+":"+f.Status)
	}
	if got := strings.Join(paths, ","); got != "haproxy.cfg:added,nginx.conf:modified,conf.d/hosts/x.conf:removed,conf.d/hosts/y.conf:added" {
		t.Fatalf("paths = %s", got)
	}
}

func TestDiffLargeReplaceFallsBack(t *testing.T) {
	var a, b []string
	for i := 0; i < 5000; i++ {
		a = append(a, fmt.Sprintf("a%d", i))
		b = append(b, fmt.Sprintf("b%d", i))
	}
	edits := myers(a, b)
	replay(t, a, b, edits)
}
