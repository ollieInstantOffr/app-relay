package edge

import (
	"net/http"
	"strconv"
	"time"
)

// logRequest records a finished request in the metrics and access.log
// (relay_json keys, in the nginx log_format order).
func (s *Server) logRequest(rs *reqState, body *countingBody) {
	end := time.Now()
	r := rs.r
	status := rs.rw.status
	switch {
	case rs.aborted:
		status = 444
	case rs.rw.wroteHeader:
	case rs.up.status == http.StatusSwitchingProtocols:
		status = http.StatusSwitchingProtocols // hijacked websocket
	case rs.clientGone || r.Context().Err() != nil:
		status = 499
	default:
		status = http.StatusOK
	}
	var bodyBytes int64
	if body != nil {
		bodyBytes = body.n.Load()
	}
	reqLen := requestLength(r) + bodyBytes
	dur := end.Sub(rs.start)
	s.metrics.requests.Add(1)
	if rs.vs != nil && rs.vs.metrics != nil {
		rs.vs.metrics.observe(status, dur, rs.rw.bytes, reqLen)
	}
	sink := s.accessLog.Load()
	if sink == nil {
		return
	}
	var num [32]byte
	b := getLine()
	l := append(*b, '{')
	l = appendRawField(l, "ts", appendMsec(num[:0], end))
	hostID := ""
	if rs.vs != nil {
		hostID = rs.vs.hostID
	}
	l = appendField(l, "host_id", hostID)
	l = appendField(l, "host", rs.host)
	l = appendField(l, "method", r.Method)
	l = appendField(l, "uri", rs.requestURI())
	l = appendField(l, "protocol", r.Proto)
	l = appendField(l, "scheme", rs.role.scheme)
	l = appendRawField(l, "status", strconv.AppendInt(num[:0], int64(status), 10))
	l = appendRawField(l, "bytes_sent", strconv.AppendInt(num[:0], rs.rw.bytes, 10))
	l = appendRawField(l, "request_length", strconv.AppendInt(num[:0], reqLen, 10))
	l = appendRawField(l, "request_time", appendSeconds(num[:0], dur))
	up := &rs.up
	if up.used {
		l = appendField(l, "upstream_addr", up.addr)
		if up.status > 0 {
			l = appendRawField(l, "upstream_status", strconv.AppendInt(num[:0], int64(up.status), 10))
		} else {
			l = appendField(l, "upstream_status", "")
		}
		l = appendTiming(l, "upstream_connect_time", up.connect, up.connected, &num)
		l = appendTiming(l, "upstream_header_time", up.header, up.gotHeader, &num)
		l = appendTiming(l, "upstream_response_time", up.response, true, &num)
	} else {
		for _, k := range [...]string{"upstream_addr", "upstream_status", "upstream_connect_time", "upstream_header_time", "upstream_response_time"} {
			l = appendField(l, k, "")
		}
	}
	h := r.Header
	l = appendField(l, "remote_addr", rs.remoteIP)
	l = appendField(l, "user_agent", headerVariable(h, "User-Agent"))
	l = appendField(l, "referer", headerVariable(h, "Referer"))
	l = appendField(l, "accept", headerVariable(h, "Accept"))
	l = appendField(l, "x_forwarded_for", headerVariable(h, "X-Forwarded-For"))
	l = appendField(l, "request_id", rs.requestID)
	l = appendField(l, "ssl_protocol", rs.sslProtocol())
	l = appendField(l, "remote_user", rs.remoteUserName())
	tunnel, _ := r.Context().Value(tunnelCtxKey{}).(string)
	l = appendField(l, "tunnel", tunnel)
	l = append(l, '}', '\n')
	*b = l
	sink.write(b)
}

func appendTiming(l []byte, key string, d time.Duration, ok bool, num *[32]byte) []byte {
	if !ok {
		return appendField(l, key, "")
	}
	return appendRawField(l, key, appendSeconds(num[:0], d))
}

// requestLength approximates $request_length for the header part: request
// line and header lines as sent on HTTP/1.1.
func requestLength(r *http.Request) int64 {
	n := len(r.Method) + 1 + len(r.RequestURI) + 1 + len(r.Proto) + 2 + 2
	if r.Host != "" {
		n += len("Host: ") + len(r.Host) + 2
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			n += len(k) + 2 + len(v) + 2
		}
	}
	return int64(n)
}

// logStream records a finished stream session.
func (s *Server) logStream(spec *streamRT, st StreamStats) {
	m := s.metrics.stream(spec.id)
	switch st.status() {
	case 200:
		m.ok.Add(1)
	case 502:
		m.connectFailed.Add(1)
	default:
		m.failed.Add(1)
	}
	m.sent.Add(uint64(st.BytesOut))
	m.received.Add(uint64(st.BytesIn))
	if st.Err != nil && s.errlog.enabled(levelError) {
		s.errlog.logf(levelError, "stream %s: %v while connecting to upstream, client: %v, upstream: %q", spec.id, st.Err, st.Client, st.Upstream)
	}
	sink := s.streamLog.Load()
	if sink == nil {
		return
	}
	b := getLine()
	*b = streamLogLine(*b, spec.id, st, time.Now())
	sink.write(b)
}
