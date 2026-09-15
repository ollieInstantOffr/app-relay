package lb

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render/haproxy"
)

// statRow is one line of `show stat` keyed by CSV header name.
type statRow map[string]string

func (r statRow) str(k string) string { return strings.TrimSpace(r[k]) }

func (r statRow) int(k string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(r[k]), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// parseStatCSV parses the output of `show stat` (CSV with a "# " header).
func parseStatCSV(out string) ([]statRow, error) {
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	var header []string
	var rows []statRow
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if header == nil {
			if !strings.HasPrefix(line, "# ") {
				return nil, fmt.Errorf("unexpected show stat output: %q", truncate(line, 120))
			}
			header = strings.Split(strings.TrimPrefix(line, "# "), ",")
			continue
		}
		cols := strings.Split(line, ",")
		row := make(statRow, len(header))
		for i, h := range header {
			if h == "" {
				continue
			}
			if i < len(cols) {
				row[h] = cols[i]
			}
		}
		rows = append(rows, row)
	}
	if header == nil {
		return nil, fmt.Errorf("empty show stat output")
	}
	return rows, nil
}

// parseInfo parses `show info` ("Key: value" lines).
func parseInfo(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, ":"); i > 0 {
			m[strings.TrimSpace(line[:i])] = strings.TrimSpace(line[i+1:])
		}
	}
	return m
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------- live snapshot

type proxyStat struct {
	Name, Server  string
	Status        string // normalised
	RawStatus     string
	Rate, Scur    int64
	Smax, Qcur    int64
	Stot, Lbtot   int64
	Econ, Eresp   int64
	Ereq, Wretr   int64
	Wredis, Rtime int64
	Lastchg       int64
	Weight        int64
	Backup        bool
	CheckStatus   string
	CheckCode     string
	CheckDesc     string
	CheckFall     int64
	CheckDuration int64
	Addr          string
}

type liveStats struct {
	At        time.Time
	Frontends []proxyStat            // FRONTEND rows (excluding stats)
	Backends  []proxyStat            // BACKEND rows
	Servers   map[string][]proxyStat // backend name → server rows in config order
}

func toProxyStat(r statRow) proxyStat {
	return proxyStat{
		Name: r.str("pxname"), Server: r.str("svname"),
		RawStatus: r.str("status"), Status: normStatus(r.str("status")),
		Rate: r.int("rate"), Scur: r.int("scur"), Smax: r.int("smax"), Qcur: r.int("qcur"),
		Stot: r.int("stot"), Lbtot: r.int("lbtot"), Econ: r.int("econ"), Eresp: r.int("eresp"),
		Ereq: r.int("ereq"), Wretr: r.int("wretr"), Wredis: r.int("wredis"), Rtime: r.int("rtime"),
		Lastchg: r.int("lastchg"), Weight: r.int("weight"), Backup: r.int("bck") > 0,
		CheckStatus: strings.TrimSpace(strings.TrimPrefix(r.str("check_status"), "*")),
		CheckCode:   r.str("check_code"), CheckDesc: r.str("check_desc"),
		CheckFall: r.int("check_fall"), CheckDuration: r.int("check_duration"), Addr: r.str("addr"),
	}
}

func parseLive(out string, at time.Time) (*liveStats, error) {
	rows, err := parseStatCSV(out)
	if err != nil {
		return nil, err
	}
	ls := &liveStats{At: at, Servers: map[string][]proxyStat{}}
	for _, r := range rows {
		p := toProxyStat(r)
		switch p.Server {
		case "FRONTEND":
			if p.Name != haproxy.StatsFrontendName {
				ls.Frontends = append(ls.Frontends, p)
			}
		case "BACKEND":
			if p.Name != haproxy.StatsFrontendName {
				ls.Backends = append(ls.Backends, p)
			}
		default:
			if p.Name != haproxy.StatsFrontendName {
				ls.Servers[p.Name] = append(ls.Servers[p.Name], p)
			}
		}
	}
	return ls, nil
}

