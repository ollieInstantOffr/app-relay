package engines

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scripts/version.sh derives MAJOR.MINOR.PATCH from git history.
func TestVersionScript(t *testing.T) {
	script, err := filepath.Abs("../../scripts/version.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	version := func(rev ...string) string {
		t.Helper()
		cmd := exec.Command("sh", append([]string{script}, rev...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("version.sh: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(msg ...string) {
		args := []string{"commit", "-q", "--allow-empty"}
		for _, m := range msg {
			args = append(args, "-m", m)
		}
		git(args...)
	}

	git("init", "-q", "-b", "main")
	commit("initial")
	if v := version(); v != "0.1.0" {
		t.Fatalf("first commit = %s, want 0.1.0", v)
	}
	steps := []struct {
		msg  []string
		want string
	}{
		{[]string{"fix: typo"}, "0.1.1"},
		{[]string{"Tidy up hosts page"}, "0.1.2"},
		{[]string{"feat: Resend channel"}, "0.2.0"},
		{[]string{"feat(ui): docs page"}, "0.3.0"},
		{[]string{"Small thing"}, "0.3.1"},
		{[]string{"Add upgrades [minor]"}, "0.4.0"},
		{[]string{"Explain rules", "Mentions BREAKING CHANGE / [major] / feat!: in passing"}, "0.4.1"},
		{[]string{"feat!: new config format"}, "1.0.0"},
		{[]string{"refactor", "Moves settings.\n\nBREAKING CHANGE: settings moved"}, "2.0.0"},
		{[]string{"Big rewrite [major]"}, "3.0.0"},
	}
	for _, s := range steps {
		commit(s.msg...)
		if v := version(); v != s.want {
			t.Fatalf("after %q: %s, want %s", s.msg[0], v, s.want)
		}
	}
	// Merge commits don't bump.
	git("merge", "-q", "--no-ff", "-m", "Merge branch x", "HEAD")
	if v := version(); v != "3.0.0" {
		t.Fatalf("after merge: %s", v)
	}
	// A tag sets the base; older revisions keep their number.
	git("tag", "v5.0.0")
	commit("fix: after tag")
	if v := version(); v != "5.0.1" {
		t.Fatalf("after tag: %s, want 5.0.1", v)
	}
	if v := version("v5.0.0"); v != "5.0.0" {
		t.Fatalf("at tag: %s", v)
	}
}
