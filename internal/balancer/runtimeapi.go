package balancer

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/balancer/spec"
	"github.com/instantoffr/relay/internal/lbcheck"
)

// runtimeSocket answers HAProxy runtime API commands in non-interactive
// mode: one command line per connection (several separated by ';'), the
// output, then the connection is closed.
type runtimeSocket struct {
	path string
	ln   net.Listener
	srv  *Server
}

func listenRuntime(path string, s *Server) (*runtimeSocket, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		os.Remove(path) // stale socket of a previous process
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(true)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	return &runtimeSocket{path: path, ln: ln, srv: s}, nil
}

func (rs *runtimeSocket) serve() {
	for {
		c, err := rs.ln.Accept()
		if err != nil {
			if isTimeout(err) {
				continue
			}
			return
		}
		go rs.handle(c)
	}
}

func (rs *runtimeSocket) handle(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	line, err := bufio.NewReaderSize(io.LimitReader(c, 1<<20), 4096).ReadString('\n')
	if err != nil && line == "" {
		return
	}
	io.WriteString(c, rs.srv.execLine(line))
}

func (rs *runtimeSocket) close() { rs.ln.Close() }

const runtimeHelp = `The following commands are valid at this level:
  help                                    : list the available commands
  show info                               : report information about the running process
  show stat                               : report counters for each proxy and server
  get weight <bk>/<srv>                   : report a server's current weight
  set weight <bk>/<srv> <w>[%]            : change a server's weight (deprecated)
  set server <bk>/<srv> state <state>     : set a server's admin state (ready, drain, maint)
  set server <bk>/<srv> weight <w>[%]     : change a server's weight
  set server <bk>/<srv> health <state>    : force a server's health (up, stopping, down)
  enable server <bk>/<srv>                : leave maintenance
  disable server <bk>/<srv>               : enter maintenance
`

func (s *Server) execLine(line string) string {
	var b strings.Builder
	for cmd := range strings.SplitSeq(strings.TrimRight(line, "\r\n"), ";") {
		if cmd = strings.TrimSpace(cmd); cmd != "" {
			b.WriteString(s.exec(cmd))
		}
	}
	return b.String()
}

func (s *Server) exec(cmd string) string {
	rt := s.rt.Load()
	f := strings.Fields(cmd)
	word := func(i int) string {
		if i < len(f) {
			return f[i]
		}
		return ""
	}
	if rt == nil {
		return "Not ready.\n"
	}
	switch {
	case f[0] == "help":
		return runtimeHelp
	case f[0] == "show" && word(1) == "stat":
		return s.showStat(rt) + "\n"
	case f[0] == "show" && word(1) == "info":
		return s.showInfo(rt) + "\n"
	case f[0] == "set" && word(1) == "server":
		return setServer(rt, f[2:])
	case f[0] == "set" && word(1) == "weight":
		srv, msg := findServer(rt, word(2))
		if msg != "" {
			return msg
		}
		return setWeight(srv, word(3))
	case f[0] == "get" && word(1) == "weight":
		srv, msg := findServer(rt, word(2))
		if msg != "" {
			return msg
		}
		return fmt.Sprintf("%d (initial %d)\n", srv.st.weight.Load(), srv.st.snapshot().iweight)
	case (f[0] == "enable" || f[0] == "disable") && word(1) == "server":
		srv, msg := findServer(rt, word(2))
		if msg != "" {
			return msg
		}
		if f[0] == "enable" {
			srv.st.setAdmin(spec.StateReady)
		} else {
			srv.st.setAdmin(spec.StateMaint)
		}
		return ""
	}
	return "Unknown command: '" + cmd + "'\n\n" + runtimeHelp
}

// findServer resolves "<backend>/<server>" with HAProxy's error messages.
func findServer(rt *runtime, arg string) (*server, string) {
	bk, sv, ok := strings.Cut(arg, "/")
	if !ok || bk == "" || sv == "" {
		return nil, "Require 'backend/server'.\n"
	}
	be := rt.beByName[bk]
	if be == nil {
		return nil, "No such backend.\n"
	}
	srv := be.srvByName[sv]
	if srv == nil {
		return nil, "No such server.\n"
	}
	return srv, ""
}

