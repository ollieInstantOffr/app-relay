package balancer

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/instantoffr/relay/internal/balancer/spec"
	"github.com/instantoffr/relay/internal/lbcheck"
)

// Server flags read on the hot path.
const (
	flagUp    = 1 << iota // running: checks passing (or unchecked) and the address resolved
	flagDrain             // admin drain
	flagMaint             // admin maint
)

// feState is a frontend's state that survives reloads (by name).
type feState struct {
	name string
	c    counters
}

// beState is a backend's state that survives reloads (by name).
type beState struct {
	name string
	c    counters
	log  *logger

	cur     atomic.Pointer[backend] // compiled backend of the active runtime
	version atomic.Uint64           // bumped on every server state or weight change
	stick   atomic.Pointer[stickTable]

	mu         sync.Mutex
	wake       chan struct{}
	up         bool
	lastChange time.Time
	downSince  time.Time
	downtime   time.Duration
}

func newBeState(name string, log *logger) *beState {
	now := time.Now()
	return &beState{name: name, log: log, up: true, lastChange: now}
}

// waitCh returns a channel closed on the next server change.
func (b *beState) waitCh() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.wake == nil {
		b.wake = make(chan struct{})
	}
	return b.wake
}

// changed records a server change: bumps the version, wakes queued
// sessions and tracks the backend UP/DOWN status.
func (b *beState) changed() {
	b.version.Add(1)
	be := b.cur.Load()
	up := be != nil && (len(be.servers) == 0 || be.usableCount(false)+be.usableCount(true) > 0)
	b.mu.Lock()
	if b.wake != nil {
		close(b.wake)
		b.wake = nil
	}
	var alert bool
	if be != nil && up != b.up {
		now := time.Now()
		b.up, b.lastChange = up, now
		if up {
			b.downtime += now.Sub(b.downSince)
		} else {
			b.downSince = now
			b.c.chkdown.Add(1)
			alert = true
		}
	}
	b.mu.Unlock()
	if alert {
		b.log.alert("backend %s has no server available!", b.name)
	}
}

func (b *beState) status() (up bool, lastChange time.Time, downtime time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	d := b.downtime
	if !b.up {
		d += time.Since(b.downSince)
	}
	return b.up, b.lastChange, d
}

// srvState is a server's state that survives reloads for the same backend
// name, server name, address and port: counters, health, admin state and
// runtime weight.
type srvState struct {
	key string
	be  *beState
	c   counters

	flags  atomic.Uint32
	weight atomic.Int32  // current user weight
	kick   chan struct{} // wakes the health checker (leaving maintenance)

	mu         sync.Mutex
	name       string
	cfgState   string // State of the last applied config ("" = never applied)
	cfgWeight  int    // initial weight (HAProxy iweight)
	checkOn    bool
	rise, fall int
	checkGen   uint64
	unresolved bool
	admin      string
	running    bool
	health     int
	lastChange time.Time
	downSince  time.Time
	downtime   time.Duration
	last       lbcheck.Result
	checked    bool
}

func newSrvState(key, name string, be *beState) *srvState {
	return &srvState{key: key, name: name, be: be, kick: make(chan struct{}, 1)}
}

func (s *srvState) usableLB() bool {
	return s.flags.Load() == flagUp && s.weight.Load() > 0
}

func (s *srvState) usablePersist() bool {
	return s.flags.Load()&(flagUp|flagMaint) == flagUp
}

func (s *srvState) maint() bool { return s.flags.Load()&flagMaint != 0 }

// publishLocked refreshes the hot-path flags; s.mu must be held. The caller
// calls s.be.changed() after unlocking.
func (s *srvState) publishLocked() {
	var f uint32
	if s.running && !s.unresolved {
		f |= flagUp
	}
	switch s.admin {
	case spec.StateDrain:
		f |= flagDrain
	case spec.StateMaint:
		f |= flagMaint
	}
	s.flags.Store(f)
}

// applyConfig merges a (re)loaded server definition into the state. The
// config's State and Weight win when they differ from the previously loaded
// config (the renderer changed them); otherwise runtime changes made through
// the runtime API are kept.
func (s *srvState) applyConfig(srv *server) {
	s.mu.Lock()
	now := time.Now()
	first := s.cfgState == ""
	if first {
		s.admin, s.running, s.lastChange = srv.cfgState, true, now
		s.weight.Store(int32(srv.cfgWeight))
		s.health = srv.rise
	} else {
		if srv.cfgState != s.cfgState {
			s.admin = srv.cfgState
			s.lastChange = now
		}
		if srv.cfgWeight != s.cfgWeight {
			s.weight.Store(int32(srv.cfgWeight))
		}
		switch {
		case srv.checkOn && !s.checkOn:
			s.health = srv.rise
			s.checked = false
			if !s.running {
				s.running = true
				s.downtime += now.Sub(s.downSince)
				s.lastChange = now
			}
		case !srv.checkOn && s.checkOn:
			if !s.running {
				s.running = true
				s.downtime += now.Sub(s.downSince)
				s.lastChange = now
			}
		case srv.checkOn:
			if s.running {
				s.health = max(min(s.health, srv.rise+srv.fall-1), srv.rise)
			} else {
				s.health = min(s.health, srv.rise-1)
			}
		}
	}
	s.cfgState, s.cfgWeight = srv.cfgState, srv.cfgWeight
	s.checkOn, s.rise, s.fall = srv.checkOn, srv.rise, srv.fall
	s.unresolved = srv.unresolved
	s.checkGen++
	s.publishLocked()
	s.mu.Unlock()
}

