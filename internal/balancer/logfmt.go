package balancer

import (
	"net/http"
	"net/netip"
	"strconv"
	"time"
)

var months = [...]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}

// appendLogDate formats HAProxy's accept date: 15/Sep/2026:14:02:03.123.
func appendLogDate(b []byte, t time.Time) []byte {
	t = t.Local()
	b = append2(b, t.Day())
	b = append(b, '/')
	b = append(b, months[t.Month()-1]...)
	b = append(b, '/')
	b = strconv.AppendInt(b, int64(t.Year()), 10)
	b = append(b, ':')
	b = append2(b, t.Hour())
	b = append(b, ':')
	b = append2(b, t.Minute())
	b = append(b, ':')
	b = append2(b, t.Second())
	b = append(b, '.')
	ms := t.Nanosecond() / 1e6
	b = append(b, byte('0'+ms/100), byte('0'+ms/10%10), byte('0'+ms%10))
	return b
}

func append2(b []byte, v int) []byte { return append(b, byte('0'+v/10%10), byte('0'+v%10)) }

// appendClient formats %ci:%cp.
func appendClient(b []byte, ap netip.AddrPort) []byte {
	if !ap.Addr().IsValid() {
		return append(b, "-:-"...)
	}
	b = ap.Addr().AppendTo(b)
	b = append(b, ':')
	return strconv.AppendUint(b, uint64(ap.Port()), 10)
}

func appendMs(b []byte, d time.Duration) []byte {
	if d < 0 {
		return append(b, "-1"...)
	}
	return strconv.AppendInt(b, d.Milliseconds(), 10)
}

// appendQuoted appends the request line in double quotes, encoding '"', '#'
// and non-printable bytes as #XX like HAProxy.
func appendQuoted(b []byte, parts ...string) []byte {
	const hex = "0123456789ABCDEF"
	b = append(b, '"')
	for i, p := range parts {
		if i > 0 {
			b = append(b, ' ')
		}
		for j := 0; j < len(p); j++ {
			c := p[j]
			if c < 0x20 || c >= 0x7f || c == '"' || c == '#' {
				b = append(b, '#', hex[c>>4], hex[c&0xf])
				continue
			}
			b = append(b, c)
		}
	}
	return append(b, '"')
}

// logRequest writes an `option httplog` line:
//
//	%ci:%cp [%tr] %ft %b/%s %TR/%Tw/%Tc/%Tr/%Ta %ST %B %CC %CS %tsc %ac/%fc/%bc/%sc/%rc %sq/%bq %{+Q}r
func (h *httpHandler) logRequest(r *http.Request, t *httpTxn) {
	s := h.bl.srv
	if !s.log.enabled() {
		return
	}
	b := getLine()
	*b = appendClient(*b, t.client)
	*b = append(*b, " ["...)
	*b = appendLogDate(*b, t.start)
	*b = append(*b, "] "...)
	*b = append(*b, t.fe.name...)
	*b = append(*b, ' ')
	*b = s.appendBackendServer(*b, &t.session)
	*b = append(*b, " 0/"...)
	*b = appendMs(*b, t.tw)
	*b = append(*b, '/')
	*b = appendMs(*b, t.tc)
	*b = append(*b, '/')
	*b = appendMs(*b, t.tr)
	*b = append(*b, '/')
	*b = appendMs(*b, time.Since(t.start))
	*b = append(*b, ' ')
	*b = strconv.AppendInt(*b, int64(t.status), 10)
	*b = append(*b, ' ')
	*b = strconv.AppendInt(*b, t.bytesOut, 10)
	*b = append(*b, " - - "...)
	*b = append(*b, t.term[:]...)
	*b = append(*b, ' ')
	*b = s.appendConnCounts(*b, &t.session)
	*b = append(*b, ' ')
	*b = appendQuoted(*b, r.Method, r.RequestURI, r.Proto)
	*b = append(*b, '\n')
	s.log.traffic(b)
}

// logTCP writes an `option tcplog` line (dontlognull: sessions that
// exchanged nothing and failed nowhere are skipped):
//
//	%ci:%cp [%t] %ft %b/%s %Tw/%Tc/%Tt %B %ts %ac/%fc/%bc/%sc/%rc %sq/%bq
func (s *Server) logTCP(x *session, term [2]byte, bytesIn, bytesOut int64) {
	if !s.log.enabled() || (bytesIn == 0 && x.tc >= 0) {
		return
	}
	b := getLine()
	*b = appendClient(*b, x.client)
	*b = append(*b, " ["...)
	*b = appendLogDate(*b, x.start)
	*b = append(*b, "] "...)
	*b = append(*b, x.fe.name...)
	*b = append(*b, ' ')
	*b = s.appendBackendServer(*b, x)
	*b = append(*b, ' ')
	*b = appendMs(*b, x.tw)
	*b = append(*b, '/')
	*b = appendMs(*b, x.tc)
	*b = append(*b, '/')
	*b = appendMs(*b, time.Since(x.start))
	*b = append(*b, ' ')
	*b = strconv.AppendInt(*b, bytesOut, 10)
	*b = append(*b, ' ')
	*b = append(*b, term[:]...)
	*b = append(*b, ' ')
	*b = s.appendConnCounts(*b, x)
	*b = append(*b, '\n')
	s.log.traffic(b)
}

func (s *Server) appendBackendServer(b []byte, x *session) []byte {
	if x.be != nil {
		b = append(b, x.be.name...)
	} else {
		b = append(b, x.fe.name...)
	}
	b = append(b, '/')
	if x.srv != nil {
		return append(b, x.srv.name...)
	}
	return append(b, "<NOSRV>"...)
}

// appendConnCounts formats %ac/%fc/%bc/%sc/%rc %sq/%bq.
func (s *Server) appendConnCounts(b []byte, x *session) []byte {
	b = strconv.AppendInt(b, s.actconn.Load(), 10)
	b = append(b, '/')
	b = strconv.AppendInt(b, x.fe.st.c.scur.Load(), 10)
	b = append(b, '/')
	var bc, sc, bq int64
	if x.be != nil {
		bc, bq = x.be.st.c.scur.Load(), x.be.st.c.qcur.Load()
	}
	if x.srv != nil {
		sc = x.srv.st.c.scur.Load()
	}
	b = strconv.AppendInt(b, bc, 10)
	b = append(b, '/')
	b = strconv.AppendInt(b, sc, 10)
	b = append(b, '/')
	if x.redispatched {
		b = append(b, '+')
	}
	b = strconv.AppendInt(b, int64(x.retries), 10)
	b = append(b, " 0/"...)
	return strconv.AppendInt(b, bq, 10)
}

// logConnError writes HAProxy's connection error line
// ("%ci:%cp [%tr] %ft/%b: %s").
func (s *Server) logConnError(fc *feConn, fe *frontend, msg string) {
	if !s.log.enabled() {
		return
	}
	b := getLine()
	*b = appendClient(*b, fc.src)
	*b = append(*b, " ["...)
	*b = appendLogDate(*b, time.Now())
	*b = append(*b, "] "...)
	*b = append(*b, fe.name...)
	*b = append(*b, '/')
	*b = append(*b, fe.bind...)
	*b = append(*b, ": "...)
	*b = append(*b, msg...)
	*b = append(*b, '\n')
	s.log.traffic(b)
}
