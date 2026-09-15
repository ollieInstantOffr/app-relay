package edge

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func BenchmarkServerSetLookup(b *testing.B) {
	s := newServerSet()
	for i := range 2000 {
		s.addAll([]string{fmt.Sprintf("host%d.example.com", i), fmt.Sprintf("*.tenant%d.example.net", i)}, &vserver{})
	}
	s.def = &vserver{}
	names := []string{"host1500.example.com", "a.b.tenant42.example.net", "unknown.example.org", "host7.example.com"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.lookup(names[i%len(names)])
	}
}

func BenchmarkNormalizePath(b *testing.B) {
	paths := []string{"/api/v1/users/42", "/static//js/../js/app%20v2.js"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		normalizePath(paths[i%len(paths)])
	}
}

// BenchmarkHandlerProxy measures the full handler (routing, headers, access
// log, proxying to a local upstream over keep-alive) without client-side
// network overhead on the edge side.
func BenchmarkHandlerProxy(b *testing.B) {
	cfg, dir := newConfig(b)
	up := newUpstream(b, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "hello from upstream")
	}))
	h := proxyHost("h", []string{"h.test"}, up.ref())
	h.Locations[0].Headers = []Header{{Name: "X-Client", Value: "$remote_addr $request_id"}}
	h.RateLimit = &RateLimit{RequestsPerSecond: 1 << 30, Burst: 1 << 20}
	cfg.Hosts = []Host{h}
	e := startEnv(b, cfg, dir)
	handler := &listenerHandler{srv: e.srv, role: &listenerRole{kind: "http", scheme: "http", port: 80, portStr: "80"}}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest("GET", "http://h.test/api/items?x=1", nil)
			req.RemoteAddr = "192.0.2.10:40000"
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != 200 {
				b.Fatalf("status %d", rec.Code)
			}
		}
	})
}

// BenchmarkHandlerLocal measures routing and response filters for a request
// answered by Edge itself (default server 404).
func BenchmarkHandlerLocal(b *testing.B) {
	cfg, dir := newConfig(b)
	e := startEnv(b, cfg, dir)
	handler := &listenerHandler{srv: e.srv, role: &listenerRole{kind: "http", scheme: "http", port: 80, portStr: "80"}}
	req := httptest.NewRequest("GET", "http://unknown.test/", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}
}
