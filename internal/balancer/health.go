package balancer

import (
	"context"
	"math/rand/v2"
	"net/netip"
	"time"

	"github.com/instantoffr/relay/internal/lbcheck"
)

// startChecks runs a health checker per checked server of rt.
func (rt *runtime) startChecks() {
	ctx, cancel := context.WithCancel(context.Background())
	rt.checkCancel = cancel
	for _, be := range rt.backends {
		if be.check == nil {
			continue
		}
		for _, srv := range be.servers {
			if srv.checkOn && !srv.unresolved {
				go srv.checkLoop(ctx, srv.st.gen())
			}
		}
	}
}

func (rt *runtime) stopChecks() {
	if rt.checkCancel != nil {
		rt.checkCancel()
	}
}

func (s *server) checkTarget() lbcheck.Target {
	be := s.be
	hc := be.check
	t := lbcheck.Target{
		Address: s.host, Port: s.port, Type: hc.Type,
		Method: hc.Method, Path: hc.Path, Host: hc.Host, Expect: hc.Expect, User: hc.User,
		SendProxy: be.sendProxy, TLS: be.tlsConf != nil, TLSVerify: be.tlsVerify, RootCAs: be.roots,
		Timeout: be.interval, // HAProxy without "timeout check": inter bounds the whole check
	}
	if ap, err := netip.ParseAddrPort(s.dialAddr); err == nil {
		t.Address, t.Port = ap.Addr().String(), int(ap.Port())
	}
	return t
}

// checkLoop checks the server every interval (paused in maintenance). The
// first check is spread randomly over the first interval, like HAProxy.
func (s *server) checkLoop(ctx context.Context, gen uint64) {
	target := s.checkTarget()
	interval := s.be.interval
	timer := time.NewTimer(time.Duration(rand.Int64N(int64(interval))))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-s.st.kick:
		}
		if !s.st.maint() {
			res := lbcheck.Run(ctx, target)
			if ctx.Err() != nil {
				return
			}
			s.st.checkResult(gen, s, res)
		}
		timer.Reset(interval)
	}
}
