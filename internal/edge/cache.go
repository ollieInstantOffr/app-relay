package edge

import (
	"bytes"
	"container/list"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// assetCache is the in-memory equivalent of the nginx relay_assets
// proxy_cache: GET/HEAD static assets, 200/301/302 kept for 30 days unless
// the upstream says otherwise, one fetch per key at a time (proxy_cache_lock)
// and stale entries served on upstream errors or while an update is in
// flight (proxy_cache_use_stale error timeout updating http_5xx). Memory is
// bounded by an LRU over body and header sizes.
type assetCache struct {
	mu      sync.Mutex
	max     int64
	used    int64
	lru     list.List // front = most recent; values are *cacheEntry
	items   map[string]*list.Element
	vary    map[string][]string // primary key → request headers that vary
	flights map[string]chan struct{}

	hits, misses, stale atomic.Uint64
}

type cacheEntry struct {
	key     string
	status  int
	header  http.Header
	body    []byte
	modTime time.Time
	expires time.Time
	size    int64
}

const (
	cacheValid       = 30 * 24 * time.Hour
	cacheMaxObject   = 10 << 20
	cacheLockTimeout = 5 * time.Second
	maxVaryKeys      = 1 << 16
)

func newAssetCache(maxBytes int64) *assetCache {
	return &assetCache{max: maxBytes, items: map[string]*list.Element{}, vary: map[string][]string{}, flights: map[string]chan struct{}{}}
}

func (c *assetCache) resize(maxBytes int64) {
	c.mu.Lock()
	c.max = maxBytes
	c.evict()
	c.mu.Unlock()
}

func (c *assetCache) bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// key is the nginx default proxy_cache_key ($scheme$proxy_host$request_uri)
// plus the values of the request headers the cached response varies on.
func (c *assetCache) key(primary string, h http.Header) string {
	c.mu.Lock()
	names := c.vary[primary]
	c.mu.Unlock()
	return variantKey(primary, names, h)
}

func variantKey(primary string, names []string, h http.Header) string {
	if len(names) == 0 {
		return primary
	}
	var b strings.Builder
	b.WriteString(primary)
	for _, n := range names {
		b.WriteByte(0)
		b.WriteString(strings.Join(h[n], ","))
	}
	return b.String()
}

func (c *assetCache) get(key string, now time.Time) *cacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.items[key]
	if el == nil {
		return nil
	}
	e := el.Value.(*cacheEntry)
	if now.After(e.expires.Add(cacheValid)) { // inactive=30d
		c.remove(el)
		return nil
	}
	c.lru.MoveToFront(el)
	return e
}

func (c *assetCache) put(primary string, varyNames []string, e *cacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.size > c.max {
		return
	}
	if len(varyNames) > 0 {
		if len(c.vary) >= maxVaryKeys {
			clear(c.vary)
		}
		c.vary[primary] = varyNames
	} else {
		delete(c.vary, primary)
	}
	if el := c.items[e.key]; el != nil {
		c.remove(el)
	}
	c.items[e.key] = c.lru.PushFront(e)
	c.used += e.size
	c.evict()
}

// evict drops least recently used entries; c.mu must be held.
func (c *assetCache) evict() {
	for c.used > c.max {
		el := c.lru.Back()
		if el == nil {
			return
		}
		c.remove(el)
	}
}

func (c *assetCache) remove(el *list.Element) {
	e := el.Value.(*cacheEntry)
	c.lru.Remove(el)
	delete(c.items, e.key)
	c.used -= e.size
}

