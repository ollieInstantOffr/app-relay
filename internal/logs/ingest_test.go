package logs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/store"
)

func testApp(t *testing.T) *core.App {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &core.App{Store: st, Bus: events.New(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func accessLine(ts time.Time, hostID, host, method, uri string, status int, reqTime float64, ip, ua string) []byte {
	return []byte(fmt.Sprintf(`{"ts":"%d.%03d","host_id":"%s","host":"%s","method":"%s","uri":"%s","protocol":"HTTP/1.1","scheme":"http",`+
		`"status":"%d","bytes_sent":"100","request_length":"50","request_time":"%.3f","upstream_addr":"10.0.0.1:80","upstream_status":"%d",`+
		`"upstream_connect_time":"0.001","upstream_header_time":"0.002","upstream_response_time":"%.3f","remote_addr":"%s",`+
		`"user_agent":"%s","referer":"-","accept":"*/*","x_forwarded_for":"-","request_id":"rid","ssl_protocol":"-","remote_user":"-"}`,
		ts.Unix(), ts.Nanosecond()/1e6, hostID, host, method, uri, status, reqTime, status, reqTime, ip, ua))
}

func TestIngestQueryAndMetrics(t *testing.T) {
	app := testApp(t)
	ctx := context.Background()
	names := newNameCache()
	names.streams["s1"] = "Minecraft"
	g := newIngester(app, names)

	live, cancel := app.Bus.Subscribe(64)
	defer cancel()

	base := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Minute)
	lines := [][]byte{
		accessLine(base.Add(1*time.Second), "h1", "jellyfin.home.lan", "GET", "/web/index.html", 502, 3.01, "192.168.1.24", "Mozilla"),
		accessLine(base.Add(2*time.Second), "h2", "cloud.home.lan", "PROPFIND", "/remote.php/dav", 200, 0.041, "192.168.1.30", "curl"),
		accessLine(base.Add(3*time.Second), "h2", "cloud.home.lan", "GET", "/index.php", 404, 0.004, "192.168.1.30", "curl"),
		accessLine(base.Add(70*time.Second), "", "scanner.example", "GET", "/.env", 444, 0.0, "45.155.205.233", "zgrab"),
		[]byte(`garbage`),
	}
	g.add(chunk{src: srcAccess, lines: lines, kvKey: kvTailPrefix + "access.log", kvVal: []byte(`{"inode":1,"offset":99}`)})
	g.add(chunk{src: srcStream, lines: [][]byte{[]byte(fmt.Sprintf(`{"ts":"%d.000","stream_id":"s1","protocol":"TCP","remote_addr":"192.168.1.50",`+
		`"server_port":"25565","upstream_addr":"10.0.0.60:25565","status":"200","bytes_sent":"1000","bytes_received":"24","session_time":"12.5",`+
		`"upstream_connect_time":"0.001"}`, base.Add(90*time.Second).Unix()))}})
	g.add(chunk{src: srcNginxError, lines: [][]byte{
		[]byte(base.Format("2006/01/02 15:04:05") + " [error] 7#7: *1 connect() failed (111: Connection refused)"),
		[]byte(base.Format("2006/01/02 15:04:05") + " [notice] 1#1: signal process started"),
	}})
	g.flush(ctx)
	if g.pending() != 0 {
		t.Fatalf("buffers not reset")
	}

	// Offsets are committed with the rows.
	if v, err := app.Store.GetKV(ctx, kvTailPrefix+"access.log"); err != nil || string(v) != `{"inode":1,"offset":99}` {
		t.Fatalf("kv: %s %v", v, err)
	}

	// Live events carry committed ids.
	got := 0
	for done := false; !done; {
		select {
		case ev := <-live:
			if ev.Topic == events.LogLine {
				if e := ev.Data.(core.AccessEntry); e.ID == 0 {
					t.Fatalf("live entry without id: %+v", e)
				}
				got++
			}
		default:
			done = true
		}
	}
	if got != 5 {
		t.Fatalf("live access events = %d, want 5", got)
	}

	q := func(aq core.AccessQuery) []core.AccessEntry {
		t.Helper()
		page, err := queryAccess(ctx, app.Store, aq)
		if err != nil {
			t.Fatal(err)
		}
		return page.Entries
	}
	if all := q(core.AccessQuery{}); len(all) != 5 || all[0].Kind != "stream" || all[0].Host != "Minecraft" {
		t.Fatalf("all: %+v", all)
	}
	if e := q(core.AccessQuery{Status: ">=500"}); len(e) != 1 || e[0].Host != "jellyfin.home.lan" || e[0].UpstreamConnectTime == nil {
		t.Fatalf(">=500: %+v", e)
	}
	if e := q(core.AccessQuery{Host: "*.home.lan", Status: "!200", Kind: "http"}); len(e) != 2 {
		t.Fatalf("suffix !200: %+v", e)
	}
	if e := q(core.AccessQuery{Host: "home.lan"}); len(e) != 0 {
		t.Fatalf("exact host must not match subdomains: %+v", e)
	}
	if e := q(core.AccessQuery{ClientIP: "192.168.1.30", Method: "propfind"}); len(e) != 1 {
		t.Fatalf("ip+method: %+v", e)
	}
	if e := q(core.AccessQuery{Search: ".env"}); len(e) != 1 || e[0].HostID != "" {
		t.Fatalf("search: %+v", e)
	}
	if e := q(core.AccessQuery{Since: base.Add(60 * time.Second)}); len(e) != 2 {
		t.Fatalf("since: %+v", e)
	}
	page, _ := queryAccess(ctx, app.Store, core.AccessQuery{Limit: 2})
	if len(page.Entries) != 2 || page.NextBeforeID != page.Entries[1].ID {
		t.Fatalf("paging: %+v", page)
	}
	next, _ := queryAccess(ctx, app.Store, core.AccessQuery{Limit: 2, BeforeID: page.NextBeforeID})
	if len(next.Entries) != 2 || next.Entries[0].ID >= page.NextBeforeID {
		t.Fatalf("next page: %+v", next)
	}
	if _, err := queryAccess(ctx, app.Store, core.AccessQuery{Status: "bogus"}); err == nil {
		t.Fatal("expected invalid status error")
	}

	// Histogram via SQL (strftime on stored timestamps).
	f, _ := accessFilter(core.AccessQuery{}, maxPageLimit)
	hist, err := app.Store.AccessHistogram(ctx, f, base, time.Minute, 5)
	if err != nil {
		t.Fatal(err)
	}
	if hist[0].Total != 3 || hist[0].S5xx != 1 || hist[0].S4xx != 1 || hist[0].S2xx != 1 || hist[1].Total != 2 || hist[1].S4xx != 1 {
		t.Fatalf("histogram: %+v", hist)
	}

	// Metrics: '' = all HTTP, per host, _unknown, stream:<id>.
	from := base.Unix() / 60
	tot, err := app.Store.MetricsTotal(ctx, store.MetricsSelector{}, from, from+5, true)
	if err != nil {
		t.Fatal(err)
	}
	if tot.Requests != 4 || tot.S5xx != 1 || tot.S4xx != 2 || tot.BytesOut != 400 {
		t.Fatalf("totals: %+v", tot)
	}
	if p, ok := Percentile(tot.Hist, 0.95); !ok || p < 2500 {
		t.Fatalf("p95 = %v (hist %v)", p, tot.Hist)
	}
	withStreams, _ := app.Store.MetricsTotal(ctx, store.MetricsSelector{IncludeStreams: true}, from, from+5, false)
	if withStreams.Requests != 5 || withStreams.BytesOut != 1400 {
		t.Fatalf("with streams: %+v", withStreams)
	}
	unknown, _ := app.Store.MetricsTotal(ctx, store.MetricsSelector{Host: store.MetricsKeyUnknown}, from, from+5, false)
	if unknown.Requests != 1 {
		t.Fatalf("unknown: %+v", unknown)
	}
	series, err := app.Store.MetricsHostSeries(ctx, from, 1, 5, from, from+5)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 2 || series["h2"].Total != 2 || series["h2"].Series[0] != 2 || series["h1"].Total != 1 {
		t.Fatalf("host series: %+v", series)
	}

	// A second flush merges into the same minute rows.
	g.add(chunk{src: srcAccess, lines: [][]byte{accessLine(base.Add(4*time.Second), "h1", "jellyfin.home.lan", "GET", "/", 200, 0.02, "192.168.1.24", "Mozilla")}})
	g.flush(ctx)
	tot2, _ := app.Store.MetricsTotal(ctx, store.MetricsSelector{Host: "h1"}, from, from+5, true)
	var histSum int64
	for _, c := range tot2.Hist {
		histSum += c
	}
	if tot2.Requests != 2 || histSum != 2 {
		t.Fatalf("merged: %+v", tot2)
	}

	// Error log
	errs, err := app.Store.QueryErrors(ctx, store.ErrorFilter{Levels: LevelsAtLeast("error")})
	if err != nil || len(errs) != 1 || errs[0].Message != "connect() failed (111: Connection refused)" {
		t.Fatalf("errors: %+v %v", errs, err)
	}

	// Stream activity
	act, err := app.Store.StreamActivity(ctx, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	if err != nil || act["s1"].Clients != 1 || act["s1"].Bytes != 1024 {
		t.Fatalf("stream activity: %+v %v", act, err)
	}

	// Retention
	removed, err := app.Store.PruneObserve(ctx, time.Now().Add(8*24*time.Hour), store.RetentionPolicy{AccessMaxAge: 7 * 24 * time.Hour, ErrorMaxAge: 7 * 24 * time.Hour})
	if err != nil || removed["access_log"] != 6 || removed["error_log"] != 2 {
		t.Fatalf("prune: %v %v", removed, err)
	}
}

func TestIngestLiveThrottle(t *testing.T) {
	app := testApp(t)
	g := newIngester(app, newNameCache())
	live, cancel := app.Bus.Subscribe(1024)
	defer cancel()
	now := time.Now()
	var lines [][]byte
	for i := 0; i < 450; i++ {
		lines = append(lines, accessLine(now, "h", "a.lan", "GET", fmt.Sprintf("/%d", i), 200, 0.001, "10.0.0.1", "x"))
	}
	g.add(chunk{src: srcAccess, lines: lines})
	g.flush(context.Background())
	if n := len(live); n == 0 || n > liveAccessPerSec {
		t.Fatalf("published %d live events for 450 rows, want 1..%d", n, liveAccessPerSec)
	}
}

func TestParseProcNetTCP(t *testing.T) {
	data := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:63DD 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1000
   1: 0100007F:63DD 3200A8C0:D431 01 00000000:00000000 00:00000000 00000000     0        0 1001
   2: 0100007F:63DD 3300A8C0:D432 01 00000000:00000000 00:00000000 00000000     0        0 1002
   3: 0100007F:1F90 3300A8C0:D433 08 00000000:00000000 00:00000000 00000000     0        0 1003
   4: 00000000000000000000000001000000:01BB 00000000000000000000000001000000:C350 01 00000000:00000000 00:00000000 00000000 0 0 1004
`
	out := map[int]int{}
	parseProcNetTCP(data, out)
	if out[25565] != 2 || out[8080] != 0 || out[443] != 1 {
		t.Fatalf("%v", out)
	}
	if got := parsePorts("2456-2458, 25565,bad,0"); len(got) != 4 || got[0] != 2456 || got[3] != 25565 {
		t.Fatalf("ports: %v", got)
	}
}

func TestNormalizeCIDR(t *testing.T) {
	cases := map[string]string{
		"192.168.1.24":    "192.168.1.24",
		" 10.0.0.0/8 ":    "10.0.0.0/8",
		"10.1.2.3/8":      "10.0.0.0/8",
		"192.168.1.24/32": "192.168.1.24",
		"2001:db8::1":     "2001:db8::1",
		"2001:db8::/32":   "2001:db8::/32",
		"::ffff:1.2.3.4":  "1.2.3.4",
	}
	for in, want := range cases {
		if got, ok := normalizeCIDR(in); !ok || got != want {
			t.Errorf("%q: got %q %v want %q", in, got, ok, want)
		}
	}
	if _, ok := normalizeCIDR("not-an-ip"); ok {
		t.Error("expected invalid")
	}
	if !blockMatches("10.0.0.0/8", "10.2.3.4") || blockMatches("10.0.0.0/8", "11.0.0.1") || !blockMatches("1.2.3.4", "1.2.3.4") {
		t.Error("blockMatches")
	}
}
