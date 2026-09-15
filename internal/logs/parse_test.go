package logs

import (
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/agent"
)

func TestLastItem(t *testing.T) {
	cases := map[string]string{
		"":                            "",
		"-":                           "",
		"0.012":                       "0.012",
		"0.001, 0.002":                "0.002",
		"0.001, -":                    "",
		"0.001 : 0.004":               "0.004",
		"0.1, 0.2 : 0.3":              "0.3",
		"10.0.0.1:80, 10.0.0.2:80":    "10.0.0.2:80",
		"10.0.0.1:80 : 10.0.0.9:8080": "10.0.0.9:8080",
		"unix:/run/app.sock":          "unix:/run/app.sock",
		"502, 200":                    "200",
	}
	for in, want := range cases {
		if got := lastItem(in); got != want {
			t.Errorf("lastItem(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseMsec(t *testing.T) {
	ts, ok := parseMsec("1726322621.208")
	if !ok || !ts.Equal(time.Unix(1726322621, 208e6)) {
		t.Fatalf("got %v %v", ts, ok)
	}
	if ts, ok := parseMsec("1726322621.2"); !ok || ts.Nanosecond() != 200e6 {
		t.Fatalf("short fraction: %v", ts)
	}
	if _, ok := parseMsec("-"); ok {
		t.Fatal("expected failure for -")
	}
}

func TestParseAccessLine(t *testing.T) {
	line := `{"ts":"1726322621.208","host_id":"h1","host":"Jellyfin.home.lan","method":"GET","uri":"/web/index.html?x=1",` +
		`"protocol":"HTTP/2.0","scheme":"https","status":"502","bytes_sent":"157","request_length":"412","request_time":"3.010",` +
		`"upstream_addr":"10.0.0.41:8096, 10.0.0.42:8096","upstream_status":"502, 502","upstream_connect_time":"0.001, -",` +
		`"upstream_header_time":"-","upstream_response_time":"0.002 : 3.008","remote_addr":"192.168.1.24",` +
		`"user_agent":"Mozilla/5.0 \"quoted\"","referer":"-","accept":"text/html","x_forwarded_for":"-","request_id":"7f2ac19e",` +
		`"ssl_protocol":"TLSv1.3","remote_user":""}`
	r, err := ParseAccessLine([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if !r.TS.Equal(time.Unix(1726322621, 208e6)) || r.Kind != "http" || r.HostID != "h1" || r.Host != "jellyfin.home.lan" {
		t.Fatalf("basic fields: %+v", r)
	}
	if r.Status != 502 || r.Path != "/web/index.html?x=1" || r.BytesSent != 157 || r.BytesReceived != 412 || r.RequestTime != 3.01 {
		t.Fatalf("numbers: %+v", r)
	}
	if r.UpstreamConnectTime != nil || r.UpstreamHeaderTime != nil {
		t.Fatalf("expected nil timings for '-', got %v %v", r.UpstreamConnectTime, r.UpstreamHeaderTime)
	}
	if r.UpstreamResponseTime == nil || *r.UpstreamResponseTime != 3.008 {
		t.Fatalf("response time: %v", r.UpstreamResponseTime)
	}
	if r.UpstreamAddr != "10.0.0.41:8096, 10.0.0.42:8096" || r.Referer != "" || r.UserAgent != `Mozilla/5.0 "quoted"` {
		t.Fatalf("strings: %+v", r)
	}
	if r.Extra["accept"] != "text/html" || r.Extra["scheme"] != "https" || r.Extra["request_length"] != "412" {
		t.Fatalf("extra: %v", r.Extra)
	}
	if _, ok := r.Extra["x_forwarded_for"]; ok {
		t.Fatalf("empty x_forwarded_for must be dropped: %v", r.Extra)
	}
	if _, ok := r.Extra["remote_user"]; ok {
		t.Fatalf("empty remote_user must be dropped: %v", r.Extra)
	}

	if _, err := ParseAccessLine([]byte(`{"host":"x"}`)); err == nil {
		t.Fatal("expected error without ts")
	}
	if _, err := ParseAccessLine([]byte(`not json`)); err == nil {
		t.Fatal("expected error for invalid json")
	}
}

func TestParseStreamLine(t *testing.T) {
	line := `{"ts":"1726322700.000","stream_id":"s1","protocol":"TCP","remote_addr":"192.168.1.50","server_port":"25565",` +
		`"upstream_addr":"10.0.0.60:25565","status":"200","bytes_sent":"1048576","bytes_received":"2048","session_time":"61.250",` +
		`"upstream_connect_time":"0.002"}`
	r, err := ParseStreamLine([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != "stream" || r.HostID != "s1" || r.Protocol != "TCP" || r.Status != 200 || r.BytesSent != 1048576 ||
		r.BytesReceived != 2048 || r.RequestTime != 61.25 || r.Extra["server_port"] != "25565" || *r.UpstreamConnectTime != 0.002 {
		t.Fatalf("stream record: %+v", r)
	}
}

func TestParseNginxErrorLine(t *testing.T) {
	now := time.Date(2026, 9, 14, 15, 0, 0, 0, time.UTC)
	r := ParseNginxErrorLine("2026/09/14 14:22:07 [emerg] 1#1: bind() to 0.0.0.0:443 failed (98: Address already in use)", now)
	if r.Level != "emerg" || r.Source != "nginx" || r.Message != "bind() to 0.0.0.0:443 failed (98: Address already in use)" ||
		!r.TS.Equal(time.Date(2026, 9, 14, 14, 22, 7, 0, time.UTC)) {
		t.Fatalf("%+v", r)
	}
	r = ParseNginxErrorLine(`2026/09/14 14:03:41 [error] 29#29: *5 connect() failed (111: Connection refused) while connecting to upstream`, now)
	if r.Level != "error" || r.Message != "connect() failed (111: Connection refused) while connecting to upstream" {
		t.Fatalf("%+v", r)
	}
	r = ParseNginxErrorLine("2026/09/14 14:03:41 [warn] 1#1: conflicting server name", now)
	if r.Level != "warn" {
		t.Fatalf("%+v", r)
	}
	r = ParseNginxErrorLine("nginx: configuration file /etc/nginx/nginx.conf test is successful", now)
	if r.Level != "info" || !r.TS.Equal(now) {
		t.Fatalf("%+v", r)
	}
}

func TestParseHAProxyLine(t *testing.T) {
	at := time.Date(2026, 9, 14, 13, 58, 0, 0, time.UTC)
	r, ok := ParseHAProxyLine(agent.LogLine{At: at, Stream: "stderr", Text: "[ALERT]    (1) : Binding for frontend pg-in: cannot bind socket (Address already in use) [10.0.0.1:5433]"})
	if !ok || r.Level != "alert" || r.Source != "haproxy" || r.Message != "Binding for frontend pg-in: cannot bind socket (Address already in use) [10.0.0.1:5433]" || !r.TS.Equal(at) {
		t.Fatalf("%+v", r)
	}
	r, _ = ParseHAProxyLine(agent.LogLine{At: at, Text: "[WARNING]  (8) : Server api/api-2 is DOWN"})
	if r.Level != "warn" || r.Message != "Server api/api-2 is DOWN" {
		t.Fatalf("%+v", r)
	}
	r, _ = ParseHAProxyLine(agent.LogLine{At: at, Text: "Proxy web-app started."})
	if r.Level != "info" || r.Message != "Proxy web-app started." {
		t.Fatalf("%+v", r)
	}
	if _, ok := ParseHAProxyLine(agent.LogLine{At: at, Text: "   "}); ok {
		t.Fatal("blank line should be skipped")
	}
}

func TestLevelsAtLeast(t *testing.T) {
	got := LevelsAtLeast("warning")
	want := []string{"emerg", "alert", "crit", "error", "warn"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v", got)
		}
	}
}