func (s *srvState) gen() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkGen
}

// checkResult applies one health check result (HAProxy's health counter:
// 0 … rise+fall-1, running when ≥ rise).
func (s *srvState) checkResult(gen uint64, srv *server, res lbcheck.Result) {
	s.mu.Lock()
	if gen != s.checkGen || !s.checkOn || s.admin == spec.StateMaint {
		s.mu.Unlock()
		return
	}
	s.last, s.checked = res, true
	now := time.Now()
	var change string
	if res.OK {
		if s.health < s.rise+s.fall-1 {
			s.health++
			if s.health >= s.rise {
				s.health = s.rise + s.fall - 1
			}
		}
		if !s.running && s.health >= s.rise {
			s.running, s.lastChange = true, now
			s.downtime += now.Sub(s.downSince)
			change = "UP"
		}
	} else {
		if s.running {
			s.c.chkfail.Add(1)
		}
		if s.health > s.rise {
			s.health--
		} else {
			s.health = 0
			if s.running {
				s.running, s.lastChange, s.downSince = false, now, now
				s.c.chkdown.Add(1)
				change = "DOWN"
			}
		}
	}
	s.publishLocked()
	s.mu.Unlock()
	if change == "" {
		return
	}
	be := s.be.cur.Load()
	act, bck := 0, 0
	if be != nil {
		act, bck = be.usableCount(false), be.usableCount(true)
	}
	reason := fmt.Sprintf("reason: %s", lbcheck.Description(res.Status))
	if res.Code > 0 {
		reason += fmt.Sprintf(", code: %d", res.Code)
	}
	if res.Info != "" && !res.OK {
		reason += fmt.Sprintf(", info: %q", res.Info)
	}
	reason += fmt.Sprintf(", check duration: %dms", res.Duration.Milliseconds())
	if change == "UP" {
		s.be.log.warning("Server %s/%s is UP, %s. %d active and %d backup servers online. 0 sessions requeued, 0 total in queue.", s.be.name, s.name, reason, act, bck)
	} else {
		s.be.log.warning("Server %s/%s is DOWN, %s. %d active and %d backup servers left. %d sessions active, 0 requeued, %d remaining in queue.", s.be.name, s.name, reason, act, bck, s.c.scur.Load(), s.be.c.qcur.Load())
	}
	s.be.changed()
}

// setAdmin changes the admin state (runtime API).
func (s *srvState) setAdmin(state string) {
	s.mu.Lock()
	prev := s.admin
	if prev == state {
		s.mu.Unlock()
		return
	}
	now := time.Now()
	s.admin, s.lastChange = state, now
	if prev == spec.StateMaint && s.checkOn {
		// Leaving maintenance: up, but the first failed check brings it down.
		s.health = s.rise
		if !s.running {
			s.running = true
			s.downtime += now.Sub(s.downSince)
		}
	}
	s.publishLocked()
	s.mu.Unlock()
	if prev == spec.StateMaint {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
	be := s.be.cur.Load()
	s.be.changed()
	act, bck := 0, 0
	if be != nil {
		act, bck = be.usableCount(false), be.usableCount(true)
	}
	switch {
	case state == spec.StateMaint:
		s.be.log.warning("Server %s/%s is going DOWN for maintenance. %d active and %d backup servers left. %d sessions active, 0 requeued, 0 remaining in queue.", s.be.name, s.name, act, bck, s.c.scur.Load())
	case prev == spec.StateMaint:
		s.be.log.warning("Server %s/%s is UP/READY (leaving forced maintenance).", s.be.name, s.name)
	case state == spec.StateDrain:
		s.be.log.warning("Server %s/%s enters drain state.", s.be.name, s.name)
	default:
		s.be.log.warning("Server %s/%s is UP (leaving forced drain).", s.be.name, s.name)
	}
}

func (s *srvState) setWeight(w int) {
	if int(s.weight.Swap(int32(w))) != w {
		s.be.changed()
	}
}

// snapshot is a consistent copy for show stat.
type srvSnapshot struct {
	status     string
	admin      string
	checkOn    bool
	checked    bool
	last       lbcheck.Result
	rise, fall int
	health     int
	iweight    int
	lastChange time.Time
	downtime   time.Duration
	running    bool
}

func (s *srvState) snapshot() srvSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.downtime
	if !s.running {
		d += time.Since(s.downSince)
	}
	return srvSnapshot{
		status: s.statusLocked(), admin: s.admin, checkOn: s.checkOn, checked: s.checked, last: s.last,
		rise: s.rise, fall: s.fall, health: s.health, iweight: s.cfgWeight, lastChange: s.lastChange, downtime: d,
		running: s.running && !s.unresolved,
	}
}

// statusLocked renders HAProxy's server status string.
func (s *srvState) statusLocked() string {
	if s.admin == spec.StateMaint {
		return "MAINT"
	}
	if s.running && !s.unresolved {
		goingDown := s.checkOn && s.health < s.rise+s.fall-1
		switch {
		case s.admin == spec.StateDrain && goingDown:
			return fmt.Sprintf("DRAIN %d/%d", s.health-s.rise+1, s.fall)
		case s.admin == spec.StateDrain:
			return "DRAIN"
		case !s.checkOn:
			return "no check"
		case goingDown:
			return fmt.Sprintf("UP %d/%d", s.health-s.rise+1, s.fall)
		}
		return "UP"
	}
	if s.checkOn && s.health > 0 && !s.unresolved {
		return fmt.Sprintf("DOWN %d/%d", s.health, s.rise)
	}
	return "DOWN"
}