func setServer(rt *runtime, args []string) string {
	const usage = "'set server <srv>' only supports 'agent', 'health', 'state', 'weight', 'addr', 'fqdn', 'check-addr', 'check-port', 'ssl' and 'agent-addr'.\n"
	if len(args) == 0 {
		return "Require 'backend/server'.\n"
	}
	srv, msg := findServer(rt, args[0])
	if msg != "" {
		return msg
	}
	if len(args) < 2 {
		return usage
	}
	arg := ""
	if len(args) > 2 {
		arg = args[2]
	}
	switch args[1] {
	case "state":
		switch arg {
		case spec.StateReady, spec.StateDrain, spec.StateMaint:
			srv.st.setAdmin(arg)
			return ""
		}
		return "'set server <srv> state' expects 'ready', 'drain' and 'maint'.\n"
	case "weight":
		return setWeight(srv, arg)
	case "health":
		switch arg {
		case "up":
			srv.st.forceHealth(true)
			return ""
		case "down", "stopping":
			srv.st.forceHealth(false)
			return ""
		}
		return "'set server <srv> health' expects 'up', 'stopping', or 'down'.\n"
	}
	return usage
}

// setWeight applies "<n>" (0–256) or "<n>%" of the initial weight.
func setWeight(srv *server, v string) string {
	if v == "" {
		return "Require <weight> or <weight%>.\n"
	}
	rel := strings.HasSuffix(v, "%")
	n, err := strconv.Atoi(strings.TrimSuffix(v, "%"))
	if err != nil {
		return "Trailing garbage in weight string.\n"
	}
	w := n
	if rel {
		if n < 0 {
			return "Relative weight must be positive.\n"
		}
		w = min(srv.st.snapshot().iweight*n/100, 256)
	} else if n < 0 || n > 256 {
		return "Absolute weight can only be between 0 and 256 inclusive.\n"
	}
	srv.st.setWeight(w)
	return ""
}

// forceHealth implements "set server … health up|down".
func (s *srvState) forceHealth(up bool) {
	s.mu.Lock()
	now := time.Now()
	changed := s.running != up
	if changed {
		s.running, s.lastChange = up, now
		if up {
			s.downtime += now.Sub(s.downSince)
			s.health = s.rise + s.fall - 1
		} else {
			s.downSince, s.health = now, 0
			s.c.chkdown.Add(1)
		}
	}
	s.publishLocked()
	s.mu.Unlock()
	if changed {
		state := "DOWN"
		if up {
			state = "UP"
		}
		s.be.log.warning("Server %s/%s is %s (forced by the runtime API).", s.be.name, s.name, state)
		s.be.changed()
	}
}

// ---------------------------------------------------------------- show stat

// statHeader is HAProxy 3.0's `show stat` column order.
const statHeader = "pxname,svname,qcur,qmax,scur,smax,slim,stot,bin,bout,dreq,dresp,ereq,econ,eresp,wretr,wredis,status,weight,act,bck,chkfail,chkdown,lastchg,downtime,qlimit,pid,iid,sid,throttle,lbtot,tracked,type,rate,rate_lim,rate_max,check_status,check_code,check_duration,hrsp_1xx,hrsp_2xx,hrsp_3xx,hrsp_4xx,hrsp_5xx,hrsp_other,hanafail,req_rate,req_rate_max,req_tot,cli_abrt,srv_abrt,comp_in,comp_out,comp_byp,comp_rsp,lastsess,last_chk,last_agt,qtime,ctime,rtime,ttime,agent_status,agent_code,agent_duration,check_desc,agent_desc,check_rise,check_fall,check_health,agent_rise,agent_fall,agent_health,addr,cookie,mode,algo,conn_rate,conn_rate_max,conn_tot,intercepted,dcon,dses,wrew,connect,reuse,cache_lookups,cache_hits,srv_icur,src_ilim,qtime_max,ctime_max,rtime_max,ttime_max,eint,idle_conn_cur,safe_conn_cur,used_conn_cur,need_conn_est,uweight,agg_server_status,agg_server_check_status,agg_check_status,srid,sess_other,h1sess,h2sess,h3sess,req_other,h1req,h2req,h3req,proto,-,ssl_sess,ssl_reused_sess,ssl_failed_handshake,h2_headers_rcvd,h2_data_rcvd,h2_settings_rcvd,h2_rst_stream_rcvd,h2_goaway_rcvd,h2_detected_conn_protocol_errors,h2_detected_strm_protocol_errors,h2_rst_stream_resp,h2_goaway_resp,h2_open_connections,h2_backend_open_streams,h2_total_connections,h2_backend_total_streams,h1_open_connections,h1_open_streams,h1_total_connections,h1_total_streams,h1_bytes_in,h1_bytes_out,h1_spliced_bytes_in,h1_spliced_bytes_out"

var (
	statCols = strings.Split(statHeader, ",")
	statIdx  = func() map[string]int {
		m := make(map[string]int, len(statCols))
		for i, c := range statCols {
			m[c] = i
		}
		return m
	}()
)

