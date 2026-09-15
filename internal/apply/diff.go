package apply

import (
	"fmt"
	"sort"
	"strings"
)

// DiffLine is one line of a unified diff.
type DiffLine struct {
	Type  string `json:"type"` // add | del | ctx | hunk
	Text  string `json:"text"`
	OldNo int    `json:"oldNo,omitempty"`
	NewNo int    `json:"newNo,omitempty"`
}

// FileDiff is the diff of one config file.
type FileDiff struct {
	Path    string     `json:"path"`
	Status  string     `json:"status"` // added | removed | modified
	Added   int        `json:"added"`
	Removed int        `json:"removed"`
	Lines   []DiffLine `json:"lines"`
}

const diffContext = 3

// diffFileSets diffs two path → content maps. Unchanged files are omitted.
func diffFileSets(old, new map[string]string) []FileDiff {
	paths := map[string]bool{}
	for p := range old {
		paths[p] = true
	}
	for p := range new {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Slice(sorted, func(i, j int) bool { return pathRank(sorted[i]) < pathRank(sorted[j]) })
	out := []FileDiff{}
	for _, p := range sorted {
		a, inOld := old[p]
		b, inNew := new[p]
		if inOld && inNew && a == b {
			continue
		}
		fd := diffText(a, b)
		fd.Path = p
		switch {
		case !inOld:
			fd.Status = "added"
		case !inNew:
			fd.Status = "removed"
		default:
			fd.Status = "modified"
		}
		out = append(out, fd)
	}
	return out
}

// pathRank orders files: haproxy.cfg, nginx.conf / edge.json, hosts,
// redirects, streams, the rest (htpasswd/…).
func pathRank(p string) string {
	switch {
	case p == "haproxy.cfg":
		return "0"
	case p == "nginx.conf":
		return "1"
	case p == "edge/edge.json" || p == "edge.json":
		return "1" + p
	case strings.HasPrefix(p, "conf.d/hosts/"):
		return "2" + p
	case strings.HasPrefix(p, "conf.d/redirects/"):
		return "3" + p
	case strings.HasPrefix(p, "streams/"):
		return "4" + p
	case strings.HasPrefix(p, "conf.d/"):
		return "5" + p
	}
	return "6" + p
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

type edit struct {
	op   byte // '=' '-' '+'
	a, b int  // indexes into old / new
}

// diffText returns a unified diff with context hunks.
func diffText(oldText, newText string) FileDiff {
	a, b := splitLines(oldText), splitLines(newText)
	edits := myers(a, b)
	fd := FileDiff{Lines: []DiffLine{}}
	for _, e := range edits {
		switch e.op {
		case '+':
			fd.Added++
		case '-':
			fd.Removed++
		}
	}
	fd.Lines = hunks(edits, a, b, diffContext)
	return fd
}

// myers computes a shortest edit script (Myers O(ND)) after trimming the
// common prefix and suffix. Very large differences degrade to replace-all.
func myers(a, b []string) []edit {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	out := make([]edit, 0, len(a)+len(b))
	for i := 0; i < pre; i++ {
		out = append(out, edit{'=', i, i})
	}
	A, B := a[pre:len(a)-suf], b[pre:len(b)-suf]
	for _, e := range myersCore(A, B) {
		out = append(out, edit{e.op, e.a + pre, e.b + pre})
	}
	for i := 0; i < suf; i++ {
		out = append(out, edit{'=', len(a) - suf + i, len(b) - suf + i})
	}
	return out
}

const maxEditDistance = 2000

func myersCore(a, b []string) []edit {
	n, m := len(a), len(b)
	replaceAll := func() []edit {
		out := make([]edit, 0, n+m)
		for i := 0; i < n; i++ {
			out = append(out, edit{'-', i, 0})
		}
		for j := 0; j < m; j++ {
			out = append(out, edit{'+', n, j})
		}
		return out
	}
	if n == 0 || m == 0 {
		return replaceAll()
	}
	max := n + m
	if max > 2*maxEditDistance+2 {
		max = 2*maxEditDistance + 2
	}
	offset := max + 1
	v := make([]int, 2*offset+1)
	var trace [][]int
	for d := 0; d <= max; d++ {
		snap := make([]int, 2*d+3)
		copy(snap, v[offset-d-1:offset+d+2])
		trace = append(trace, snap)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				return backtrack(trace, n, m)
			}
		}
		if d >= maxEditDistance {
			break
		}
	}
	return replaceAll()
}

func backtrack(trace [][]int, n, m int) []edit {
	get := func(snap []int, d, k int) int {
		i := k + d + 1
		if i < 0 || i >= len(snap) {
			return 0
		}
		return snap[i]
	}
	var rev []edit
	x, y := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		snap := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && get(snap, d, k-1) < get(snap, d, k+1)) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := get(snap, d, prevK)
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			rev = append(rev, edit{'=', x - 1, y - 1})
			x--
			y--
		}
		if d > 0 {
			if x == prevX {
				rev = append(rev, edit{'+', x, y - 1})
			} else {
				rev = append(rev, edit{'-', x - 1, y})
			}
		}
		x, y = prevX, prevY
	}
	out := make([]edit, len(rev))
	for i := range rev {
		out[i] = rev[len(rev)-1-i]
	}
	return out
}

// hunks renders edits as unified-diff hunks with ctx lines of context.
func hunks(edits []edit, a, b []string, ctx int) []DiffLine {
	out := []DiffLine{}
	i := 0
	for i < len(edits) {
		// find next change
		for i < len(edits) && edits[i].op == '=' {
			i++
		}
		if i >= len(edits) {
			break
		}
		start := i - ctx
		if start < 0 {
			start = 0
		}
		// extend while changes are within 2*ctx of each other
		end := i
		for end < len(edits) {
			if edits[end].op != '=' {
				end++
				continue
			}
			run := end
			for run < len(edits) && edits[run].op == '=' {
				run++
			}
			if run >= len(edits) || run-end > 2*ctx {
				end += min(ctx, run-end)
				break
			}
			end = run
		}
		if end > len(edits) {
			end = len(edits)
		}
		oldStart, newStart, oldLen, newLen := 0, 0, 0, 0
		first := true
		body := []DiffLine{}
		for _, e := range edits[start:end] {
			switch e.op {
			case '=':
				if first {
					oldStart, newStart, first = e.a+1, e.b+1, false
				}
				oldLen++
				newLen++
				body = append(body, DiffLine{Type: "ctx", Text: a[e.a], OldNo: e.a + 1, NewNo: e.b + 1})
			case '-':
				if first {
					oldStart, newStart, first = e.a+1, e.b+1, false
				}
				oldLen++
				body = append(body, DiffLine{Type: "del", Text: a[e.a], OldNo: e.a + 1})
			case '+':
				if first {
					oldStart, newStart, first = e.a+1, e.b+1, false
				}
				newLen++
				body = append(body, DiffLine{Type: "add", Text: b[e.b], NewNo: e.b + 1})
			}
		}
		if oldLen == 0 {
			oldStart--
		}
		if newLen == 0 {
			newStart--
		}
		out = append(out, DiffLine{Type: "hunk", Text: fmt.Sprintf("@@ -%d,%d +%d,%d @@", oldStart, oldLen, newStart, newLen)})
		out = append(out, body...)
		i = end
	}
	return out
}
