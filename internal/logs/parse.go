package logs

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/render/nginx"
	"github.com/instantoffr/relay/internal/store"
)

// Parsing of the nginx access/stream JSON logs (see render/nginx/logformat.go),
// the nginx error log and HAProxy's engine output.

var errNoTimestamp = errors.New("log record has no timestamp")

// value normalises an nginx variable: "-" and blanks become "".
func value(s string) string {
	s = strings.TrimSpace(s)
	if s == "-" {
		return ""
	}
	return s
}

// lastItem returns the final entry of an nginx per-upstream list. Servers
// within a group are separated by ", ", internal redirects by " : ".
func lastItem(s string) string {
	s = strings.TrimSpace(s)
	cut, width := -1, 0
	if i := strings.LastIndex(s, ","); i > cut {
		cut, width = i, 1
	}
	if i := strings.LastIndex(s, " : "); i > cut {
		cut, width = i, 3
	}
	if cut >= 0 {
		s = s[cut+width:]
	}
	return value(s)
}

// optFloat parses the last value of a numeric list; nil for "-".
func optFloat(s string) *float64 {
	v := lastItem(s)
	if v == "" {
		return nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil
	}
	return &f
}

func float(s string) float64 {
	if f := optFloat(s); f != nil {
		return *f
	}
	return 0
}

func integer(s string) int64 {
	v := lastItem(s)
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return int64(f)
	}
	return 0
}

// parseMsec parses nginx $msec ("1726322621.208").
func parseMsec(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	sec, frac, _ := strings.Cut(s, ".")
	secs, err := strconv.ParseInt(sec, 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}, false
	}
	var ms int64
	if frac != "" {
		frac = (frac + "000")[:3]
		if ms, err = strconv.ParseInt(frac, 10, 64); err != nil {
			return time.Time{}, false
		}
	}
	return time.Unix(secs, ms*int64(time.Millisecond)).UTC(), true
}

// ParseAccessLine parses one relay_json access log line.
func ParseAccessLine(line []byte) (store.AccessRecord, error) {
	var rec nginx.AccessLogRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return store.AccessRecord{}, err
	}
	ts, ok := parseMsec(rec.TS)
	if !ok {
		return store.AccessRecord{}, errNoTimestamp
	}
	r := store.AccessRecord{
		TS:                   ts,
		Kind:                 "http",
		HostID:               value(rec.HostID),
		Host:                 strings.ToLower(value(rec.Host)),
		Method:               value(rec.Method),
		Path:                 value(rec.URI),
		Protocol:             value(rec.Protocol),
		Status:               int(integer(rec.Status)),
		ClientIP:             value(rec.RemoteAddr),
		UpstreamAddr:         value(rec.UpstreamAddr),
		UpstreamStatus:       value(rec.UpstreamStatus),
		RequestTime:          float(rec.RequestTime),
		UpstreamConnectTime:  optFloat(rec.UpstreamConnectTime),
		UpstreamHeaderTime:   optFloat(rec.UpstreamHeaderTime),
		UpstreamResponseTime: optFloat(rec.UpstreamResponseTime),
		BytesSent:            integer(rec.BytesSent),
		BytesReceived:        integer(rec.RequestLength),
		UserAgent:            value(rec.UserAgent),
		Referer:              value(rec.Referer),
		RequestID:            value(rec.RequestID),
		SSLProtocol:          value(rec.SSLProtocol),
	}
	extra := map[string]string{}
	for k, v := range map[string]string{
		"accept":          rec.Accept,
		"x_forwarded_for": rec.XForwardedFor,
		"remote_user":     rec.RemoteUser,
		"scheme":          rec.Scheme,
		"request_length":  rec.RequestLength,
	} {
		if v = value(v); v != "" {
			extra[k] = v
		}
	}
	if len(extra) > 0 {
		r.Extra = extra
	}
	return r, nil
}