// normStatus maps haproxy status strings ("UP 1/3", "MAINT (via x/y)", "no check") to
// UP | DOWN | DRAIN | MAINT | NOLB | NOCHECK | OPEN.
func normStatus(s string) string {
	u := strings.ToUpper(strings.TrimSpace(s))
	switch {
	case strings.HasPrefix(u, "MAINT"):
		return "MAINT"
	case strings.HasPrefix(u, "DRAIN"):
		return "DRAIN"
	case strings.HasPrefix(u, "NOLB"):
		return "NOLB"
	case u == "NO CHECK":
		return "NOCHECK"
	case strings.HasPrefix(u, "DOWN"):
		return "DOWN"
	case strings.HasPrefix(u, "UP"):
		return "UP"
	}
	return u
}

// statusClass groups statuses for transition detection.
func statusClass(s string) string {
	switch s {
	case "UP", "DRAIN", "NOLB", "NOCHECK":
		return "up"
	case "DOWN":
		return "down"
	case "MAINT":
		return "maint"
	}
	return ""
}

var checkNames = map[string]string{
	"UNK": "check unknown", "INI": "initializing", "SOCKERR": "socket error",
	"L4OK": "L4 ok", "L4TOUT": "L4 timeout", "L4CON": "connection refused",
	"L6OK": "L6 ok", "L6TOUT": "L6 timeout", "L6RSP": "L6 invalid response",
	"L7OK": "L7 ok", "L7OKC": "L7 ok (conditional)", "L7TOUT": "L7 timeout",
	"L7RSP": "L7 invalid response", "L7STS": "L7 status",
	"PROCERR": "external check error", "PROCTOUT": "external check timeout", "PROCOK": "external check ok",
}

func checkLabel(p proxyStat) string {
	label, ok := checkNames[p.CheckStatus]
	if !ok {
		label = p.CheckStatus
	}
	if p.CheckStatus == "L7STS" && p.CheckCode != "" {
		label += " " + p.CheckCode
	}
	return label
}

// humanDuration renders seconds as "42s", "6 min", "3 h", "41 d".
func humanDuration(sec int64) string {
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%d min", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%d h", sec/3600)
	}
	return fmt.Sprintf("%d d", sec/86400)
}

// checkDetail explains a non-UP server state: "L7 timeout · 3/3 failed · 6 min".
func checkDetail(p proxyStat) string {
	var parts []string
	switch p.Status {
	case "DOWN":
		if p.CheckStatus != "" {
			parts = append(parts, checkLabel(p))
			if p.CheckFall > 0 {
				parts = append(parts, fmt.Sprintf("%d/%d failed", p.CheckFall, p.CheckFall))
			}
		} else if p.CheckDesc != "" {
			parts = append(parts, p.CheckDesc)
		}
		parts = append(parts, humanDuration(p.Lastchg))
	case "MAINT":
		label := "maintenance"
		if i := strings.Index(p.RawStatus, "(via "); i >= 0 {
			label += " " + strings.TrimSuffix(p.RawStatus[i:], "")
		}
		parts = append(parts, label, humanDuration(p.Lastchg))
	case "DRAIN":
		if p.Scur > 0 {
			parts = append(parts, fmt.Sprintf("draining · finishing %d sessions", p.Scur))
		} else {
			parts = append(parts, "draining · idle")
		}
	case "NOLB":
		parts = append(parts, "not load balanced")
	default:
		return ""
	}
	return strings.Join(parts, " · ")
}

// ---------------------------------------------------------------- mapping

// p95Func returns the p95 (ms) of sampled response times for backend/server
// ("" server = backend row).
type p95Func func(backend, server string) int64

