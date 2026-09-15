package balancer

import (
	"context"
	"net/netip"
	"time"
)

// session is the server side of one HTTP request or TCP connection: server
// assignment, queueing, connection retries and redispatch.
type session struct {
	start        time.Time
	fe           *frontend
	be           *backend
	srv          *server
	client, dst  netip.AddrPort
	tw, tc       time.Duration // -1 = not reached
	retries      int
	redispatched bool
}

// setServer assigns srv; lb reports a load-balancing decision (lbtot).
func (x *session) setServer(srv *server, lb bool) {
	now := time.Now()
	if x.srv != nil {
		x.srv.st.c.sessionEnd()
	}
	x.srv = srv
	srv.st.c.sessionStart(now)
	if lb {
		srv.st.c.lbtot.Add(1)
		x.be.st.c.lbtot.Add(1)
	}
}

// release ends the server session.
func (x *session) release() {
	if x.srv != nil {
		x.srv.st.c.sessionEnd()
	}
}

// assign picks a server. Servers have no connection limit, so there is
// nothing to queue behind: like HAProxy, a request or session that finds no
// usable server fails at once (503 / closed connection).
func (x *session) assign(_ context.Context, pc *pickCtx) bool {
	x.tw = 0
	if s := x.be.pick(pc); s != nil {
		x.setServer(s, true)
		return true
	}
	return false
}

// connect returns a connection to the assigned server. idle (optional)
// supplies a reusable connection. Failed connections are retried up to the
// backend's retries; with redispatch the last retry goes to another server.
func (x *session) connect(ctx context.Context, pc *pickCtx, idle func(*server) *sconn) (sc *sconn, reused bool, err error) {
	be := x.be
	start := time.Now()
	for {
		srv := x.srv
		if idle != nil {
			if sc = idle(srv); sc != nil {
				x.tc = 0
				return sc, true, nil
			}
		}
		attempt := time.Now()
		sc, err = srv.dial(ctx, x.client, x.dst)
		if err == nil {
			x.tc = time.Since(start)
			return sc, false, nil
		}
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		if x.retries >= be.retries {
			srv.st.c.econ.Add(1)
			be.st.c.econ.Add(1)
			x.tc = -1
			return nil, false, err
		}
		x.retries++
		srv.st.c.wretr.Add(1)
		be.st.c.wretr.Add(1)
		// Turn-around timer: don't hammer a server that refuses immediately.
		if turn := min(be.connectTimeout, time.Second) - time.Since(attempt); turn > 0 {
			t := time.NewTimer(turn)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return nil, false, ctx.Err()
			}
		}
		if be.redispatch && x.retries == be.retries {
			pc.exclude = srv
			if next := be.pick(pc); next != nil && next != srv {
				srv.st.c.wredis.Add(1)
				be.st.c.wredis.Add(1)
				x.redispatched = true
				x.setServer(next, true)
			}
			pc.exclude = nil
		}
	}
}
