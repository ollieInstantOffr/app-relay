package balancer

import (
	"fmt"
	"html"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// serveStats is the stats listener: "/" HTML page (refresh 10 s), "/;csv"
// the show stat CSV and, with Prometheus, "/metrics". Access rules are
// evaluated first (first match wins, unmatched clients are allowed).
func (bl *boundListener) serveStats(w http.ResponseWriter, r *http.Request) {
	sc := bl.stats.Load()
	s := bl.srv
	now := time.Now()
	c := &sc.st.c
	c.sessionStart(now)
	defer c.sessionEnd()
	c.request(now)
	ap, _ := netip.ParseAddrPort(r.RemoteAddr)
	if !accessAllowed(sc.rules, ap.Addr().Unmap()) {
		body := errorPage(http.StatusForbidden)
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusForbidden)
		w.Write(body)
		c.status(http.StatusForbidden)
		return
	}
	rt := s.rt.Load()
	var out string
	switch {
	case sc.prometheus && r.URL.Path == "/metrics":
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		out = s.prometheus(rt)
	case strings.Contains(r.RequestURI, ";csv"):
		w.Header().Set("Content-Type", "text/plain")
		out = s.showStat(rt)
	default:
		w.Header().Set("Content-Type", "text/html")
		if !strings.Contains(r.RequestURI, ";norefresh") {
			w.Header().Set("Refresh", "10")
		}
		out = s.statsHTML(rt)
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Write([]byte(out))
	c.status(http.StatusOK)
	c.bout.Add(int64(len(out)))
}

func accessAllowed(rules []accessRule, a netip.Addr) bool {
	for _, r := range rules {
		if r.all || r.p.Contains(a) {
			return r.allow
		}
	}
	return true
}

// ---------------------------------------------------------------- HTML

const statsCSS = `body{font-family:Arial,Helvetica,sans-serif;font-size:12px;margin:12px;color:#222;background:#fff}
h1{font-size:18px;margin:0 0 4px}p.info{margin:0 0 12px;color:#555}
table{border-collapse:collapse;margin:0 0 14px;min-width:60%}
th{background:#20435c;color:#fff;font-weight:normal;padding:2px 5px;border:1px solid #20435c}
td{padding:2px 5px;border:1px solid #bbb;text-align:right;white-space:nowrap}
td.n{text-align:left;font-weight:bold}caption{text-align:left;font-weight:bold;font-size:14px;padding:2px 0;color:#20435c}
tr.FRONTEND td,tr.BACKEND td{background:#eef;font-weight:bold}
tr.UP td{background:#c0ffc0}tr.DOWN td{background:#ff9090}tr.MAINT td{background:#c07820;color:#fff}
tr.DRAIN td{background:#20a0ff}tr.GOING td{background:#ffffa0}tr.NOCHECK td{background:#e0e0e0}
.w{overflow-x:auto}`

func statusClassCSS(status, svname string) string {
	switch {
	case svname == "FRONTEND" || svname == "BACKEND":
		if status == "DOWN" {
			return "DOWN"
		}
		return svname
	case strings.HasPrefix(status, "MAINT"):
		return "MAINT"
	case strings.HasPrefix(status, "DRAIN"):
		return "DRAIN"
	case status == "no check":
		return "NOCHECK"
	case strings.Contains(status, "/"):
		return "GOING"
	case strings.HasPrefix(status, "DOWN"):
		return "DOWN"
	}
	return "UP"
}

func (s *Server) statsHTML(rt *runtime) string {
	var b strings.Builder
	esc := html.EscapeString
	now := time.Now()
	up := int64(now.Sub(s.started) / time.Second)
	fmt.Fprintf(&b, "<!doctype html><html><head><meta charset=\"utf-8\"><title>Statistics Report for Relay Balancer</title><style>%s</style></head><body>", statsCSS)
	fmt.Fprintf(&b, "<h1>Relay Balancer statistics</h1><p class=\"info\">pid %d · uptime %dd %dh%02dm%02ds · maxconn %d · current connections %d · config %s · %s</p>",
		s.pid(), up/86400, up%86400/3600, up%3600/60, up%60, rt.maxConn, s.actconn.Load(), esc(rt.hash), esc(now.Format(time.RFC1123)))
	rows := s.statRows(rt)
	cols := []struct{ head, col string }{
		{"Qcur", "qcur"}, {"Qmax", "qmax"}, {"Rate", "rate"}, {"Rate max", "rate_max"},
		{"Scur", "scur"}, {"Smax", "smax"}, {"Slim", "slim"}, {"Total", "stot"}, {"LbTot", "lbtot"},
		{"Bytes in", "bin"}, {"Bytes out", "bout"}, {"Err req", "ereq"}, {"Err conn", "econ"}, {"Err resp", "eresp"},
		{"Retr", "wretr"}, {"Redis", "wredis"}, {"Status", "status"}, {"LastChk", "check_status"},
		{"Wght", "weight"}, {"Act", "act"}, {"Bck", "bck"}, {"Chk", "chkfail"}, {"Dwn", "chkdown"}, {"Dwntme", "downtime"}, {"Rtime", "rtime"},
	}
	open := ""
	for _, r := range rows {
		px := r.get("pxname")
		if px != open {
			if open != "" {
				b.WriteString("</table></div>")
			}
			open = px
			fmt.Fprintf(&b, "<div class=\"w\"><table><caption>%s</caption><tr><th>Name</th>", esc(px))
			for _, c := range cols {
				fmt.Fprintf(&b, "<th>%s</th>", c.head)
			}
			b.WriteString("</tr>")
		}
		fmt.Fprintf(&b, "<tr class=\"%s\"><td class=\"n\">%s</td>", statusClassCSS(r.get("status"), r.get("svname")), esc(r.get("svname")))
		for _, c := range cols {
			v := r.get(c.col)
			switch c.col {
			case "check_status":
				if d := r.get("check_duration"); v != "" && d != "" {
					v += " in " + d + "ms"
				}
			case "downtime", "lastchg":
				if v != "" {
					n, _ := strconv.ParseInt(v, 10, 64)
					v = humanSeconds(n)
				}
			}
			fmt.Fprintf(&b, "<td>%s</td>", esc(v))
		}
		b.WriteString("</tr>")
	}
	if open != "" {
		b.WriteString("</table></div>")
	}
	b.WriteString("<p class=\"info\">Also available: <a href=\"/;csv\">CSV export</a></p></body></html>\n")
	return b.String()
}

func humanSeconds(n int64) string {
	switch {
	case n < 60:
		return fmt.Sprintf("%ds", n)
	case n < 3600:
		return fmt.Sprintf("%dm%02ds", n/60, n%60)
	case n < 86400:
		return fmt.Sprintf("%dh%02dm", n/3600, n%3600/60)
	}
	return fmt.Sprintf("%dd%02dh", n/86400, n%86400/3600)
}

// ---------------------------------------------------------------- Prometheus

type promMetric struct {
	name, help, typ, col string
	scale                float64 // multiply the CSV value (ms → s)
}

var (
	promFrontend = []promMetric{
		{"current_sessions", "Number of current sessions.", "gauge", "scur", 0},
		{"max_sessions", "Maximum observed number of sessions.", "gauge", "smax", 0},
		{"limit_sessions", "Configured session limit.", "gauge", "slim", 0},
		{"sessions_total", "Total number of sessions.", "counter", "stot", 0},
		{"bytes_in_total", "Current total of incoming bytes.", "counter", "bin", 0},
		{"bytes_out_total", "Current total of outgoing bytes.", "counter", "bout", 0},
		{"requests_denied_total", "Total number of denied requests.", "counter", "dreq", 0},
		{"request_errors_total", "Total number of request errors.", "counter", "ereq", 0},
		{"current_session_rate", "Current number of sessions per second over last elapsed second.", "gauge", "rate", 0},
		{"max_session_rate", "Maximum observed number of sessions per second.", "gauge", "rate_max", 0},
		{"connections_total", "Total number of connections.", "counter", "conn_tot", 0},
		{"http_requests_total", "Total number of HTTP requests received.", "counter", "req_tot", 0},
		{"http_comp_bytes_in_total", "Total number of HTTP response bytes fed to the compressor.", "counter", "comp_in", 0},
		{"http_comp_bytes_out_total", "Total number of HTTP response bytes emitted by the compressor.", "counter", "comp_out", 0},
		{"http_comp_bytes_bypassed_total", "Total number of bytes that bypassed the HTTP compressor.", "counter", "comp_byp", 0},
		{"http_comp_responses_total", "Total number of HTTP responses that were compressed.", "counter", "comp_rsp", 0},
	}
	promBackend = []promMetric{
		{"current_queue", "Current number of queued requests.", "gauge", "qcur", 0},
		{"max_queue", "Maximum observed number of queued requests.", "gauge", "qmax", 0},
		{"current_sessions", "Number of current sessions.", "gauge", "scur", 0},
		{"max_sessions", "Maximum observed number of sessions.", "gauge", "smax", 0},
		{"limit_sessions", "Configured session limit.", "gauge", "slim", 0},
		{"sessions_total", "Total number of sessions.", "counter", "stot", 0},
		{"bytes_in_total", "Current total of incoming bytes.", "counter", "bin", 0},
		{"bytes_out_total", "Current total of outgoing bytes.", "counter", "bout", 0},
		{"connection_errors_total", "Total number of connection errors.", "counter", "econ", 0},
		{"response_errors_total", "Total number of response errors.", "counter", "eresp", 0},
		{"retry_warnings_total", "Total number of retry warnings.", "counter", "wretr", 0},
		{"redispatch_warnings_total", "Total number of redispatch warnings.", "counter", "wredis", 0},
		{"weight", "Service weight.", "gauge", "weight", 0},
		{"active_servers", "Current number of active servers.", "gauge", "act", 0},
		{"backup_servers", "Current number of backup servers.", "gauge", "bck", 0},
		{"check_up_down_total", "Total number of UP->DOWN transitions.", "counter", "chkdown", 0},
		{"check_last_change_seconds", "How long ago the last server state changed, in seconds.", "gauge", "lastchg", 0},
		{"downtime_seconds_total", "Total downtime (in seconds) for the service.", "counter", "downtime", 0},
		{"loadbalanced_total", "Total number of times a service was selected.", "counter", "lbtot", 0},
		{"current_session_rate", "Current number of sessions per second over last elapsed second.", "gauge", "rate", 0},
		{"max_session_rate", "Maximum observed number of sessions per second.", "gauge", "rate_max", 0},
		{"last_session_seconds", "How long ago some traffic was seen on this object, in seconds.", "gauge", "lastsess", 0},
		{"http_requests_total", "Total number of HTTP requests received.", "counter", "req_tot", 0},
		{"client_aborts_total", "Total number of data transfers aborted by the client.", "counter", "cli_abrt", 0},
		{"server_aborts_total", "Total number of data transfers aborted by the server.", "counter", "srv_abrt", 0},
		{"queue_time_average_seconds", "Avg. queue time for last 1024 successful connections.", "gauge", "qtime", 0.001},
		{"connect_time_average_seconds", "Avg. connect time for last 1024 successful connections.", "gauge", "ctime", 0.001},
		{"response_time_average_seconds", "Avg. response time for last 1024 successful connections.", "gauge", "rtime", 0.001},
		{"total_time_average_seconds", "Avg. total time for last 1024 successful connections.", "gauge", "ttime", 0.001},
		{"http_comp_bytes_in_total", "Total number of HTTP response bytes fed to the compressor.", "counter", "comp_in", 0},
		{"http_comp_bytes_out_total", "Total number of HTTP response bytes emitted by the compressor.", "counter", "comp_out", 0},
		{"http_comp_responses_total", "Total number of HTTP responses that were compressed.", "counter", "comp_rsp", 0},
	}
	promServer = []promMetric{
		{"current_queue", "Current number of queued requests.", "gauge", "qcur", 0},
		{"max_queue", "Maximum observed number of queued requests.", "gauge", "qmax", 0},
		{"current_sessions", "Number of current sessions.", "gauge", "scur", 0},
		{"max_sessions", "Maximum observed number of sessions.", "gauge", "smax", 0},
		{"sessions_total", "Total number of sessions.", "counter", "stot", 0},
		{"bytes_in_total", "Current total of incoming bytes.", "counter", "bin", 0},
		{"bytes_out_total", "Current total of outgoing bytes.", "counter", "bout", 0},
		{"connection_errors_total", "Total number of connection errors.", "counter", "econ", 0},
		{"response_errors_total", "Total number of response errors.", "counter", "eresp", 0},
		{"retry_warnings_total", "Total number of retry warnings.", "counter", "wretr", 0},
		{"redispatch_warnings_total", "Total number of redispatch warnings.", "counter", "wredis", 0},
		{"weight", "Service weight.", "gauge", "weight", 0},
		{"check_failures_total", "Total number of failed checks (only counts checks failed when the server is up).", "counter", "chkfail", 0},
		{"check_up_down_total", "Total number of UP->DOWN transitions.", "counter", "chkdown", 0},
		{"check_last_change_seconds", "How long ago the last server state changed, in seconds.", "gauge", "lastchg", 0},
		{"downtime_seconds_total", "Total downtime (in seconds) for the service.", "counter", "downtime", 0},
		{"loadbalanced_total", "Total number of times a service was selected.", "counter", "lbtot", 0},
		{"current_session_rate", "Current number of sessions per second over last elapsed second.", "gauge", "rate", 0},
		{"max_session_rate", "Maximum observed number of sessions per second.", "gauge", "rate_max", 0},
		{"last_session_seconds", "How long ago some traffic was seen on this object, in seconds.", "gauge", "lastsess", 0},
		{"check_duration_seconds", "Total duration of the latest server health check, in seconds.", "gauge", "check_duration", 0.001},
		{"client_aborts_total", "Total number of data transfers aborted by the client.", "counter", "cli_abrt", 0},
		{"server_aborts_total", "Total number of data transfers aborted by the server.", "counter", "srv_abrt", 0},
		{"queue_time_average_seconds", "Avg. queue time for last 1024 successful connections.", "gauge", "qtime", 0.001},
		{"connect_time_average_seconds", "Avg. connect time for last 1024 successful connections.", "gauge", "ctime", 0.001},
		{"response_time_average_seconds", "Avg. response time for last 1024 successful connections.", "gauge", "rtime", 0.001},
		{"total_time_average_seconds", "Avg. total time for last 1024 successful connections.", "gauge", "ttime", 0.001},
	}
)

func promLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

// prometheus renders a subset of HAProxy's prometheus-exporter metrics with
// the same names and proxy/server labels.
func (s *Server) prometheus(rt *runtime) string {
	var b strings.Builder
	head := func(name, help, typ string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	now := time.Now()
	process := []struct {
		name, help, typ string
		v               float64
	}{
		{"haproxy_process_nbthread", "Number of started threads (global.nbthread)", "gauge", 1},
		{"haproxy_process_nbproc", "Number of started worker processes (historical, always 1)", "gauge", 1},
		{"haproxy_process_relative_process_id", "Relative worker process number (1)", "gauge", 1},
		{"haproxy_process_uptime_seconds", "How long ago this worker process was started (seconds)", "gauge", float64(now.Sub(s.started) / time.Second)},
		{"haproxy_process_start_time_seconds", "Start time in seconds", "gauge", float64(s.started.Unix())},
		{"haproxy_process_max_connections", "Maximum number of concurrent connections (global.maxconn)", "gauge", float64(rt.maxConn)},
		{"haproxy_process_current_connections", "Current number of connections on this worker process", "gauge", float64(s.actconn.Load())},
		{"haproxy_process_connections_total", "Total number of connections on this worker process since started", "counter", float64(s.cumConns.Load())},
		{"haproxy_process_requests_total", "Total number of requests on this worker process since started", "counter", float64(s.cumReq.Load())},
		{"haproxy_process_dropped_logs_total", "Total number of dropped logs for current worker process since started", "counter", float64(s.log.dropped())},
		{"haproxy_process_stopping", "Non-zero means stopping in progress", "gauge", map[bool]float64{true: 1}[s.stopping.Load()]},
	}
	for _, p := range process {
		head(p.name, p.help, p.typ)
		fmt.Fprintf(&b, "%s %s\n", p.name, strconv.FormatFloat(p.v, 'f', -1, 64))
	}

	rows := s.statRows(rt)
	type group struct {
		prefix  string
		metrics []promMetric
		rows    []statRow
		states  []string
	}
	groups := []*group{
		{prefix: "haproxy_frontend_", metrics: promFrontend, states: nil},
		{prefix: "haproxy_backend_", metrics: promBackend, states: []string{"DOWN", "UP"}},
		{prefix: "haproxy_server_", metrics: promServer, states: []string{"DOWN", "UP", "MAINT", "DRAIN", "NOLB"}},
	}
	for _, r := range rows {
		switch r.get("svname") {
		case "FRONTEND":
			groups[0].rows = append(groups[0].rows, r)
		case "BACKEND":
			groups[1].rows = append(groups[1].rows, r)
		default:
			groups[2].rows = append(groups[2].rows, r)
		}
	}
	labels := func(g *group, r statRow) string {
		if g.prefix == "haproxy_server_" {
			return fmt.Sprintf(`proxy="%s",server="%s"`, promLabel(r.get("pxname")), promLabel(r.get("svname")))
		}
		return fmt.Sprintf(`proxy="%s"`, promLabel(r.get("pxname")))
	}
	for _, g := range groups {
		if len(g.rows) == 0 {
			continue
		}
		name := g.prefix + "status"
		head(name, "Current status of the service.", "gauge")
		for _, r := range g.rows {
			st := normPromStatus(r.get("status"))
			if g.states == nil {
				v := 0
				if st == "OPEN" {
					v = 1
				}
				fmt.Fprintf(&b, "%s{%s} %d\n", name, labels(g, r), v)
				continue
			}
			for _, state := range g.states {
				v := 0
				if state == st {
					v = 1
				}
				fmt.Fprintf(&b, "%s{%s,state=\"%s\"} %d\n", name, labels(g, r), state, v)
			}
		}
		for _, m := range g.metrics {
			name := g.prefix + m.name
			printed := false
			for _, r := range g.rows {
				v := r.get(m.col)
				if v == "" {
					continue
				}
				if !printed {
					head(name, m.help, m.typ)
					printed = true
				}
				f, _ := strconv.ParseFloat(v, 64)
				if m.scale != 0 {
					f *= m.scale
				}
				fmt.Fprintf(&b, "%s{%s} %s\n", name, labels(g, r), strconv.FormatFloat(f, 'f', -1, 64))
			}
		}
		name = g.prefix + "http_responses_total"
		printed := false
		for _, r := range g.rows {
			if r.get("hrsp_2xx") == "" {
				continue
			}
			if !printed {
				head(name, "Total number of HTTP responses.", "counter")
				printed = true
			}
			for _, code := range []string{"1xx", "2xx", "3xx", "4xx", "5xx", "other"} {
				fmt.Fprintf(&b, "%s{%s,code=\"%s\"} %s\n", name, labels(g, r), code, r.get("hrsp_"+code))
			}
		}
	}
	return b.String()
}

func normPromStatus(s string) string {
	switch {
	case strings.HasPrefix(s, "MAINT"):
		return "MAINT"
	case strings.HasPrefix(s, "DRAIN"):
		return "DRAIN"
	case strings.HasPrefix(s, "NOLB"):
		return "NOLB"
	case strings.HasPrefix(s, "DOWN"):
		return "DOWN"
	case strings.HasPrefix(s, "UP"), s == "no check":
		return "UP"
	}
	return s
}
