package logs

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// Routes registers the observe slice API: dashboard, logs, audit, health (runs behind auth).
func Routes(app *core.App, r chi.Router) {
	h := &handlers{app: app}
	r.Get("/health", h.health)
	r.Post("/health/probe", h.probe)
	r.Get("/logs/access", h.accessList)
	r.Get("/logs/access/histogram", h.accessHistogram)
	r.Get("/logs/access/{id}", h.accessDetail)
	r.Get("/logs/error", h.errorList)
	r.Get("/logs/export", h.export)
	r.Get("/audit", h.auditList)
	r.Get("/audit/export", h.auditExport)
	r.Get("/activity", h.activityList)
	r.Get("/metrics/overview", h.metricsOverview)
	r.Get("/metrics/hosts", h.metricsHosts)
	r.Get("/metrics/streams", h.metricsStreams)
	r.Post("/blocklist", h.blocklistAdd)
	r.Delete("/blocklist/{cidr}", h.blocklistRemove)
}

type handlers struct{ app *core.App }

// ---------------------------------------------------------------- health

func (h *handlers) health(w http.ResponseWriter, r *http.Request) {
	if h.app.Health == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]core.HealthStatus{})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.app.Health.All())
}

func (h *handlers) probe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Upstream model.Upstream `json:"upstream"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if h.app.Health == nil {
		httpx.Fail(w, r, core.ErrNotImplemented)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.app.Health.Probe(r.Context(), body.Upstream))
}

// ---------------------------------------------------------------- access log

func (h *handlers) accessList(w http.ResponseWriter, r *http.Request) {
	q, err := accessQueryFrom(r, time.Now())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	page, err := queryAccess(r.Context(), h.app.Store, q)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

type hostRef struct {
	ID     string `json:"id"`
	Domain string `json:"domain"`
	Kind   string `json:"kind"` // host | stream
}

type accessDetail struct {
	Entry      core.AccessEntry   `json:"entry"`
	SameClient []core.AccessEntry `json:"sameClient"`
	Host       *hostRef           `json:"host"`
	Blocked    bool               `json:"blocked"` // client IP is on the global blocklist
}

func (h *handlers) accessDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		httpx.Fail(w, r, store.ErrNotFound)
		return
	}
	ctx := r.Context()
	st := h.app.Store
	rec, err := st.GetAccess(ctx, id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := accessDetail{Entry: toEntry(*rec), SameClient: []core.AccessEntry{}}
	if rec.ClientIP != "" {
		same, err := st.QueryAccess(ctx, store.AccessFilter{
			ClientIP: rec.ClientIP, Since: rec.TS.Add(-sameClientWindow), Until: rec.TS.Add(sameClientWindow),
			ExcludeID: rec.ID, Limit: sameClientMaxRows,
		})
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		for _, s := range same {
			out.SameClient = append(out.SameClient, toEntry(s))
		}
		out.Blocked = isBlocked(ctx, st, rec.ClientIP)
	}
	if rec.HostID != "" {
		if rec.Kind == "stream" {
			if s, err := st.Streams().Get(ctx, rec.HostID); err == nil {
				out.Host = &hostRef{ID: s.ID, Domain: s.Name, Kind: "stream"}
			}
		} else if ph, err := st.Hosts().Get(ctx, rec.HostID); err == nil {
			domain := ""
			if len(ph.Domains) > 0 {
				domain = ph.Domains[0]
			}
			out.Host = &hostRef{ID: ph.ID, Domain: domain, Kind: "host"}
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

type histogramBucket struct {
	T time.Time `json:"t"`
	store.StatusCounts
}

type histogramResponse struct {
	Buckets     []histogramBucket `json:"buckets"`
	StepSeconds int64             `json:"stepSeconds"`
	Since       time.Time         `json:"since"`
	Until       time.Time         `json:"until"`
}

// accessHistogram counts requests matching the access filters in `buckets`
// equal time slots (2xx/3xx/4xx/5xx). The last slot contains `until`.
func (h *handlers) accessHistogram(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	q, err := accessQueryFrom(r, now)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	n := intParam(r, "buckets", 60, 1, 240)
	since, until := q.Since, q.Until
	if since.IsZero() {
		since = now.Add(-time.Hour)
	}
	if until.IsZero() {
		until = now
	}
	if !until.After(since) {
		httpx.Fail(w, r, badRequest("until must be after since"))
		return
	}
	stepSec := int64(math.Ceil(until.Sub(since).Seconds() / float64(n)))
	if stepSec < 1 {
		stepSec = 1
	}
	step := time.Duration(stepSec) * time.Second
	end := time.Unix((until.Unix()/stepSec+1)*stepSec, 0).UTC()
	start := end.Add(-step * time.Duration(n))

	q.Since, q.Until, q.BeforeID = time.Time{}, time.Time{}, 0
	f, err := accessFilter(q, maxPageLimit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var counts []store.StatusCounts
	unfiltered := f.HostID == "" && f.Host == "" && len(f.Status) == 0 && f.ClientIP == "" && f.Method == "" && f.Search == "" && f.Kind != "stream"
	if unfiltered && stepSec%60 == 0 {
		// Fast path: pre-aggregated per-minute metrics.
		mb, err := h.app.Store.MetricsBuckets(r.Context(), store.MetricsSelector{Host: "", IncludeStreams: f.Kind == ""}, start.Unix()/60, stepSec/60, n, false)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		counts = make([]store.StatusCounts, n)
		for i, m := range mb {
			counts[i] = store.StatusCounts{Total: m.Requests, S3xx: m.S3xx, S4xx: m.S4xx, S5xx: m.S5xx, S2xx: m.Requests - m.S3xx - m.S4xx - m.S5xx}
		}
	} else if counts, err = h.app.Store.AccessHistogram(r.Context(), f, start, step, n); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := histogramResponse{Buckets: make([]histogramBucket, n), StepSeconds: stepSec, Since: start, Until: end}
	for i := range counts {
		out.Buckets[i] = histogramBucket{T: start.Add(step * time.Duration(i)), StatusCounts: counts[i]}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- error log

type errorPage struct {
	Entries      []store.ErrorRecord `json:"entries"`
	NextBeforeID int64               `json:"nextBeforeId"`
}

func errorFilterFrom(r *http.Request, now time.Time) (store.ErrorFilter, error) {
	v := r.URL.Query()
	f := store.ErrorFilter{Source: strings.TrimSpace(v.Get("source")), Search: strings.TrimSpace(v.Get("q"))}
	if lvl := strings.TrimSpace(v.Get("level")); lvl != "" {
		if base, ok := strings.CutSuffix(lvl, "+"); ok {
			f.Levels = LevelsAtLeast(base)
		} else {
			for _, l := range strings.Split(lvl, ",") {
				if l = strings.TrimSpace(l); l != "" {
					f.Levels = append(f.Levels, NormalizeLevel(l))
				}
			}
		}
	}
	var err error
	if f.Since, err = ParseSince(v.Get("since"), now); err != nil {
		return f, badRequest(err.Error())
	}
	if f.Until, err = ParseUntil(v.Get("until"), now); err != nil {
		return f, badRequest(err.Error())
	}
	if s := v.Get("before"); s != "" {
		if f.BeforeID, err = strconv.ParseInt(s, 10, 64); err != nil {
			return f, badRequest("before must be a log entry id")
		}
	}
	f.Limit = intParam(r, "limit", defaultPageLimit, 1, maxPageLimit)
	return f, nil
}

func (h *handlers) errorList(w http.ResponseWriter, r *http.Request) {
	f, err := errorFilterFrom(r, time.Now())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	recs, err := h.app.Store.QueryErrors(r.Context(), f)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	out := errorPage{Entries: recs}
	if len(recs) == f.Limit && len(recs) > 0 {
		out.NextBeforeID = recs[len(recs)-1].ID
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- export

// export streams the access log (default) or, with log=error, the error log
// as CSV or NDJSON, honouring the same filters as the list endpoints.
func (h *handlers) export(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	v := r.URL.Query()
	format := strings.ToLower(v.Get("format"))
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "ndjson" {
		httpx.Fail(w, r, badRequest("format must be csv or ndjson"))
		return
	}
	kind := v.Get("log")
	if kind == "" {
		kind = "access"
	}
	ctx := r.Context()
	st := h.app.Store

	switch kind {
	case "access":
		q, err := accessQueryFrom(r, now)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		f, err := accessFilter(q, exportPageRows)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		f.Limit = exportPageRows
		ew := newExportWriter(w, format, "relay-access-"+now.UTC().Format("20060102-150405"))
		ew.header([]string{"time", "kind", "host", "method", "path", "protocol", "status", "client_ip", "upstream_addr", "upstream_status",
			"request_time", "upstream_connect_time", "upstream_header_time", "upstream_response_time", "bytes_sent", "bytes_received",
			"user_agent", "referer", "request_id", "ssl_protocol"})
		written := 0
		for written < exportMaxRows && ctx.Err() == nil {
			recs, err := st.QueryAccess(ctx, f)
			if err != nil {
				h.app.Log.Warn("observe: export access log", "err", err)
				break
			}
			for _, rec := range recs {
				if format == "ndjson" {
					ew.json(toEntry(rec))
				} else {
					ew.row([]string{store.FormatLogTime(rec.TS), rec.Kind, rec.Host, rec.Method, rec.Path, rec.Protocol, strconv.Itoa(rec.Status),
						rec.ClientIP, rec.UpstreamAddr, rec.UpstreamStatus, ftoa(&rec.RequestTime), ftoa(rec.UpstreamConnectTime),
						ftoa(rec.UpstreamHeaderTime), ftoa(rec.UpstreamResponseTime), strconv.FormatInt(rec.BytesSent, 10),
						strconv.FormatInt(rec.BytesReceived, 10), rec.UserAgent, rec.Referer, rec.RequestID, rec.SSLProtocol})
				}
			}
			written += len(recs)
			ew.flush()
			if len(recs) < f.Limit {
				break
			}
			f.BeforeID = recs[len(recs)-1].ID
		}
	case "error":
		f, err := errorFilterFrom(r, now)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		f.Limit = exportPageRows
		ew := newExportWriter(w, format, "relay-errors-"+now.UTC().Format("20060102-150405"))
		ew.header([]string{"time", "source", "level", "message"})
		written := 0
		for written < exportMaxRows && ctx.Err() == nil {
			recs, err := st.QueryErrors(ctx, f)
			if err != nil {
				h.app.Log.Warn("observe: export error log", "err", err)
				break
			}
			for _, rec := range recs {
				if format == "ndjson" {
					ew.json(rec)
				} else {
					ew.row([]string{store.FormatLogTime(rec.TS), rec.Source, rec.Level, rec.Message})
				}
			}
			written += len(recs)
			ew.flush()
			if len(recs) < f.Limit {
				break
			}
			f.BeforeID = recs[len(recs)-1].ID
		}
	default:
		httpx.Fail(w, r, badRequest("log must be access or error"))
	}
}

func ftoa(f *float64) string {
	if f == nil {
		return ""
	}
	return strconv.FormatFloat(*f, 'f', 3, 64)
}

type exportWriter struct {
	w      http.ResponseWriter
	format string
	csv    *csv.Writer
	enc    *json.Encoder
}

func newExportWriter(w http.ResponseWriter, format, name string) *exportWriter {
	ew := &exportWriter{w: w, format: format}
	if format == "ndjson" {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.ndjson"`, name))
		ew.enc = json.NewEncoder(w)
		ew.enc.SetEscapeHTML(false)
	} else {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, name))
		ew.csv = csv.NewWriter(w)
	}
	w.WriteHeader(http.StatusOK)
	return ew
}