// ParseStreamLine parses one relay_stream_json log line. Host is left empty
// (the ingester fills in the stream name).
func ParseStreamLine(line []byte) (store.AccessRecord, error) {
	var rec nginx.StreamLogRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return store.AccessRecord{}, err
	}
	ts, ok := parseMsec(rec.TS)
	if !ok {
		return store.AccessRecord{}, errNoTimestamp
	}
	r := store.AccessRecord{
		TS:                  ts,
		Kind:                "stream",
		HostID:              value(rec.StreamID),
		Protocol:            value(rec.Protocol),
		Status:              int(integer(rec.Status)),
		ClientIP:            value(rec.RemoteAddr),
		UpstreamAddr:        value(rec.UpstreamAddr),
		RequestTime:         float(rec.SessionTime),
		UpstreamConnectTime: optFloat(rec.UpstreamConnectTime),
		BytesSent:           integer(rec.BytesSent),
		BytesReceived:       integer(rec.BytesReceived),
	}
	if p := value(rec.ServerPort); p != "" {
		r.Extra = map[string]string{"server_port": p}
	}
	return r, nil
}

var nginxErrorRe = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) \[([a-z]+)\] (?:\d+#\d+: )?(?:\*\d+ )?(.*)$`)

// ParseNginxErrorLine parses "2026/09/14 14:22:07 [emerg] 1#1: bind() …".
// Lines without the standard prefix are kept as info at time now.
func ParseNginxErrorLine(line string, now time.Time) store.ErrorRecord {
	line = strings.TrimRight(line, "\r\n")
	m := nginxErrorRe.FindStringSubmatch(line)
	if m == nil {
		return store.ErrorRecord{TS: now.UTC(), Source: "nginx", Level: "info", Message: strings.TrimSpace(line)}
	}
	ts, err := time.ParseInLocation("2006/01/02 15:04:05", m[1], time.UTC)
	if err != nil {
		ts = now.UTC()
	}
	return store.ErrorRecord{TS: ts, Source: "nginx", Level: NormalizeLevel(m[2]), Message: strings.TrimSpace(m[3])}
}

var haproxyRe = regexp.MustCompile(`^\[([A-Za-z]+)\]\s*(?:\(\d+\)\s*:\s*)?(.*)$`)

// ParseHAProxyLine converts one line of HAProxy engine output. ok is false for
// blank lines.
func ParseHAProxyLine(l agent.LogLine) (store.ErrorRecord, bool) {
	text := strings.TrimSpace(l.Text)
	if text == "" {
		return store.ErrorRecord{}, false
	}
	ts := l.At.UTC()
	if l.At.IsZero() {
		ts = time.Now().UTC()
	}
	rec := store.ErrorRecord{TS: ts, Source: "haproxy", Level: "info", Message: text}
	if m := haproxyRe.FindStringSubmatch(text); m != nil {
		rec.Level = NormalizeLevel(m[1])
		rec.Message = strings.TrimSpace(m[2])
	}
	return rec, true
}

// Levels in decreasing severity.
var Levels = []string{"emerg", "alert", "crit", "error", "warn", "notice", "info", "debug"}

// NormalizeLevel maps nginx/haproxy/syslog level spellings onto Levels.
func NormalizeLevel(l string) string {
	switch strings.ToLower(strings.TrimSpace(l)) {
	case "emerg", "emergency", "panic":
		return "emerg"
	case "alert":
		return "alert"
	case "crit", "critical", "fatal":
		return "crit"
	case "error", "err":
		return "error"
	case "warn", "warning":
		return "warn"
	case "notice":
		return "notice"
	case "debug":
		return "debug"
	}
	return "info"
}

// LevelsAtLeast returns the levels at or above min severity ("warn" → emerg…warn).
func LevelsAtLeast(min string) []string {
	min = NormalizeLevel(min)
	for i, l := range Levels {
		if l == min {
			return append([]string(nil), Levels[:i+1]...)
		}
	}
	return nil
}
