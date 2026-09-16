package gateway

import (
	"bufio"
	"context"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/instantoffr/relay/internal/sniff"
	"github.com/instantoffr/relay/internal/tunnel/wire"
)

// acceptClient admits a client connection against the connection limits and
// serves it with handle on its own goroutine.
func (s *Server) acceptClient(c net.Conn, handle func(net.Conn)) {
	ip, ok := s.admit(c, slotClient)
	if !ok {
		c.Close()
		return
	}
	go func() {
		defer s.release(c, ip, slotClient)
		defer c.Close()
		handle(c)
	}()
}

// serveHTTPS routes a connection by its TLS ClientHello SNI.
func (s *Server) serveHTTPS(c net.Conn, port uint16) {
	s.serveNamed(c, port, wire.KindHTTPS, httpsPeek, sniff.ClientHelloSNI)
}

// serveHTTP routes a connection by the Host header of its first request; the
// home re-checks Host on every later request of the connection.
func (s *Server) serveHTTP(c net.Conn, port uint16) {
	s.serveNamed(c, port, wire.KindHTTP, httpPeek, sniff.HTTPHost)
}

func (s *Server) serveNamed(c net.Conn, port uint16, kind wire.Kind, peek int, inspect func([]byte) (string, bool)) {
	if s.active.Load() == nil {
		s.stats.rejectedPort.Add(1)
		return
	}
	br := bufio.NewReaderSize(c, peek)
	name := wire.NormalizeName(sniffName(c, br, inspect))
	if name == "" || len(name) > wire.MaxNameLen || !s.routes.Load().names.Match(name) {
		s.stats.rejectedName.Add(1)
		return
	}
	s.forward(c, br, wire.StreamHeader{Kind: kind, Port: port, Name: name})
}

// serveTCPPort forwards a connection to a published TCP port.
func (s *Server) serveTCPPort(c net.Conn, port uint16) {
	if !s.routes.Load().tcp[port] {
		s.stats.rejectedPort.Add(1)
		return
	}
	s.forward(c, nil, wire.StreamHeader{Kind: wire.KindTCP, Port: port})
}

// sniffName reads into br until inspect decides, the buffer is full or the
// sniff timeout passes, without consuming anything.
func sniffName(c net.Conn, br *bufio.Reader, inspect func([]byte) (string, bool)) string {
	c.SetReadDeadline(time.Now().Add(sniffTimeout))
	defer c.SetReadDeadline(time.Time{})
	for need := 1; need <= br.Size(); {
		_, err := br.Peek(need)
		if n := br.Buffered(); n > 0 {
			data, _ := br.Peek(n)
			if name, done := inspect(data); done {
				return name
			}
			need = n + 1
		}
		if err != nil {
			return ""
		}
	}
	return ""
}

// forward opens a stream to the home for c, sends the header and the bytes
// already read into br (nil when nothing was sniffed), and pipes both ways.
func (s *Server) forward(c net.Conn, br *bufio.Reader, h wire.StreamHeader) {
	ts := s.active.Load()
	if ts == nil {
		s.stats.rejectedPort.Add(1)
		return
	}
	h.Src, h.Dst = addrPort(c.RemoteAddr()), addrPort(c.LocalAddr())
	buf, err := wire.AppendStreamHeader(nil, h)
	if err != nil {
		s.stats.rejectedName.Add(1)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), openTimeout)
	st, err := ts.sess.OpenStream(ctx)
	cancel()
	if err != nil {
		s.stats.rejectedPort.Add(1)
		s.log.Debug("cannot open tunnel stream", "client", h.Src.String(), "err", err)
		return
	}
	defer st.Close()
	var r io.Reader = c
	peeked := 0
	if br != nil {
		peeked = br.Buffered()
		data, _ := br.Peek(peeked)
		buf = append(buf, data...)
		br.Discard(peeked)
		r = br // empty now: large reads go straight to c
	}
	s.stats.accepted.Add(1)
	s.stats.active.Add(1)
	defer s.stats.active.Add(-1)
	if _, err := st.Write(buf); err != nil {
		return
	}
	s.stats.bytesIn.Add(uint64(peeked))

	// Each direction propagates EOF as a half-close; an error in either
	// direction closes both ends, which also ends the other copy.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := io.Copy(countingWriter{c, &s.stats.bytesOut}, st)
		if err != nil {
			c.Close()
			st.Close()
			return
		}
		if cw, ok := c.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()
	if _, err := io.Copy(countingWriter{st, &s.stats.bytesIn}, r); err != nil {
		c.Close()
		st.Close()
	} else {
		st.CloseWrite()
	}
	<-done
}

// countingWriter counts written bytes into n as they are written.
type countingWriter struct {
	w io.Writer
	n *atomic.Uint64
}

func (cw countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n.Add(uint64(n))
	return n, err
}