// join returns the flight for key and whether the caller leads it.
func (c *assetCache) join(key string) (chan struct{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch, ok := c.flights[key]; ok {
		return ch, false
	}
	ch := make(chan struct{})
	c.flights[key] = ch
	return ch, true
}

func (c *assetCache) leave(key string, ch chan struct{}) {
	c.mu.Lock()
	delete(c.flights, key)
	c.mu.Unlock()
	close(ch)
}

func (c *assetCache) serve(rs *reqState, req *http.Request, loc *locationRT) {
	primary := rs.role.scheme + "\x00" + loc.up.hostPort + "\x00" + rs.requestURI()
	key := c.key(primary, req.Header)
	e := c.get(key, time.Now())
	if e != nil && time.Now().Before(e.expires) {
		c.hits.Add(1)
		c.write(rs, req, e)
		return
	}
	flight, leader := c.join(key)
	if !leader {
		if e != nil { // updating: serve stale while the leader refreshes
			c.stale.Add(1)
			c.write(rs, req, e)
			return
		}
		timer := time.NewTimer(cacheLockTimeout)
		select {
		case <-flight:
		case <-timer.C:
		case <-req.Context().Done():
		}
		timer.Stop()
		if e := c.get(key, time.Now()); e != nil && time.Now().Before(e.expires) {
			c.hits.Add(1)
			c.write(rs, req, e)
			return
		}
		c.misses.Add(1)
		loc.up.transport.rp.ServeHTTP(&rs.fw, req)
		rs.up.response = time.Since(rs.up.start)
		return
	}
	defer c.leave(key, flight)
	c.misses.Add(1)
	rec := &cacheRecorder{next: &rs.fw, stale: e, base: rs.fw.Header().Clone()}
	rs.cacheFetch = true
	loc.up.transport.rp.ServeHTTP(rec, req)
	rs.up.response = time.Since(rs.up.start)
	if rec.swallow {
		h := rs.fw.Header()
		clear(h)
		for k, v := range rec.base {
			h[k] = v
		}
		c.stale.Add(1)
		c.write(rs, req, e)
		return
	}
	if !rec.store || !rs.up.bodyComplete {
		return
	}
	var varyNames []string
	for _, v := range rec.header["Vary"] {
		for _, n := range strings.Split(v, ",") {
			if n = strings.TrimSpace(n); n != "" {
				varyNames = append(varyNames, http.CanonicalHeaderKey(n))
			}
		}
	}
	now := time.Now()
	entry := &cacheEntry{key: variantKey(primary, varyNames, req.Header), status: rec.status, header: rec.header, body: rec.body, expires: now.Add(rec.ttl)}
	if lm := rec.header.Get("Last-Modified"); lm != "" {
		entry.modTime, _ = http.ParseTime(lm)
	}
	entry.size = int64(len(entry.body)) + int64(len(entry.key)) + 256
	for k, vs := range entry.header {
		for _, v := range vs {
			entry.size += int64(len(k) + len(v))
		}
	}
	c.put(primary, varyNames, entry)
}

// write answers from a cache entry. 200 responses go through ServeContent
// for Range and conditional requests (nginx range and not_modified filters).
func (c *assetCache) write(rs *reqState, req *http.Request, e *cacheEntry) {
	rs.up = upstreamInfo{}
	w := &rs.fw
	h := w.Header()
	for k, vs := range e.header {
		h[k] = vs
	}
	if e.status == http.StatusOK && h.Get("Content-Encoding") == "" {
		http.ServeContent(w, req, "", e.modTime, bytes.NewReader(e.body))
		return
	}
	h["Content-Length"] = []string{strconv.Itoa(len(e.body))}
	w.WriteHeader(e.status)
	w.Write(e.body)
}

// cacheRecorder tees an upstream response into a cache entry.
type cacheRecorder struct {
	next  *filterWriter
	stale *cacheEntry
	base  http.Header // response headers set before proxying

	wrote   bool
	swallow bool // upstream failed and a stale entry answers instead
	store   bool
	status  int
	header  http.Header
	body    []byte
	ttl     time.Duration
}

func (c *cacheRecorder) Header() http.Header { return c.next.Header() }

func (c *cacheRecorder) WriteHeader(code int) {
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		c.next.WriteHeader(code)
		return
	}
	if c.wrote {
		return
	}
	c.wrote, c.status = true, code
	if c.stale != nil && (code == 500 || code == 502 || code == 503 || code == 504) {
		c.swallow = true
		return
	}
	h := c.next.Header()
	c.ttl, c.store = cacheTTL(code, h, time.Now())
	if c.store {
		c.header = make(http.Header, len(h))
		for k, vs := range h {
			if n := len(c.base[k]); len(vs) > n {
				c.header[k] = slices.Clip(slices.Clone(vs[n:]))
			}
		}
		if n, err := strconv.ParseInt(h.Get("Content-Length"), 10, 64); err == nil && n > 0 {
			c.body = make([]byte, 0, n)
		}
	}
	c.next.WriteHeader(code)
}

func (c *cacheRecorder) Write(p []byte) (int, error) {
	if !c.wrote {
		c.WriteHeader(http.StatusOK)
	}
	if c.swallow {
		return len(p), nil
	}
	if c.store {
		if len(c.body)+len(p) > cacheMaxObject {
			c.store, c.body = false, nil
		} else {
			c.body = append(c.body, p...)
		}
	}
	return c.next.Write(p)
}

func (c *cacheRecorder) Flush() {
	if !c.swallow {
		c.next.Flush()
	}
}

func (c *cacheRecorder) Unwrap() http.ResponseWriter { return c.next }

// cacheTTL decides whether a response may be cached and for how long,
// honouring the upstream headers nginx honours.
func cacheTTL(code int, h http.Header, now time.Time) (time.Duration, bool) {
	if code != 200 && code != 301 && code != 302 {
		return 0, false
	}
	if len(h["Set-Cookie"]) > 0 {
		return 0, false
	}
	for _, v := range h["Vary"] {
		if strings.Contains(v, "*") {
			return 0, false
		}
	}
	if n, err := strconv.ParseInt(h.Get("Content-Length"), 10, 64); err == nil && n > cacheMaxObject {
		return 0, false
	}
	if v := h.Get("X-Accel-Expires"); v != "" {
		if s, err := strconv.Atoi(v); err == nil {
			return time.Duration(s) * time.Second, s > 0
		}
	}
	cc := strings.ToLower(strings.Join(h["Cache-Control"], ","))
	if strings.Contains(cc, "private") || strings.Contains(cc, "no-store") || strings.Contains(cc, "no-cache") {
		return 0, false
	}
	maxAge := -1
	for _, d := range strings.Split(cc, ",") {
		name, val, ok := strings.Cut(strings.TrimSpace(d), "=")
		if !ok {
			continue
		}
		if s, err := strconv.Atoi(strings.Trim(val, `"`)); err == nil {
			if name == "s-maxage" || (name == "max-age" && maxAge < 0) {
				maxAge = s
			}
		}
	}
	if maxAge >= 0 {
		return time.Duration(maxAge) * time.Second, maxAge > 0
	}
	if exp := h.Get("Expires"); exp != "" {
		t, err := http.ParseTime(exp)
		if err != nil || !t.After(now) {
			return 0, false
		}
		return t.Sub(now), true
	}
	return cacheValid, true
}