func (e *exportWriter) header(cols []string) {
	if e.csv != nil {
		_ = e.csv.Write(cols)
	}
}

func (e *exportWriter) row(cols []string) {
	if e.csv != nil {
		_ = e.csv.Write(cols)
	}
}

func (e *exportWriter) json(v any) {
	if e.enc != nil {
		_ = e.enc.Encode(v)
	}
}

func (e *exportWriter) flush() {
	if e.csv != nil {
		e.csv.Flush()
	}
	if f, ok := e.w.(http.Flusher); ok {
		f.Flush()
	}
}

// ---------------------------------------------------------------- audit & activity

func auditQueryFrom(r *http.Request, now time.Time) (store.AuditQuery, error) {
	v := r.URL.Query()
	q := store.AuditQuery{Search: strings.TrimSpace(v.Get("q")), ActorType: v.Get("actorType"), Actor: v.Get("actor")}
	var err error
	if q.Since, err = ParseSince(v.Get("since"), now); err != nil {
		return q, badRequest(err.Error())
	}
	if s := v.Get("before"); s != "" {
		if q.BeforeID, err = strconv.ParseInt(s, 10, 64); err != nil {
			return q, badRequest("before must be an audit entry id")
		}
	}
	q.Limit = intParam(r, "limit", 100, 1, 1000)
	return q, nil
}

