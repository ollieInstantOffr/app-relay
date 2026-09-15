package logs

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/store"
)

func badRequest(msg string) error { return httpx.Errorf(http.StatusBadRequest, "bad_request", msg) }

// accessFilter validates an AccessQuery. Host "*.example.com" or
// ".example.com" matches the domain and its subdomains; anything else is exact.
func accessFilter(q core.AccessQuery, maxLimit int) (store.AccessFilter, error) {
	status, err := ParseStatusFilter(q.Status)
	if err != nil {
		return store.AccessFilter{}, httpx.Errorf(http.StatusBadRequest, "invalid_filter", err.Error())
	}
	f := store.AccessFilter{
		HostID:   strings.TrimSpace(q.HostID),
		Status:   status,
		ClientIP: strings.TrimSpace(q.ClientIP),
		Method:   strings.ToUpper(strings.TrimSpace(q.Method)),
		Search:   strings.TrimSpace(q.Search),
		Since:    q.Since,
		Until:    q.Until,
		BeforeID: q.BeforeID,
		Limit:    q.Limit,
	}
	host := strings.ToLower(strings.TrimSpace(q.Host))
	switch {
	case strings.HasPrefix(host, "*."):
		f.Host, f.HostSuffix = host[2:], true
	case strings.HasPrefix(host, "."):
		f.Host, f.HostSuffix = host[1:], true
	default:
		f.Host = host
	}
	switch k := strings.ToLower(strings.TrimSpace(q.Kind)); k {
	case "", "all":
	case "http", "stream":
		f.Kind = k
	default:
		return f, httpx.Errorf(http.StatusBadRequest, "invalid_filter", "kind must be http or stream")
	}
	if f.Limit <= 0 {
		f.Limit = defaultPageLimit
	}
	if f.Limit > maxLimit {
		f.Limit = maxLimit
	}
	return f, nil
}

func queryAccess(ctx context.Context, st *store.Store, q core.AccessQuery) (*core.AccessPage, error) {
	f, err := accessFilter(q, maxPageLimit)
	if err != nil {
		return nil, err
	}
	recs, err := st.QueryAccess(ctx, f)
	if err != nil {
		return nil, err
	}
	page := &core.AccessPage{Entries: make([]core.AccessEntry, 0, len(recs))}
	for _, r := range recs {
		page.Entries = append(page.Entries, toEntry(r))
	}
	if len(recs) == f.Limit && len(recs) > 0 {
		page.NextBeforeID = recs[len(recs)-1].ID
	}
	return page, nil
}

func toEntry(r store.AccessRecord) core.AccessEntry {
	extra := r.Extra
	if extra == nil {
		extra = map[string]string{}
	}
	return core.AccessEntry{
		ID: r.ID, TS: r.TS, Kind: r.Kind, HostID: r.HostID, Host: r.Host, Method: r.Method, Path: r.Path,
		Protocol: r.Protocol, Status: r.Status, ClientIP: r.ClientIP, UpstreamAddr: r.UpstreamAddr,
		UpstreamStatus: r.UpstreamStatus, RequestTime: r.RequestTime, UpstreamConnectTime: r.UpstreamConnectTime,
		UpstreamHeaderTime: r.UpstreamHeaderTime, UpstreamResponseTime: r.UpstreamResponseTime,
		BytesSent: r.BytesSent, BytesReceived: r.BytesReceived, UserAgent: r.UserAgent, Referer: r.Referer,
		RequestID: r.RequestID, SSLProtocol: r.SSLProtocol, Extra: extra,
	}
}

// accessQueryFrom reads host, hostId, status, ip, method, q, kind, since,
// until, before and limit query parameters.
func accessQueryFrom(r *http.Request, now time.Time) (core.AccessQuery, error) {
	v := r.URL.Query()
	q := core.AccessQuery{
		HostID: v.Get("hostId"), Host: v.Get("host"), Status: v.Get("status"), ClientIP: v.Get("ip"),
		Method: v.Get("method"), Search: v.Get("q"), Kind: v.Get("kind"),
	}
	var err error
	if q.Since, err = ParseSince(v.Get("since"), now); err != nil {
		return q, badRequest(err.Error())
	}
	if q.Until, err = ParseUntil(v.Get("until"), now); err != nil {
		return q, badRequest(err.Error())
	}
	if s := v.Get("before"); s != "" {
		if q.BeforeID, err = strconv.ParseInt(s, 10, 64); err != nil {
			return q, badRequest("before must be a log entry id")
		}
	}
	if s := v.Get("limit"); s != "" {
		if q.Limit, err = strconv.Atoi(s); err != nil {
			return q, badRequest("limit must be a number")
		}
	}
	return q, nil
}

func intParam(r *http.Request, key string, def, lo, hi int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return def
	}
	return max(lo, min(hi, n))
}
