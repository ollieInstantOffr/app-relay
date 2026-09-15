package balancer

import (
	"context"
	"time"

	"github.com/instantoffr/relay/internal/balancer/spec"
)

// handleTCP serves one tcp-mode session: optional SNI inspection, rules,
// server assignment, connection and a bidirectional copy.
func (bl *boundListener) handleTCP(fc *feConn, fe *frontend) {
	now := time.Now()
	x := &session{start: now, fe: fe, client: fc.src, dst: fc.dst, tw: -1, tc: -1}
	term := [2]byte{'-', '-'}
	var bytesIn, bytesOut int64
	defer func() { bl.srv.logTCP(x, term, bytesIn, bytesOut) }()

	sni := ""
	if fe.needSNI {
		sni = peekSNI(fc, fe.inspectDelay)
	}
	be := fe.routeTCP(fc.src.Addr(), sni)
	if be == nil {
		term = [2]byte{'P', 'R'}
		return
	}
	x.be = be
	be.st.c.sessionStart(now)
	defer be.st.c.sessionEnd()
	defer x.release()
	ctx := context.Background()

	pc := pickCtx{client: fc.src.Addr()}
	var persist *server
	tbl := (*stickTable)(nil)
	if be.sticky != nil && be.sticky.Mode == spec.StickySource {
		tbl = be.st.stick.Load()
	}
	if tbl != nil {
		if name, ok := tbl.get(fc.src.Addr(), now); ok {
			if srv := be.srvByName[name]; srv != nil && srv.st.usablePersist() {
				persist = srv
			}
		}
	}
	if persist != nil {
		x.tw = 0
		x.setServer(persist, false)
	} else if !x.assign(ctx, &pc) {
		term = [2]byte{'S', 'C'}
		return
	}
	sc, _, err := x.connect(ctx, &pc, nil)
	if err != nil {
		term = [2]byte{'S', 'C'}
		if isTimeout(err) {
			term[0] = 's'
		}
		return
	}
	defer sc.close()
	if tbl != nil {
		tbl.put(fc.src.Addr(), x.srv.name, now)
	}
	srv := x.srv
	sc.raw.off.Store(true)
	sc.raw.Conn.SetDeadline(time.Time{})
	res := pipe(
		pipeEnd{conn: fc, r: fc, timeout: fe.clientTimeout, closeW: func() { fc.CloseWrite() }},
		pipeEnd{conn: sc.conn, r: sc.conn, timeout: be.serverTimeout, closeW: sc.closeWrite},
		func(n int) { fe.st.c.bin.Add(int64(n)); be.st.c.bin.Add(int64(n)); srv.st.c.bin.Add(int64(n)) },
		func(n int) { fe.st.c.bout.Add(int64(n)); be.st.c.bout.Add(int64(n)); srv.st.c.bout.Add(int64(n)) },
	)
	bytesIn, bytesOut = res.up, res.down
	if res.term != '-' {
		term = [2]byte{res.term, 'D'}
		switch res.term {
		case 'C', 'c':
			be.st.c.cliAbrt.Add(1)
			srv.st.c.cliAbrt.Add(1)
		case 'S', 's':
			be.st.c.srvAbrt.Add(1)
			srv.st.c.srvAbrt.Add(1)
		}
	}
	total := time.Since(x.start)
	be.st.c.timers(x.tw, x.tc, -1, total)
	srv.st.c.timers(x.tw, x.tc, -1, total)
}

// peekSNI waits up to delay for a complete TLS ClientHello without consuming
// it (tcp-request inspect-delay + req.ssl_sni).
func peekSNI(fc *feConn, delay time.Duration) string {
	br := fc.br
	fc.Conn.SetReadDeadline(time.Now().Add(delay))
	defer fc.Conn.SetReadDeadline(time.Time{})
	for need := 1; need <= br.Size(); {
		data, err := br.Peek(need)
		if len(data) > 0 {
			all, _ := br.Peek(br.Buffered())
			if sni, done := parseClientHelloSNI(all); done {
				return sni
			}
			need = br.Buffered() + 1
		}
		if err != nil {
			return ""
		}
	}
	return ""
}