type statRow []string

func newStatRow() statRow { return make(statRow, len(statCols)) }

func (r statRow) set(col, v string) { r[statIdx[col]] = v }

func (r statRow) num(col string, v int64) { r[statIdx[col]] = strconv.FormatInt(v, 10) }

func (r statRow) get(col string) string { return r[statIdx[col]] }

func (r statRow) int(col string) int64 {
	v, _ := strconv.ParseInt(r[statIdx[col]], 10, 64)
	return v
}

func secondsSince(t time.Time, now time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return int64(now.Sub(t) / time.Second)
}

func lastSess(c *counters, now time.Time) int64 {
	v := c.lastSess.Load()
	if v == 0 {
		return -1
	}
	return now.Unix() - v
}

func (r statRow) httpCounters(c *counters, now time.Time, frontend bool) {
	for i, col := range []string{"hrsp_1xx", "hrsp_2xx", "hrsp_3xx", "hrsp_4xx", "hrsp_5xx", "hrsp_other"} {
		r.num(col, c.hrsp[i].Load())
	}
	r.num("req_tot", c.reqTot.Load())
	if frontend {
		r.num("req_rate", c.reqRate.read(now))
		r.num("req_rate_max", c.reqRateMax.Load())
	}
	r.num("comp_in", c.compIn.Load())
	r.num("comp_out", c.compOut.Load())
	r.num("comp_byp", c.compByp.Load())
	r.num("comp_rsp", c.compRsp.Load())
}

func frontendRow(name, mode string, iid, maxConn int, c *counters, now time.Time) statRow {
	r := newStatRow()
	r.set("pxname", name)
	r.set("svname", "FRONTEND")
	r.num("scur", c.scur.Load())
	r.num("smax", c.smax.Load())
	if maxConn > 0 {
		r.num("slim", int64(maxConn))
	}
	r.num("stot", c.stot.Load())
	r.num("bin", c.bin.Load())
	r.num("bout", c.bout.Load())
	r.num("dreq", 0)
	r.num("dresp", 0)
	r.num("ereq", c.ereq.Load())
	r.set("status", "OPEN")
	r.num("pid", 1)
	r.num("iid", int64(iid))
	r.num("sid", 0)
	r.num("type", 0)
	r.num("rate", c.sessRate.read(now))
	r.num("rate_lim", 0)
	r.num("rate_max", c.sessRateMax.Load())
	if mode == spec.ModeHTTP {
		r.httpCounters(c, now, true)
	}
	r.set("mode", mode)
	r.num("conn_rate", c.connRate.read(now))
	r.num("conn_rate_max", c.connRateMax.Load())
	r.num("conn_tot", c.connTot.Load())
	r.num("intercepted", c.intercepted.Load())
	r.num("dcon", 0)
	r.num("dses", 0)
	r.num("wrew", 0)
	return r
}

func backendRow(be *backend, now time.Time) statRow {
	c := &be.st.c
	r := newStatRow()
	r.set("pxname", be.name)
	r.set("svname", "BACKEND")
	r.num("qcur", c.qcur.Load())
	r.num("qmax", c.qmax.Load())
	r.num("scur", c.scur.Load())
	r.num("smax", c.smax.Load())
	r.num("slim", int64(max(be.fullConn, 1)))
	r.num("stot", c.stot.Load())
	r.num("bin", c.bin.Load())
	r.num("bout", c.bout.Load())
	r.num("dreq", 0)
	r.num("dresp", 0)
	r.num("econ", c.econ.Load())
	r.num("eresp", c.eresp.Load())
	r.num("wretr", c.wretr.Load())
	r.num("wredis", c.wredis.Load())
	w := be.totalWeight()
	status := "DOWN"
	if w > 0 || len(be.servers) == 0 {
		status = "UP"
	}
	r.set("status", status)
	r.num("weight", int64(w))
	r.num("act", int64(be.usableCount(false)))
	r.num("bck", int64(be.usableCount(true)))
	_, lastChange, downtime := be.st.status()
	r.num("chkdown", c.chkdown.Load())
	r.num("lastchg", secondsSince(lastChange, now))
	r.num("downtime", int64(downtime/time.Second))
	r.num("pid", 1)
	r.num("iid", int64(be.iid))
	r.num("sid", 0)
	r.num("lbtot", c.lbtot.Load())
	r.num("type", 1)
	r.num("rate", c.sessRate.read(now))
	r.num("rate_max", c.sessRateMax.Load())
	if be.mode == spec.ModeHTTP {
		r.httpCounters(c, now, false)
	}
	r.num("cli_abrt", c.cliAbrt.Load())
	r.num("srv_abrt", c.srvAbrt.Load())
	r.num("lastsess", lastSess(c, now))
	r.num("qtime", c.qtime.avg())
	r.num("ctime", c.ctime.avg())
	r.num("rtime", c.rtime.avg())
	r.num("ttime", c.ttime.avg())
	r.set("mode", be.mode)
	r.set("algo", be.algo)
	r.num("qtime_max", c.qtimeMax.Load())
	r.num("ctime_max", c.ctimeMax.Load())
	r.num("rtime_max", c.rtimeMax.Load())
	r.num("ttime_max", c.ttimeMax.Load())
	r.num("eint", 0)
	uw := 0
	for _, s := range be.actives {
		uw += int(s.st.weight.Load())
	}
	r.num("uweight", int64(uw))
	return r
}