func (h *handlers) auditList(w http.ResponseWriter, r *http.Request) {
	q, err := auditQueryFrom(r, time.Now())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	rows, err := h.app.Store.ListAudit(r.Context(), q)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rows)
}

func (h *handlers) auditExport(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	q, err := auditQueryFrom(r, now)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if f := r.URL.Query().Get("format"); f != "" && f != "csv" {
		httpx.Fail(w, r, badRequest("format must be csv"))
		return
	}
	q.Limit = 1000
	ew := newExportWriter(w, "csv", "relay-audit-"+now.UTC().Format("20060102-150405"))
	ew.header([]string{"time", "actor_type", "actor", "action", "target", "detail", "version", "result", "ip"})
	written := 0
	for written < exportMaxRows && r.Context().Err() == nil {
		rows, err := h.app.Store.ListAudit(r.Context(), q)
		if err != nil {
			h.app.Log.Warn("observe: export audit log", "err", err)
			break
		}
		for _, a := range rows {
			version := ""
			if a.Version != nil {
				version = "v" + strconv.FormatInt(*a.Version, 10)
			}
			ew.row([]string{a.At.UTC().Format(time.RFC3339), a.ActorType, a.ActorName, a.Action, a.Target, a.Detail, version, a.Result, a.IP})
		}
		written += len(rows)
		ew.flush()
		if len(rows) < q.Limit {
			break
		}
		q.BeforeID = rows[len(rows)-1].ID
	}
}

