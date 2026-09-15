package acme

import (
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
)

func TestParseHtpasswd(t *testing.T) {
	h, _ := bcrypt.GenerateFromPassword([]byte("secret-pass"), bcrypt.MinCost)
	y := "$2y$" + string(h)[4:]
	users, err := parseHtpasswd("# comment\n\njonas:" + y + "\nmira:" + string(h) + "\njonas:" + string(h) + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0].Username != "jonas" || users[0].PasswordHash != string(h) {
		t.Errorf("users = %+v", users)
	}
	if bcrypt.CompareHashAndPassword([]byte(y), []byte("secret-pass")) != nil {
		t.Error("$2y$ hashes must verify with Go bcrypt")
	}

	_, err = parseHtpasswd("grandma:$apr1$abc$def\nbob:{SHA}W6ph5Mm5Pz8GgiULbPgzG37mj9g=\nplain:hunter2\n")
	var he *httpx.HTTPError
	if !errors.As(err, &he) || he.Status != 422 {
		t.Fatalf("expected 422, got %v", err)
	}
	for _, want := range []string{"line 1 (grandma): APR1-MD5", "line 2 (bob): SHA-1", "line 3 (plain): plain-text", "htpasswd -B"} {
		if !strings.Contains(he.Message, want) {
			t.Errorf("message %q missing %q", he.Message, want)
		}
	}
	if _, err := parseHtpasswd("\n# nothing\n"); err == nil {
		t.Error("empty import accepted")
	}
}

func TestEvaluateAccess(t *testing.T) {
	lan := &model.AccessList{Rules: []model.IPRule{
		{ID: "1", Action: "allow", CIDR: "192.168.0.0/16"},
		{ID: "2", Action: "allow", CIDR: "10.0.0.0/8"},
		{ID: "3", Action: "deny", CIDR: "all"},
	}}
	ip := netip.MustParseAddr
	d := evaluateAccess(lan, ip("192.168.1.24"))
	if !d.Allowed || d.RequiresAuth || d.MatchedRule.ID != "1" {
		t.Errorf("lan allow: %+v", d)
	}
	d = evaluateAccess(lan, ip("185.220.101.4"))
	if d.Allowed || d.RequiresAuth || d.MatchedRule.ID != "3" {
		t.Errorf("deny all: %+v", d)
	}
	d = evaluateAccess(lan, ip("::ffff:10.1.2.3"))
	if !d.Allowed || d.MatchedRule.ID != "2" {
		t.Errorf("v4-mapped: %+v", d)
	}

	family := &model.AccessList{
		Rules:     []model.IPRule{{ID: "1", Action: "allow", CIDR: "192.168.0.0/16"}, {ID: "2", Action: "deny", CIDR: "all"}},
		BasicAuth: model.BasicAuth{Enabled: true, Users: []model.BasicAuthUser{{Username: "jonas", PasswordHash: "x"}}},
	}
	d = evaluateAccess(family, ip("192.168.1.2"))
	if d.Allowed || !d.RequiresAuth {
		t.Errorf("satisfy all, allowed IP asks for password: %+v", d)
	}
	d = evaluateAccess(family, ip("8.8.8.8"))
	if d.Allowed || d.RequiresAuth {
		t.Errorf("satisfy all, denied IP: %+v", d)
	}
	family.SatisfyAny = true
	d = evaluateAccess(family, ip("192.168.1.2"))
	if !d.Allowed || d.RequiresAuth {
		t.Errorf("satisfy any, allowed IP: %+v", d)
	}
	d = evaluateAccess(family, ip("8.8.8.8"))
	if d.Allowed || !d.RequiresAuth {
		t.Errorf("satisfy any, denied IP passes with password: %+v", d)
	}

	empty := &model.AccessList{}
	if d := evaluateAccess(empty, ip("1.2.3.4")); !d.Allowed || d.MatchedRule != nil {
		t.Errorf("no rules: %+v", d)
	}
}

func TestPortConflicts(t *testing.T) {
	entries := []PortEntry{
		{Port: 80, Proto: "tcp", Address: "0.0.0.0", Owner: "nginx", Kind: "http", Name: "HTTP"},
		{Port: 443, Proto: "tcp", Address: "0.0.0.0", Owner: "nginx", Kind: "https", Name: "HTTPS"},
		{Port: 10080, Proto: "tcp", Address: "127.0.0.1", Owner: "haproxy", Kind: "frontend", Name: "http-in"},
		{Port: 25565, Proto: "tcp", Address: "0.0.0.0", Owner: "nginx", Kind: "stream", Name: "Minecraft", ID: "mc"},
		{Port: 51820, Proto: "udp", Address: "10.0.0.1", Owner: "nginx", Kind: "stream", Name: "WireGuard", ID: "wg"},
	}
	if c := portConflicts(entries, "0.0.0.0", 443, 443, "udp", ""); len(c) != 0 {
		t.Errorf("udp 443 does not clash with tcp 443: %v", c)
	}
	if c := portConflicts(entries, "10.0.0.5", 440, 450, "both", ""); len(c) != 1 || c[0].Port != 443 {
		t.Errorf("range over 443: %v", c)
	}
	if c := portConflicts(entries, "0.0.0.0", 10080, 10080, "tcp", ""); len(c) != 1 {
		t.Errorf("wildcard clashes with loopback frontend: %v", c)
	}
	if c := portConflicts(entries, "10.0.0.2", 51820, 51820, "udp", ""); len(c) != 0 {
		t.Errorf("different specific addresses don't clash: %v", c)
	}
	if c := portConflicts(entries, "0.0.0.0", 25565, 25565, "tcp", "mc"); len(c) != 0 {
		t.Errorf("editing a stream excludes itself: %v", c)
	}
	if c := portConflicts(entries, "::", 80, 80, "tcp", ""); len(c) != 0 {
		t.Errorf("IPv6 wildcard doesn't clash with IPv4 (ipv6only): %v", c)
	}
	if !strings.Contains(describeConflict(entries[3]), "Port 25565/tcp is used by stream Minecraft") {
		t.Error(describeConflict(entries[3]))
	}
}

func TestSplitBindAndStreamEntries(t *testing.T) {
	for in, want := range map[string]string{"127.0.0.1:10080": "127.0.0.1/10080", ":8080": "0.0.0.0/8080", "*:80": "0.0.0.0/80", "[::]:443": ":: /443"} {
		h, p, ok := splitBind(in)
		got := h + "/" + itoa(p)
		if want == ":: /443" {
			want = "::/443"
		}
		if !ok || got != want {
			t.Errorf("splitBind(%q) = %s %v", in, got, ok)
		}
	}
	if _, _, ok := splitBind("nonsense"); ok {
		t.Error("nonsense bind accepted")
	}
	s := &model.Stream{ID: "v", Name: "Valheim", Protocol: "both", ListenAddress: "0.0.0.0", ListenPorts: "2456-2458", Enabled: true}
	if e := streamEntries(s); len(e) != 6 || e[0].Port != 2456 || e[5].Proto != "udp" {
		t.Errorf("stream entries: %+v", e)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