func serverRow(srv *server, now time.Time) statRow {
	be := srv.be
	c := &srv.st.c
	snap := srv.st.snapshot()
	r := newStatRow()
	r.set("pxname", be.name)
	r.set("svname", srv.name)
	r.num("qcur", 0)
	r.num("qmax", 0)
	r.num("scur", c.scur.Load())
	r.num("smax", c.smax.Load())
	r.num("stot", c.stot.Load())
	r.num("bin", c.bin.Load())
	r.num("bout", c.bout.Load())
	r.num("dresp", 0)
	r.num("econ", c.econ.Load())
	r.num("eresp", c.eresp.Load())
	r.num("wretr", c.wretr.Load())
	r.num("wredis", c.wredis.Load())
	r.set("status", snap.status)
	r.num("weight", int64(srv.st.weight.Load()))
	if srv.backup {
		r.num("act", 0)
		r.num("bck", 1)
	} else {
		r.num("act", 1)
		r.num("bck", 0)
	}
	if snap.checkOn {
		r.num("chkfail", c.chkfail.Load())
	}
	r.num("chkdown", c.chkdown.Load())
	r.num("lastchg", secondsSince(snap.lastChange, now))
	r.num("downtime", int64(snap.downtime/time.Second))
	r.num("pid", 1)
	r.num("iid", int64(be.iid))
	r.num("sid", int64(srv.idx+1))
	r.num("lbtot", c.lbtot.Load())
	r.num("type", 2)
	r.num("rate", c.sessRate.read(now))
	r.num("rate_max", c.sessRateMax.Load())
	if snap.checkOn {
		if !snap.checked {
			r.set("check_status", "INI")
			r.set("check_desc", "Initializing")
		} else {
			r.set("check_status", snap.last.Status)
			if snap.last.Code > 0 && strings.HasPrefix(snap.last.Status, "L7") {
				r.num("check_code", int64(snap.last.Code))
			}
			r.num("check_duration", snap.last.Duration.Milliseconds())
			r.set("check_desc", lbcheck.Description(snap.last.Status))
			r.set("last_chk", csvSafe(snap.last.Info))
		}
		r.num("check_rise", int64(snap.rise))
		r.num("check_fall", int64(snap.fall))
		r.num("check_health", int64(snap.health))
	}
	if be.mode == spec.ModeHTTP {
		for i, col := range []string{"hrsp_1xx", "hrsp_2xx", "hrsp_3xx", "hrsp_4xx", "hrsp_5xx", "hrsp_other"} {
			r.num(col, c.hrsp[i].Load())
		}
	}
	r.num("cli_abrt", c.cliAbrt.Load())
	r.num("srv_abrt", c.srvAbrt.Load())
	r.num("lastsess", lastSess(c, now))
	r.num("qtime", c.qtime.avg())
	r.num("ctime", c.ctime.avg())
	r.num("rtime", c.rtime.avg())
	r.num("ttime", c.ttime.avg())
	r.set("addr", srv.dialAddr)
	r.set("cookie", srv.cookie)
	r.set("mode", be.mode)
	r.num("qtime_max", c.qtimeMax.Load())
	r.num("ctime_max", c.ctimeMax.Load())
	r.num("rtime_max", c.rtimeMax.Load())
	r.num("ttime_max", c.ttimeMax.Load())
	r.num("eint", 0)
	if srv.pool != nil {
		r.num("idle_conn_cur", int64(srv.pool.idleCount()))
	}
	r.num("uweight", int64(srv.st.weight.Load()))
	return r
}

func csvSafe(s string) string {
	return strings.NewReplacer(",", ";", "\"", "'", "\n", " ", "\r", " ").Replace(s)
}

