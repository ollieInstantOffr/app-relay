package lb

import (
	"context"
	"fmt"
	"time"

	"github.com/instantoffr/relay/internal/lbcheck"
	"github.com/instantoffr/relay/internal/model"
)

// ProbeResult is the outcome of a "Run health check" request.
type ProbeResult struct {
	OK         bool      `json:"ok"`
	Status     string    `json:"status"` // UP | DOWN
	Check      string    `json:"check"`  // L4OK, L7STS, …
	Detail     string    `json:"detail"`
	LatencyMs  int64     `json:"latencyMs"`
	HTTPStatus int       `json:"httpStatus,omitempty"`
	CheckedAt  time.Time `json:"checkedAt"`
}

// probeServer checks one server directly from Relay with the backend's health
// check type, exactly like HAProxy / Relay Balancer do (internal/lbcheck):
// PROXY v2 LOCAL header for send-proxy backends, TLS for re-encrypting ones.
func probeServer(ctx context.Context, b *model.Backend, srv model.Server) ProbeResult {
	timeout := 5 * time.Second
	if d, err := lbcheck.ParseDuration(b.Timeouts.Connect); err == nil && d > 0 && d < timeout {
		timeout = d
	}
	typ := b.HealthCheck.Type
	if typ == "" || typ == "none" {
		typ = lbcheck.TypeTCP
	}
	start := time.Now()
	r := lbcheck.Run(ctx, lbcheck.Target{
		Address:   srv.Address,
		Port:      srv.Port,
		Type:      typ,
		Method:    b.HealthCheck.Method,
		Path:      b.HealthCheck.Path,
		Host:      b.HealthCheck.Host,
		Expect:    b.HealthCheck.ExpectStatus,
		UserAgent: "Relay-Health-Check",
		User:      "relay", // option pgsql-check user relay
		SendProxy: b.SendProxy,
		TLS:       b.TLSReencrypt,
		TLSVerify: b.TLSVerify,
		Timeout:   timeout,
	})
	res := ProbeResult{OK: r.OK, Check: r.Status, HTTPStatus: r.Code, CheckedAt: start, LatencyMs: r.Duration.Milliseconds(), Status: "DOWN"}
	if r.OK {
		res.Status = "UP"
	}
	res.Detail = fmt.Sprintf("%s · %s · %d ms", r.Status, r.Info, res.LatencyMs)
	return res
}

// statusMatches implements the ExpectStatus syntax: "", "200", "2xx", "200-399", "200,204".
func statusMatches(expect string, code int) bool { return lbcheck.StatusMatches(expect, code) }
