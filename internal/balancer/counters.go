package balancer

import (
	"sync"
	"sync/atomic"
	"time"
)

// freqCtr counts events per second like HAProxy's freq_ctr: the rate is the
// current second's count plus the previous second's count weighted by the
// part of the second not elapsed yet.
type freqCtr struct {
	sec  atomic.Int64
	cur  atomic.Int64
	prev atomic.Int64
}

func (f *freqCtr) rotate(sec int64) {
	for {
		s := f.sec.Load()
		if s >= sec {
			return
		}
		if f.sec.CompareAndSwap(s, sec) {
			c := f.cur.Swap(0)
			if sec == s+1 {
				f.prev.Store(c)
			} else {
				f.prev.Store(0)
			}
			return
		}
	}
}

// add counts one event and returns the rate.
func (f *freqCtr) add(now time.Time) int64 {
	f.rotate(now.Unix())
	f.cur.Add(1)
	return f.read(now)
}

func (f *freqCtr) read(now time.Time) int64 {
	f.rotate(now.Unix())
	return f.cur.Load() + f.prev.Load()*int64(1e9-now.Nanosecond())/1e9
}

func raiseMax(m *atomic.Int64, v int64) {
	for {
		cur := m.Load()
		if v <= cur || m.CompareAndSwap(cur, v) {
			return
		}
	}
}

// swrateSamples is HAProxy's TIME_STATS_SAMPLES.
const swrateSamples = 1024

// swrate is HAProxy's sliding window average over the last 1024 samples
// (swrate_add / swrate_avg; the exact mean while fewer samples were seen).
type swrate struct {
	mu  sync.Mutex
	sum uint64
	n   uint64
}

func (s *swrate) add(v int64) {
	if v < 0 {
		v = 0
	}
	s.mu.Lock()
	if s.n < swrateSamples {
		s.n++ // exact mean until the window is full
		s.sum += uint64(v)
	} else {
		s.sum = s.sum - (s.sum+s.n-1)/s.n + uint64(v)
	}
	s.mu.Unlock()
}

func (s *swrate) avg() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n == 0 {
		return 0
	}
	return int64(s.sum / s.n)
}

// counters are the show stat counters of a frontend, backend or server.
// They survive reloads for proxies and servers that still exist.
type counters struct {
	scur, smax, stot     atomic.Int64
	qcur, qmax           atomic.Int64
	bin, bout            atomic.Int64
	dreq, dresp          atomic.Int64
	ereq, econ, eresp    atomic.Int64
	wretr, wredis        atomic.Int64
	cliAbrt, srvAbrt     atomic.Int64
	lbtot                atomic.Int64
	reqTot, connTot      atomic.Int64
	intercepted          atomic.Int64
	hrsp                 [6]atomic.Int64 // 1xx … 5xx, other
	compIn, compOut      atomic.Int64
	compByp, compRsp     atomic.Int64
	sessRate, connRate   freqCtr
	reqRate              freqCtr
	sessRateMax          atomic.Int64
	connRateMax          atomic.Int64
	reqRateMax           atomic.Int64
	lastSess             atomic.Int64 // unix seconds, 0 = never
	qtime, ctime         swrate
	rtime, ttime         swrate
	qtimeMax, ctimeMax   atomic.Int64
	rtimeMax, ttimeMax   atomic.Int64
	chkfail, chkdown     atomic.Int64
	downtimeAccumulated  atomic.Int64 // seconds, closed down periods
	lastChangeUnixMillis atomic.Int64
}

func (c *counters) sessionStart(now time.Time) {
	raiseMax(&c.smax, c.scur.Add(1))
	c.stot.Add(1)
	raiseMax(&c.sessRateMax, c.sessRate.add(now))
	c.lastSess.Store(now.Unix())
}

func (c *counters) sessionEnd() { c.scur.Add(-1) }

func (c *counters) request(now time.Time) {
	c.reqTot.Add(1)
	raiseMax(&c.reqRateMax, c.reqRate.add(now))
}

func (c *counters) connection(now time.Time) {
	c.connTot.Add(1)
	raiseMax(&c.connRateMax, c.connRate.add(now))
}

func (c *counters) status(code int) {
	i := code/100 - 1
	if i < 0 || i > 4 {
		i = 5
	}
	c.hrsp[i].Add(1)
}

func (c *counters) queued() { raiseMax(&c.qmax, c.qcur.Add(1)) }

func (c *counters) timers(q, conn, resp, total time.Duration) {
	if q >= 0 {
		ms := q.Milliseconds()
		c.qtime.add(ms)
		raiseMax(&c.qtimeMax, ms)
	}
	if conn >= 0 {
		ms := conn.Milliseconds()
		c.ctime.add(ms)
		raiseMax(&c.ctimeMax, ms)
	}
	if resp >= 0 {
		ms := resp.Milliseconds()
		c.rtime.add(ms)
		raiseMax(&c.rtimeMax, ms)
	}
	if total >= 0 {
		ms := total.Milliseconds()
		c.ttime.add(ms)
		raiseMax(&c.ttimeMax, ms)
	}
}
