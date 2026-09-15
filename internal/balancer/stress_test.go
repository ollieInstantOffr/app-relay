package balancer

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/balancer/spec"
)

// Many concurrent keep-alive clients against servers that sometimes close
// their connection (like nginx after keepalive_requests): no request may hang.
func TestKeepAliveStressNoHangs(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	var n atomic.Int64
	h := func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1)%97 == 0 {
			w.Header().Set("Connection", "close")
		}
		io.WriteString(w, strings.Repeat("x", 228))
	}
	var servers []spec.Server
	for _, name := range []string{"a", "b", "c"} {
		servers = append(servers, newUpstream(t, name, h).server())
	}
	e := singleBackendEnv(t, func(cfg *spec.Config) {
		cfg.Backends[0].Algorithm = spec.AlgoRandom
		cfg.Timeouts.ServerMs = 4000
		cfg.Timeouts.ClientMs = 4000
	}, servers...)
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 50, MaxConnsPerHost: 50}}
	defer client.CloseIdleConnections()
	const workers, each = 40, 250
	var errs atomic.Int64
	var first atomic.Value
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range each {
				resp, err := client.Get(e.feURL(0, "/"))
				if err != nil {
					errs.Add(1)
					first.CompareAndSwap(nil, err.Error())
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		})
	}
	wg.Wait()
	if errs.Load() > 0 {
		logs := e.logs()
		if len(logs) > 3000 {
			logs = logs[len(logs)-3000:]
		}
		t.Fatalf("%d of %d requests failed, first: %v\nlog tail:\n%s", errs.Load(), workers*each, first.Load(), logs)
	}
}
