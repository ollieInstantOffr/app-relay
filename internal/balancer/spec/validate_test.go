package spec

import (
	"strings"
	"testing"
)

func validConfig() *Config {
	return &Config{
		Schema:        SchemaVersion,
		RuntimeSocket: "/run/relay/balancer-runtime.sock",
		MaxConn:       100,
		Check:         CheckDefaults{IntervalMs: 2000, Rise: 2, Fall: 3},
		Stats:         &Stats{Bind: "127.0.0.1:8404", Access: []IPRule{{Allow: true, CIDR: "10.0.0.0/8"}, {CIDR: "all"}}},
		Frontends: []Frontend{
			{ID: "f1", Name: "http-in", Mode: ModeHTTP, Bind: "127.0.0.1:10080", ForwardFor: true, ForwardForExcept: []string{"127.0.0.0/8"},
				Rules: []Rule{{Backend: "b1", Conditions: []Condition{
					{Type: CondHost, Match: MatchSuffix, Values: []string{".example.com"}},
					{Type: CondPathReg, Values: []string{"^/api/(a|b)$"}, Negate: true},
					{Type: CondHeader, Name: "X-Env", Match: MatchExact, Values: []string{"staging"}},
					{Type: CondHeader, Name: "X-Debug", Match: MatchFound},
					{Type: CondSrc, Values: []string{"0.0.0.0/0", "::/0"}},
				}}},
				DefaultBackend: "b1"},
			{ID: "f2", Name: "tls", Mode: ModeTCP, Bind: ":8443", AcceptProxy: true, InspectDelayMs: 5000,
				Rules: []Rule{{Backend: "b2", Conditions: []Condition{{Type: CondSNI, Values: []string{"db.lan"}}, {Type: CondHost, Values: []string{"never.lan"}}}}}},
		},
		Backends: []Backend{
			{ID: "b1", Name: "web", Mode: ModeHTTP, Algorithm: AlgoURI, Retries: 3, Redispatch: true, TLS: true, TLSVerify: true,
				Check:   &HealthCheck{Type: CheckHTTP, Method: "GET", Path: "/health", Expect: "200-299,404"},
				Sticky:  &Sticky{Mode: StickyInsert, Cookie: "SRVID"},
				Servers: []Server{{Name: "web-1", Address: "10.0.0.1", Port: 80, Weight: 100, State: StateReady, Cookie: "web-1"}, {Name: "web-2", Address: "fd00::2", Port: 80, Weight: 0, State: StateDrain, Backup: true}}},
			{ID: "b2", Name: "pg", Mode: ModeTCP, Algorithm: AlgoLeastConn, Check: &HealthCheck{Type: CheckPgSQL, User: "relay"},
				Sticky:  &Sticky{Mode: StickySource, ExpireMs: 1000, TableSize: 10},
				Servers: []Server{{Name: "pg-1", Address: "db.lan", Port: 5432, Weight: 1, State: StateMaint}}},
			{ID: "b3", Name: "unused", Mode: ModeTCP, Algorithm: AlgoFirst},
		},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(c *Config)
		want   []string // nil = valid
	}{
		{"valid", func(*Config) {}, nil},
		{"empty", func(c *Config) { *c = Config{Schema: SchemaVersion, RuntimeSocket: "/run/x.sock"} }, nil},
		{"schema", func(c *Config) { c.Schema = 2 }, []string{"unsupported schema 2"}},
		{"relative socket", func(c *Config) { c.RuntimeSocket = "run/x.sock" }, []string{"must be an absolute path"}},
		{"long socket", func(c *Config) { c.RuntimeSocket = "/" + strings.Repeat("s", 120) + ".sock" }, []string{"too long for a unix socket"}},
		{"negative timeouts", func(c *Config) { c.Timeouts.ClientMs = -1; c.Backends[0].Timeouts.QueueMs = -5 }, []string{"global timeouts must not be negative", `backend "web" timeouts must not be negative`}},
		{"bad mode", func(c *Config) { c.Frontends[0].Mode = "udp" }, []string{"mode must be http or tcp"}},
		{"bad bind", func(c *Config) { c.Frontends[0].Bind = "127.0.0.1" }, []string{"use host:port"}},
		{"bad port", func(c *Config) { c.Frontends[0].Bind = "127.0.0.1:70000" }, []string{"port must be 1–65535"}},
		{"duplicate bind", func(c *Config) { c.Frontends[1].Bind = "127.0.0.1:10080" }, []string{`bind 127.0.0.1:10080 is already used by frontend "http-in"`}},
		{"bind clashes with stats", func(c *Config) { c.Frontends[0].Bind = "127.0.0.1:8404" }, []string{"already used by stats"}},
		{"wildcard conflicts with specific", func(c *Config) { c.Frontends[1].Bind = "0.0.0.0:10080" }, []string{"conflicts with 127.0.0.1:10080", "wildcard address"}},
		{"specific after wildcard", func(c *Config) { c.Stats.Bind = ":10080" }, []string{"bind 127.0.0.1:10080 conflicts with :10080 (stats)"}},
		{"stats access", func(c *Config) { c.Stats.Access[0].CIDR = "10.0.0.300" }, []string{"stats access rule 1"}},
		{"missing backend", func(c *Config) { c.Frontends[0].DefaultBackend = "nope" }, []string{`default backend: backend "nope" does not exist`}},
		{"mode mismatch", func(c *Config) { c.Frontends[0].Rules[0].Backend = "b2" }, []string{`backend "pg" is in tcp mode, the frontend in http mode`}},
		{"bad regex", func(c *Config) { c.Frontends[0].Rules[0].Conditions[1].Values = []string{"(?<=x)"} }, []string{"path_reg"}},
		{"bad src", func(c *Config) { c.Frontends[0].Rules[0].Conditions[4].Values = []string{"nope"} }, []string{`src: "nope" is not an IP address or CIDR`}},
		{"header without name", func(c *Config) { c.Frontends[0].Rules[0].Conditions[2].Name = "" }, []string{"header name"}},
		{"host match", func(c *Config) { c.Frontends[0].Rules[0].Conditions[0].Match = "regex" }, []string{"match must be exact or suffix"}},
		{"unknown condition", func(c *Config) { c.Frontends[0].Rules[0].Conditions[0].Type = "cookie" }, []string{`unknown condition type "cookie"`}},
		{"no values", func(c *Config) { c.Frontends[0].Rules[0].Conditions[0].Values = nil }, []string{"host needs at least one value"}},
		{"except", func(c *Config) { c.Frontends[0].ForwardForExcept = []string{"x"} }, []string{"forwardForExcept"}},
		{"duplicate backend id", func(c *Config) { c.Backends[1].ID = "b1" }, []string{`duplicate backend id "b1"`}},
		{"duplicate backend name", func(c *Config) { c.Backends[1].Name = "web" }, []string{"duplicate backend name"}},
		{"bad names", func(c *Config) { c.Backends[0].Name = "a/b"; c.Backends[0].Servers[0].Name = "x y" }, []string{"name must be", `server "x y": name must be`}},
		{"algorithm", func(c *Config) { c.Backends[1].Algorithm = "hash" }, []string{`unknown algorithm "hash"`}},
		{"uri needs http", func(c *Config) { c.Backends[1].Algorithm = AlgoURI }, []string{"algorithm uri needs http mode"}},
		{"duplicate server", func(c *Config) { c.Backends[0].Servers[1].Name = "web-1" }, []string{"duplicate server name"}},
		{"server fields", func(c *Config) {
			s := &c.Backends[0].Servers[0]
			s.Address, s.Port, s.Weight, s.State = "", 0, 257, "down"
		}, []string{"address", "port must be 1–65535", "weight must be 0–256", "state must be ready, drain or maint"}},
		{"check type", func(c *Config) { c.Backends[0].Check.Type = "smtp" }, []string{`unknown check type "smtp"`}},
		{"check http", func(c *Config) {
			c.Backends[0].Check.Path, c.Backends[0].Check.Expect, c.Backends[0].Check.Method = "health", "20x", "get"
		}, []string{"check path must start with /", `check expect "20x"`, `check method "get"`}},
		{"sticky cookie needs http", func(c *Config) { c.Backends[1].Sticky = &Sticky{Mode: StickyPrefix, Cookie: "S"} }, []string{"sticky prefix needs http mode"}},
		{"sticky cookie name", func(c *Config) { c.Backends[0].Sticky.Cookie = "" }, []string{"sticky cookie name"}},
		{"sticky mode", func(c *Config) { c.Backends[0].Sticky.Mode = "table" }, []string{`unknown sticky mode "table"`}},
		{"verify needs tls", func(c *Config) { c.Backends[0].TLS = false }, []string{"tlsVerify needs tls"}},
		{"retries", func(c *Config) { c.Backends[0].Retries = -1 }, []string{"retries must not be negative"}},
		{"several", func(c *Config) { c.Schema = 9; c.Backends[0].Algorithm = "x" }, []string{"unsupported schema 9", `unknown algorithm "x"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(c)
			err := c.Validate()
			if tt.want == nil {
				if err != nil {
					t.Fatalf("valid config rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid config accepted")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q:\n%v", w, err)
				}
			}
		})
	}
}

func TestParseHelpers(t *testing.T) {
	if h, p, err := ParseBind("*:80"); err != nil || h != "" || p != 80 {
		t.Errorf("*:80 → %q %d %v", h, p, err)
	}
	if h, p, err := ParseBind("[::1]:443"); err != nil || h != "::1" || p != 443 {
		t.Errorf("[::1]:443 → %q %d %v", h, p, err)
	}
	for in, want := range map[string]string{"10.1.2.3": "10.1.2.3/32", "10.1.2.3/8": "10.0.0.0/8", "::ffff:10.0.0.1": "10.0.0.1/32", "fd00::1/64": "fd00::/64", "::ffff:10.0.0.0/104": "10.0.0.0/8"} {
		if p, err := ParsePrefix(in); err != nil || p.String() != want {
			t.Errorf("%s → %v %v, want %s", in, p, err, want)
		}
	}
	if _, all, err := ParseIPRule("ALL"); !all || err != nil {
		t.Error("all")
	}
}
