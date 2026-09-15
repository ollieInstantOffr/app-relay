package balancer

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/balancer/spec"
)

func TestShowStatHeaderMatchesHAProxy(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "lb", "testdata", "showstat.csv"))
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := strings.Cut(string(data), "\n")
	want := strings.TrimSuffix(strings.TrimPrefix(line, "# "), ",")
	if statHeader != want {
		t.Fatalf("show stat header differs from HAProxy's:\n got %s\nwant %s", statHeader, want)
	}
	// Every column internal/lb reads is present.
	for _, col := range []string{"pxname", "svname", "status", "rate", "scur", "smax", "qcur", "stot", "lbtot", "econ", "eresp", "ereq",
		"wretr", "wredis", "rtime", "lastchg", "weight", "bck", "check_status", "check_code", "check_desc", "check_fall", "check_duration", "addr"} {
		if _, ok := statIdx[col]; !ok {
			t.Errorf("missing column %s", col)
		}
	}
}

func TestRuntimeAPI(t *testing.T) {
	a, b := newUpstream(t, "a", nil), newUpstream(t, "b", nil)
	cfg := baseConfig(t)
	sb := b.server()
	sb.Backup = true
	sd := spec.Server{Name: "dead", Address: "127.0.0.1", Port: closedPort(t), Weight: 7, State: spec.StateReady, Check: true}
	web := httpBackend("web", a.server(), sb)
	chk := httpBackend("chk", sd)
	chk.Check = &spec.HealthCheck{Type: spec.CheckTCP, IntervalMs: 20, Rise: 1, Fall: 1}
	cfg.Backends = []spec.Backend{web, chk}
	cfg.Frontends = []spec.Frontend{httpFrontend(t, "http-in", "web")}
	cfg.Stats = &spec.Stats{Bind: freeBind(t)}
	e := startEnv(t, cfg)

	fi, err := os.Stat(cfg.RuntimeSocket)
	if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o660 {
		t.Fatalf("runtime socket: %v %v", fi.Mode(), err)
	}
	for range 3 {
		get(t, e.feURL(0, "/"))
	}

	out := e.cmd("show stat")
	if !strings.HasPrefix(out, "# pxname,svname,qcur,") || !strings.HasSuffix(out, ",\n\n") {
		t.Fatalf("show stat framing: %q", out[:min(len(out), 80)])
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var order []string
	for _, l := range lines[1:] {
		f := strings.Split(l, ",")
		if len(f) != len(statCols)+1 {
			t.Fatalf("row has %d fields, header %d: %s", len(f), len(statCols)+1, l)
		}
		order = append(order, f[0]+"/"+f[1])
	}
	if strings.Join(order, " ") != "stats/FRONTEND http-in/FRONTEND web/a web/b web/BACKEND chk/dead chk/BACKEND" {
		t.Fatalf("row order: %v", order)
	}
	fe := statFrom(t, out, "http-in", "FRONTEND")
	if fe["status"] != "OPEN" || fe["req_tot"] != "3" || fe["mode"] != "http" || fe["type"] != "0" {
		t.Fatalf("frontend row: %v", fe)
	}
	sa := statFrom(t, out, "web", "a")
	if sa["status"] != "no check" || sa["stot"] != "3" || sa["lbtot"] != "3" || sa["weight"] != "100" || sa["bck"] != "0" || sa["act"] != "1" ||
		sa["addr"] != "127.0.0.1:"+strconv.Itoa(a.port) || sa["hrsp_2xx"] != "3" || sa["check_status"] != "" || sa["sid"] != "1" || sa["type"] != "2" {
		t.Fatalf("server row: %v", sa)
	}
	if sbk := statFrom(t, out, "web", "b"); sbk["bck"] != "1" || sbk["act"] != "0" {
		t.Fatalf("backup row: %v", sbk)
	}
	be := statFrom(t, out, "web", "BACKEND")
	if be["status"] != "UP" || be["weight"] != "100" || be["act"] != "1" || be["bck"] != "1" || be["stot"] != "3" || be["algo"] != "roundrobin" || be["rtime"] == "" {
		t.Fatalf("backend row: %v", be)
	}
	if !waitFor(t, 3*time.Second, func() bool { return e.stat("chk", "dead")["status"] == "DOWN" }) {
		t.Fatalf("checked server: %v", e.stat("chk", "dead"))
	}
	dead, chkBE := e.stat("chk", "dead"), e.stat("chk", "BACKEND")
	if dead["check_status"] != "L4CON" || dead["check_desc"] != "Layer4 connection problem" || dead["check_code"] != "" || dead["check_fall"] != "1" ||
		dead["check_rise"] != "1" || dead["chkdown"] != "1" || dead["last_chk"] != "connection refused" || dead["check_duration"] == "" {
		t.Fatalf("down server row: %v", dead)
	}
	if chkBE["status"] != "DOWN" || chkBE["weight"] != "0" {
		t.Fatalf("down backend row: %v", chkBE)
	}

	// show info
	info := e.cmd("show info")
	pid := parseInfoLine(info, "Pid")
	if parseInfoLine(info, "Name") != "Relay Balancer" || parseInfoLine(info, "Version") != "test" || pid == "" || parseInfoLine(info, "Uptime_sec") == "" ||
		parseInfoLine(info, "CumReq") != "3" || !strings.HasSuffix(info, "\n\n") {
		t.Fatalf("show info:\n%s", info)
	}
	if err := e.reload("h2"); err != nil {
		t.Fatal(err)
	}
	if p2 := parseInfoLine(e.cmd("show info"), "Pid"); p2 == pid || p2 == "" {
		t.Fatalf("Pid must change on reload: %s → %s", pid, p2)
	}
	if st := e.stat("web", "a"); st["stot"] != "3" {
		t.Fatalf("counters must survive a reload: %v", st["stot"])
	}

	// set server
	cases := []struct {
		cmd, out   string
		px, sv     string
		col, value string
	}{
		{"set server web/a state drain", "", "web", "a", "status", "DRAIN"},
		{"set server web/a state maint", "", "web", "a", "status", "MAINT"},
		{"set server web/a state ready", "", "web", "a", "status", "no check"},
		{"set server web/a weight 50", "", "web", "a", "weight", "50"},
		{"set server web/a weight 10%", "", "web", "a", "weight", "10"},
		{"set weight web/a 200%", "", "web", "a", "weight", "200"},
		{"set server web/a weight 0", "", "web", "BACKEND", "act", "0"}, // the backup takes over
		{"set server web/a weight 100", "", "web", "BACKEND", "act", "1"},
		{"disable server web/b", "", "web", "b", "status", "MAINT"},
		{"enable server web/b", "", "web", "b", "status", "no check"},
		{"set server nope/a state drain", "No such backend.", "", "", "", ""},
		{"set server web/nope state drain", "No such server.", "", "", "", ""},
		{"set server web state drain", "Require 'backend/server'.", "", "", "", ""},
		{"set server web/a state up", "'set server <srv> state' expects 'ready', 'drain' and 'maint'.", "", "", "", ""},
		{"set server web/a weight 300", "Absolute weight can only be between 0 and 256 inclusive.", "", "", "", ""},
		{"set server web/a weight x", "Trailing garbage in weight string.", "", "", "", ""},
		{"set server web/a frobnicate", "'set server <srv>' only supports", "", "", "", ""},
		{"get weight web/a", "100 (initial 100)", "", "", "", ""},
	}
	for _, c := range cases {
		got := strings.TrimSpace(e.cmd(c.cmd))
		if c.out == "" && got != "" || c.out != "" && !strings.HasPrefix(got, c.out) {
			t.Errorf("%s → %q, want %q", c.cmd, got, c.out)
		}
		if c.px != "" {
			if v := e.stat(c.px, c.sv)[c.col]; v != c.value {
				t.Errorf("%s: %s/%s %s = %q, want %q", c.cmd, c.px, c.sv, c.col, v, c.value)
			}
		}
	}
	if out := e.cmd("show frobs"); !strings.HasPrefix(out, "Unknown command: 'show frobs'") {
		t.Errorf("unknown command: %q", out)
	}
	if out := e.cmd("set server web/a state drain; get weight web/a"); strings.TrimSpace(out) != "100 (initial 100)" || e.stat("web", "a")["status"] != "DRAIN" {
		t.Errorf("multiple commands: %q", out)
	}
	e.waitLog(t, "[WARNING]  (")
	e.waitLog(t, "Server web/a enters drain state.")
	e.waitLog(t, "Server web/a is going DOWN for maintenance.")
	e.waitLog(t, "[ALERT]    (")
	e.waitLog(t, "backend chk has no server available!")
}

func parseInfoLine(info, key string) string {
	for _, l := range strings.Split(info, "\n") {
		if k, v, ok := strings.Cut(l, ": "); ok && k == key {
			return v
		}
	}
	return ""
}

func TestRuntimeSocketStaleAndMoved(t *testing.T) {
	cfg := baseConfig(t)
	// A stale socket file from a crashed process.
	stale, err := net.Listen("unix", cfg.RuntimeSocket)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	e := startEnv(t, cfg)
	if !strings.HasPrefix(e.cmd("show stat"), "# pxname") {
		t.Fatal("runtime socket not answering")
	}
	old := cfg.RuntimeSocket
	e.cfg.RuntimeSocket = shortSocket(t)
	if err := e.reload("h2"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(e.cmd("show info"), "Name: Relay Balancer") {
		t.Fatal("moved runtime socket not answering")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old socket still present: %v", err)
	}
	// Empty configuration: idle but the runtime API works.
	if !strings.Contains(e.cmd("show stat"), "# pxname") {
		t.Fatal("empty config show stat")
	}
}

func TestStatsListener(t *testing.T) {
	u := newUpstream(t, "a", nil)
	cfg := baseConfig(t)
	cfg.Backends = []spec.Backend{httpBackend("web", u.server())}
	cfg.Frontends = []spec.Frontend{httpFrontend(t, "fe", "web")}
	cfg.Stats = &spec.Stats{Bind: freeBind(t), Prometheus: true}
	e := startEnv(t, cfg)
	get(t, e.feURL(0, "/"))
	base := "http://" + cfg.Stats.Bind
	page := get(t, base+"/")
	if page.StatusCode != 200 || page.Header.Get("Refresh") != "10" || !strings.Contains(page.body, "<caption>web</caption>") || !strings.Contains(page.body, "Relay Balancer statistics") {
		t.Fatalf("html: %d %v %.200s", page.StatusCode, page.Header, page.body)
	}
	csv := get(t, base+"/;csv")
	if !strings.HasPrefix(csv.body, "# pxname,svname,") || !strings.Contains(csv.body, "\nweb,a,") {
		t.Fatalf("csv: %.200s", csv.body)
	}
	m := get(t, base+"/metrics").body
	for _, want := range []string{
		"# TYPE haproxy_process_current_connections gauge",
		`haproxy_frontend_http_requests_total{proxy="fe"} 1`,
		`haproxy_frontend_sessions_total{proxy="fe"} 1`,
		`haproxy_backend_status{proxy="web",state="UP"} 1`,
		`haproxy_backend_status{proxy="web",state="DOWN"} 0`,
		`haproxy_backend_http_responses_total{proxy="web",code="2xx"} 1`,
		`haproxy_server_weight{proxy="web",server="a"} 100`,
		`haproxy_server_status{proxy="web",server="a",state="UP"} 1`,
		`haproxy_server_sessions_total{proxy="web",server="a"} 1`,
		`haproxy_backend_connection_errors_total{proxy="web"} 0`,
		`haproxy_frontend_status{proxy="stats"} 1`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %q", want)
		}
	}

	// Access rules: first match wins; unmatched clients are allowed.
	e.cfg.Stats.Access = []spec.IPRule{{Allow: false, CIDR: "127.0.0.1"}, {Allow: true, CIDR: "all"}}
	if err := e.reload("h2"); err != nil {
		t.Fatal(err)
	}
	if r := get(t, base+"/metrics"); r.StatusCode != 403 || !strings.Contains(r.body, "403 Forbidden") {
		t.Fatalf("denied: %d", r.StatusCode)
	}
	e.cfg.Stats.Access = []spec.IPRule{{Allow: true, CIDR: "127.0.0.0/8"}, {Allow: false, CIDR: "all"}}
	e.cfg.Stats.Prometheus = false
	if err := e.reload("h3"); err != nil {
		t.Fatal(err)
	}
	if r := get(t, base+"/;csv"); r.StatusCode != 200 {
		t.Fatalf("allowed: %d", r.StatusCode)
	}
	if r := get(t, base+"/metrics"); strings.Contains(r.body, "haproxy_process") {
		t.Fatal("metrics served without prometheus")
	}
	e.cfg.Stats.Access = []spec.IPRule{{Allow: true, CIDR: "10.0.0.0/8"}}
	e.reload("h4")
	if r := get(t, base+"/"); r.StatusCode != 403 {
		// 127.0.0.1 matches no rule → allowed
		_ = r
	}
	if r := get(t, base+"/"); r.StatusCode != http.StatusOK {
		t.Fatalf("unmatched client must be allowed: %d", r.StatusCode)
	}
}