func buildStats(ls *liveStats, backends []model.Backend, frontends []model.Frontend, p95 p95Func) *core.LBStats {
	out := &core.LBStats{Running: true, Backends: []core.BackendStats{}, Frontends: []core.FrontendStats{}}
	byName := map[string]*model.Backend{}
	order := map[string]int{}
	for i := range backends {
		byName[backends[i].Name] = &backends[i]
		order[backends[i].Name] = i
	}
	live := append([]proxyStat(nil), ls.Backends...)
	sort.SliceStable(live, func(i, j int) bool {
		oi, iok := order[live[i].Name]
		oj, jok := order[live[j].Name]
		if iok != jok {
			return iok
		}
		return oi < oj
	})

	for _, bp := range live {
		bs := core.BackendStats{
			Name: bp.Name, SessRate: bp.Rate, Current: bp.Scur, Max: bp.Smax, Queue: bp.Qcur,
			Errors: bp.Econ + bp.Eresp, Servers: []core.ServerStats{},
		}
		if p95 != nil {
			bs.RespP95Ms = p95(bp.Name, "")
		}
		mb := byName[bp.Name]
		if mb != nil {
			bs.ID = mb.ID
		}
		servers := ls.Servers[bp.Name]
		var lbTotal int64
		for _, sp := range servers {
			lbTotal += sp.Lbtot
		}
		activeUp, activeDown, activeCount, backupUp := 0, 0, 0, 0
		for _, sp := range servers {
			ss := core.ServerStats{
				Name: sp.Server, Address: sp.Addr, Status: sp.Status, Weight: int(sp.Weight),
				SessRate: sp.Rate, Current: sp.Scur, Max: sp.Smax, Queue: sp.Qcur,
				Errors: sp.Econ + sp.Eresp, RespAvgMs: sp.Rtime, CheckDetail: checkDetail(sp),
				Role: model.ServerActive,
			}
			if sp.Backup {
				ss.Role = model.ServerBackup
			}
			if mb != nil {
				for i := range mb.Servers {
					if serverName(mb, i) == sp.Server {
						ss.ID = mb.Servers[i].ID
						if ss.Address == "" {
							ss.Address = fmt.Sprintf("%s:%d", mb.Servers[i].Address, mb.Servers[i].Port)
						}
						break
					}
				}
			}
			if lbTotal > 0 {
				ss.SharePct = int(math.Round(float64(sp.Lbtot) * 100 / float64(lbTotal)))
			}
			if p95 != nil {
				ss.RespP95Ms = p95(bp.Name, sp.Server)
			}
			switch statusClass(sp.Status) {
			case "up":
				ss.UptimeSec = sp.Lastchg
			case "down":
				ss.DownSec = sp.Lastchg
			}
			if ss.Role == model.ServerBackup {
				if statusClass(sp.Status) == "up" {
					backupUp++
				}
			} else if sp.Status != "MAINT" {
				activeCount++
				switch statusClass(sp.Status) {
				case "up":
					activeUp++
				case "down":
					activeDown++
				}
			}
			bs.Servers = append(bs.Servers, ss)
		}
		switch {
		case bp.Status == "DOWN" || (activeUp == 0 && backupUp == 0):
			bs.Status = "DOWN"
		case activeDown > 0 || (activeCount > 0 && activeUp == 0):
			bs.Status = "DEGRADED"
		default:
			bs.Status = "UP"
		}
		if bs.Status != "DOWN" {
			bs.UptimeSec = bp.Lastchg
		}
		out.SessRate += bp.Rate
		out.Queue += bp.Qcur
		out.Backends = append(out.Backends, bs)
	}

	feByName := map[string]string{}
	for _, f := range frontends {
		feByName[f.Name] = f.ID
	}
	for _, fp := range ls.Frontends {
		out.Frontends = append(out.Frontends, core.FrontendStats{ID: feByName[fp.Name], Name: fp.Name, SessRate: fp.Rate, Current: fp.Scur})
	}
	return out
}

// percentile returns the p-th percentile (0–100) of vals (nearest rank).
func percentile(vals []int64, p float64) int64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]int64(nil), vals...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	rank := int(math.Ceil(p/100*float64(len(s)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(s) {
		rank = len(s) - 1
	}
	return s[rank]
}