func (h *handlers) activityList(w http.ResponseWriter, r *http.Request) {
	rows, err := h.app.Store.ListActivity(r.Context(), intParam(r, "limit", 20, 1, 500))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rows)
}

// ---------------------------------------------------------------- blocklist

func normalizeCIDR(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return "", false
		}
		p = p.Masked()
		if p.Addr().Is4In6() {
			p = netip.PrefixFrom(p.Addr().Unmap(), max(0, p.Bits()-96)).Masked()
		}
		if p.Bits() == p.Addr().BitLen() {
			return p.Addr().String(), true
		}
		return p.String(), true
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return "", false
	}
	return a.Unmap().String(), true
}

func blockMatches(entry, ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return entry == ip
	}
	addr = addr.Unmap()
	if p, err := netip.ParsePrefix(entry); err == nil {
		return p.Contains(addr)
	}
	if a, err := netip.ParseAddr(entry); err == nil {
		return a.Unmap() == addr
	}
	return false
}

func isBlocked(ctx context.Context, st *store.Store, ip string) bool {
	bl, err := store.LoadSettings[model.BlocklistSettings](ctx, st, model.SettingsBlocklist)
	if err != nil {
		return false
	}
	for _, e := range bl.Entries {
		if blockMatches(e.CIDR, ip) {
			return true
		}
	}
	return false
}

func (h *handlers) blocklistAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CIDR string `json:"cidr"`
		Note string `json:"note"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	cidr, ok := normalizeCIDR(body.CIDR)
	if !ok {
		httpx.Fail(w, r, &model.ValidationError{Fields: map[string]string{"cidr": "Not a valid IP address or CIDR"}})
		return
	}
	ctx := r.Context()
	st := h.app.Store
	bl, err := store.LoadSettings[model.BlocklistSettings](ctx, st, model.SettingsBlocklist)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	for _, e := range bl.Entries {
		if existing, ok := normalizeCIDR(e.CIDR); ok && existing == cidr {
			httpx.Fail(w, r, httpx.Errorf(http.StatusConflict, "conflict", cidr+" is already blocked"))
			return
		}
	}
	note := strings.TrimSpace(body.Note)
	bl.Entries = append(bl.Entries, model.BlockEntry{CIDR: cidr, Note: note, CreatedAt: time.Now().UTC(), CreatedBy: httpx.Actor(r).Label()})
	if err := st.PutSettings(ctx, model.SettingsBlocklist, bl); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.app.Audit(ctx, core.AuditEntry{Action: "blocklist.add", Target: cidr, Detail: note})
	h.app.Changed(ctx, "settings", model.SettingsBlocklist, model.SettingsBlocklist, core.ActionUpdated)
	httpx.WriteJSON(w, http.StatusOK, bl)
}

func (h *handlers) blocklistRemove(w http.ResponseWriter, r *http.Request) {
	raw, err := url.PathUnescape(chi.URLParam(r, "cidr"))
	if err != nil {
		httpx.Fail(w, r, badRequest("invalid address"))
		return
	}
	target, ok := normalizeCIDR(raw)
	if !ok {
		target = strings.TrimSpace(raw)
	}
	ctx := r.Context()
	st := h.app.Store
	bl, err := store.LoadSettings[model.BlocklistSettings](ctx, st, model.SettingsBlocklist)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	kept := make([]model.BlockEntry, 0, len(bl.Entries))
	removed := false
	for _, e := range bl.Entries {
		n, ok := normalizeCIDR(e.CIDR)
		if e.CIDR == target || (ok && n == target) {
			removed = true
			continue
		}
		kept = append(kept, e)
	}
	if !removed {
		httpx.Fail(w, r, store.ErrNotFound)
		return
	}
	bl.Entries = kept
	if err := st.PutSettings(ctx, model.SettingsBlocklist, bl); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.app.Audit(ctx, core.AuditEntry{Action: "blocklist.remove", Target: target})
	h.app.Changed(ctx, "settings", model.SettingsBlocklist, model.SettingsBlocklist, core.ActionUpdated)
	httpx.WriteJSON(w, http.StatusOK, bl)
}
