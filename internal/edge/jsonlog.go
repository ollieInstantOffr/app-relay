package edge

import (
	"net"
	"strconv"
	"time"
	"unicode/utf8"
)

// Helpers for the relay_json / relay_stream_json access log lines. Lines are
// built by hand into pooled buffers: every value is a JSON string, exactly as
// nginx's escape=json formats emit them.

const hexDigits = "0123456789abcdef"

// appendJSONString appends s quoted and escaped. Invalid UTF-8 becomes U+FFFD
// so every line stays valid JSON.
func appendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			b = append(b, s[start:i]...)
			switch c {
			case '"', '\\':
				b = append(b, '\\', c)
			case '\n':
				b = append(b, '\\', 'n')
			case '\r':
				b = append(b, '\\', 'r')
			case '\t':
				b = append(b, '\\', 't')
			default:
				b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b = append(b, s[start:i]...)
			b = append(b, `�`...)
			i++
			start = i
			continue
		}
		i += size
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}

// appendField appends ,"key":"value" (without the comma for the first field).
func appendField(b []byte, key, value string) []byte {
	if len(b) > 0 && b[len(b)-1] != '{' {
		b = append(b, ',')
	}
	b = append(b, '"')
	b = append(b, key...)
	b = append(b, '"', ':')
	return appendJSONString(b, value)
}

// appendRawField appends a value known to need no escaping (numbers).
func appendRawField(b []byte, key string, value []byte) []byte {
	if len(b) > 0 && b[len(b)-1] != '{' {
		b = append(b, ',')
	}
	b = append(b, '"')
	b = append(b, key...)
	b = append(b, '"', ':', '"')
	b = append(b, value...)
	return append(b, '"')
}

// appendMsec formats t like nginx $msec: seconds with millisecond fraction.
func appendMsec(b []byte, t time.Time) []byte {
	ms := t.UnixMilli()
	b = strconv.AppendInt(b, ms/1000, 10)
	return appendFrac3(append(b, '.'), ms%1000)
}

// appendSeconds formats d like nginx $request_time: "0.123".
func appendSeconds(b []byte, d time.Duration) []byte {
	if d < 0 {
		d = 0
	}
	ms := d.Milliseconds()
	b = strconv.AppendInt(b, ms/1000, 10)
	return appendFrac3(append(b, '.'), ms%1000)
}

func appendFrac3(b []byte, v int64) []byte {
	return append(b, byte('0'+v/100), byte('0'+v/10%10), byte('0'+v%10))
}

// streamLogLine renders one relay_stream_json record.
func streamLogLine(b []byte, streamID string, st StreamStats, end time.Time) []byte {
	var num [32]byte
	b = append(b, '{')
	b = appendRawField(b, "ts", appendMsec(num[:0], end))
	b = appendField(b, "stream_id", streamID)
	proto := "TCP"
	if st.Proto == "udp" {
		proto = "UDP"
	}
	b = appendField(b, "protocol", proto)
	client := ""
	if st.Client != nil {
		client = st.Client.String()
		if h, _, err := net.SplitHostPort(client); err == nil {
			client = h
		}
	}
	b = appendField(b, "remote_addr", client)
	b = appendRawField(b, "server_port", strconv.AppendInt(num[:0], int64(st.ListenPort), 10))
	b = appendField(b, "upstream_addr", st.Upstream)
	b = appendRawField(b, "status", strconv.AppendInt(num[:0], int64(st.status()), 10))
	b = appendRawField(b, "bytes_sent", strconv.AppendInt(num[:0], st.BytesOut, 10))
	b = appendRawField(b, "bytes_received", strconv.AppendInt(num[:0], st.BytesIn, 10))
	b = appendRawField(b, "session_time", appendSeconds(num[:0], st.Duration))
	if st.Connected {
		b = appendRawField(b, "upstream_connect_time", appendSeconds(num[:0], st.ConnectTime))
	} else {
		b = appendField(b, "upstream_connect_time", "")
	}
	b = appendField(b, "tunnel", st.Tunnel)
	return append(b, '}', '\n')
}