// statRows returns every show stat row in HAProxy order: the stats
// frontend, frontends, then each backend's servers followed by its BACKEND row.
func (s *Server) statRows(rt *runtime) []statRow {
	now := time.Now()
	var rows []statRow
	if rt.stats != nil {
		rows = append(rows, frontendRow("stats", spec.ModeHTTP, rt.stats.iid, rt.maxConn, &rt.stats.st.c, now))
	}
	for _, fe := range rt.frontends {
		rows = append(rows, frontendRow(fe.name, fe.mode, fe.iid, fe.maxConn, &fe.st.c, now))
	}
	for _, be := range rt.backends {
		for _, srv := range be.servers {
			rows = append(rows, serverRow(srv, now))
		}
		rows = append(rows, backendRow(be, now))
	}
	return rows
}

func (s *Server) showStat(rt *runtime) string {
	var b strings.Builder
	b.WriteString("# ")
	b.WriteString(statHeader)
	b.WriteString(",\n")
	for _, r := range s.statRows(rt) {
		b.WriteString(strings.Join(r, ","))
		b.WriteString(",\n")
	}
	return b.String()
}

// ---------------------------------------------------------------- show info

// pid is the `show info` Pid: the process id plus the number of successful
// reloads, so it changes on every reload like a new HAProxy worker's.
func (s *Server) pid() int64 { return int64(os.Getpid()) + s.reloads.Load() }

func (s *Server) version() string {
	if s.opts.Version != "" {
		return s.opts.Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "dev"
}

type infoLine struct{ k, v string }

func (s *Server) infoLines(rt *runtime) []infoLine {
	up := time.Since(s.started)
	sec := int64(up / time.Second)
	now := time.Now()
	var sessRate, sessRateMax, connRate, connRateMax int64
	for _, fe := range rt.frontends {
		sessRate += fe.st.c.sessRate.read(now)
		sessRateMax += fe.st.c.sessRateMax.Load()
		connRate += fe.st.c.connRate.read(now)
		connRateMax += fe.st.c.connRateMax.Load()
	}
	stopping := "0"
	if s.stopping.Load() {
		stopping = "1"
	}
	var ms goruntime.MemStats
	goruntime.ReadMemStats(&ms)
	n := strconv.Itoa
	i64 := func(v int64) string { return strconv.FormatInt(v, 10) }
	s.mu.Lock()
	listeners := len(s.listeners)
	s.mu.Unlock()
	return []infoLine{
		{"Name", "Relay Balancer"},
		{"Version", s.version()},
		{"Release_date", ""},
		{"Nbthread", n(goruntime.GOMAXPROCS(0))},
		{"Nbproc", "1"},
		{"Process_num", "1"},
		{"Pid", i64(s.pid())},
		{"Uptime", fmt.Sprintf("%dd %dh%02dm%02ds", sec/86400, sec%86400/3600, sec%3600/60, sec%60)},
		{"Uptime_sec", i64(sec)},
		{"Memmax_MB", "0"},
		{"PoolAlloc_MB", i64(int64(ms.HeapAlloc >> 20))},
		{"PoolUsed_MB", i64(int64(ms.HeapInuse >> 20))},
		{"PoolFailed", "0"},
		{"Maxconn", n(rt.maxConn)},
		{"Hard_maxconn", n(rt.maxConn)},
		{"CurrConns", i64(s.actconn.Load())},
		{"CumConns", i64(s.cumConns.Load())},
		{"CumReq", i64(s.cumReq.Load())},
		{"ConnRate", i64(connRate)},
		{"ConnRateLimit", "0"},
		{"MaxConnRate", i64(connRateMax)},
		{"SessRate", i64(sessRate)},
		{"SessRateLimit", "0"},
		{"MaxSessRate", i64(sessRateMax)},
		{"Tasks", n(goruntime.NumGoroutine())},
		{"Run_queue", "0"},
		{"Idle_pct", "100"},
		{"node", hostname()},
		{"Stopping", stopping},
		{"Jobs", i64(s.actconn.Load() + int64(listeners))},
		{"Unstoppable Jobs", "0"},
		{"Listeners", n(listeners)},
		{"DroppedLogs", strconv.FormatUint(s.log.dropped(), 10)},
		{"Start_time_sec", i64(s.started.Unix())},
		{"Tainted", "0"},
		{"TotalWarnings", "0"},
		{"MaxconnReached", "0"},
		{"Hash", rt.hash},
	}
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

func (s *Server) showInfo(rt *runtime) string {
	var b strings.Builder
	for _, l := range s.infoLines(rt) {
		b.WriteString(l.k)
		b.WriteString(": ")
		b.WriteString(l.v)
		b.WriteByte('\n')
	}
	return b.String()
}
