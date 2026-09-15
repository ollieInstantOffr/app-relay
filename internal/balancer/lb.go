package balancer

import (
	"container/list"
	"math/rand/v2"
	"net/netip"
	"sync"
	"time"

	"github.com/instantoffr/relay/internal/balancer/spec"
)

// usableCount counts servers usable for load balancing (backup or active).
func (b *backend) usableCount(backup bool) int {
	list := b.actives
	if backup {
		list = b.backups
	}
	n := 0
	for _, s := range list {
		if s.st.usableLB() {
			n++
		}
	}
	return n
}

// totalWeight is the backend weight in show stat: the sum of the weights of
// the servers currently taking traffic (active ones, else the first backup).
func (b *backend) totalWeight() int {
	w := 0
	for _, s := range b.actives {
		if s.st.usableLB() {
			w += int(s.st.weight.Load())
		}
	}
	if w == 0 {
		if s := b.firstBackup(nil); s != nil {
			w = int(s.st.weight.Load())
		}
	}
	return w
}

type pickCtx struct {
	client  netip.Addr
	path    string
	exclude *server
}

// pick selects a server for a new session with the backend's algorithm.
// Backup servers are used only when no active server is usable (the first
// usable backup in declaration order). exclude is avoided when possible.
func (b *backend) pick(pc *pickCtx) *server {
	var s *server
	switch b.algo {
	case spec.AlgoLeastConn:
		s = b.pickLeastConn(pc.exclude)
	case spec.AlgoSource:
		s = b.pickHash(sourceHash(pc.client), pc.exclude)
	case spec.AlgoURI:
		s = b.pickHash(sdbmHash(pc.path), pc.exclude)
	case spec.AlgoRandom:
		s = b.pickRandom(pc.exclude)
	case spec.AlgoFirst:
		for _, c := range b.actives {
			if c != pc.exclude && c.st.usableLB() {
				s = c
				break
			}
		}
	default: // roundrobin, static-rr
		s = b.pickRR(pc.exclude)
	}
	if s == nil {
		s = b.firstBackup(pc.exclude)
	}
	if s == nil && pc.exclude != nil && pc.exclude.st.usableLB() {
		s = pc.exclude
	}
	return s
}

func (b *backend) firstBackup(exclude *server) *server {
	for _, s := range b.backups {
		if s != exclude && s.st.usableLB() {
			return s
		}
	}
	return nil
}

// pickRR is smooth weighted round robin: weight changes apply immediately.
func (b *backend) pickRR(exclude *server) *server {
	b.lbMu.Lock()
	defer b.lbMu.Unlock()
	var best *server
	var total int64
	for _, s := range b.actives {
		if s == exclude || !s.st.usableLB() {
			if s != exclude {
				s.cw = 0
			}
			continue
		}
		w := int64(s.st.weight.Load())
		s.cw += w
		total += w
		if best == nil || s.cw > best.cw {
			best = s
		}
	}
	if best != nil {
		best.cw -= total
	}
	return best
}

// pickLeastConn picks the server with the fewest active sessions relative to
// its weight; ties rotate.
func (b *backend) pickLeastConn(exclude *server) *server {
	n := len(b.actives)
	if n == 0 {
		return nil
	}
	start := int(b.rr.Add(1) % uint64(n))
	var best *server
	var bestCur, bestW int64
	for i := range n {
		s := b.actives[(start+i)%n]
		if s == exclude || !s.st.usableLB() {
			continue
		}
		cur, w := s.st.c.scur.Load(), int64(s.st.weight.Load())
		if best == nil || (cur+1)*bestW < (bestCur+1)*w {
			best, bestCur, bestW = s, cur, w
		}
	}
	return best
}

// pickRandom draws two servers by weight and keeps the less loaded one
// (HAProxy "random" with its default of 2 draws).
func (b *backend) pickRandom(exclude *server) *server {
	var total int64
	for _, s := range b.actives {
		if s != exclude && s.st.usableLB() {
			total += int64(s.st.weight.Load())
		}
	}
	if total <= 0 {
		return nil
	}
	draw := func() *server {
		r := rand.Int64N(total)
		var last *server
		for _, s := range b.actives {
			if s == exclude || !s.st.usableLB() {
				continue
			}
			last = s
			if r -= int64(s.st.weight.Load()); r < 0 {
				return s
			}
		}
		return last
	}
	a, c := draw(), draw()
	if a == nil || c == nil {
		if a == nil {
			return c
		}
		return a
	}
	// HAProxy keeps the latest draw unless the previous one serves fewer
	// sessions relative to its weight.
	if c.st.c.scur.Load()*int64(a.st.weight.Load()) > a.st.c.scur.Load()*int64(c.st.weight.Load()) {
		return a
	}
	return c
}

// hashMap is HAProxy's map-based hash table: every usable active server
// appears weight times, interleaved.
type hashMap struct {
	ver   uint64
	slots []*server
}

func (b *backend) slots() []*server {
	ver := b.st.version.Load()
	if m := b.hmap.Load(); m != nil && m.ver == ver {
		return m.slots
	}
	b.lbMu.Lock()
	defer b.lbMu.Unlock()
	if m := b.hmap.Load(); m != nil && m.ver == ver {
		return m.slots
	}
	type ent struct {
		s      *server
		w, cur int64
	}
	var ents []ent
	var total int64
	for _, s := range b.actives {
		if s.st.usableLB() {
			w := int64(s.st.weight.Load())
			ents = append(ents, ent{s: s, w: w})
			total += w
		}
	}
	slots := make([]*server, 0, total)
	for range total {
		best := -1
		for i := range ents {
			ents[i].cur += ents[i].w
			if best < 0 || ents[i].cur > ents[best].cur {
				best = i
			}
		}
		ents[best].cur -= total
		slots = append(slots, ents[best].s)
	}
	b.hmap.Store(&hashMap{ver: ver, slots: slots})
	return slots
}

func (b *backend) pickHash(h uint32, exclude *server) *server {
	slots := b.slots()
	if len(slots) == 0 {
		return nil
	}
	i := int(h % uint32(len(slots)))
	for n := range len(slots) {
		s := slots[(i+n)%len(slots)]
		if s != exclude && s.st.usableLB() {
			return s
		}
	}
	return nil
}

// sourceHash is HAProxy's map-based source hash: the XOR of the address's
// 32-bit words.
func sourceHash(a netip.Addr) uint32 {
	a = a.Unmap()
	if a.Is4() {
		b := a.As4()
		return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	}
	b := a.As16()
	var h uint32
	for i := 0; i < 16; i += 4 {
		h ^= uint32(b[i])<<24 | uint32(b[i+1])<<16 | uint32(b[i+2])<<8 | uint32(b[i+3])
	}
	return h
}

// sdbmHash is HAProxy's default hash function (balance uri).
func sdbmHash(s string) uint32 {
	var h uint32
	for i := 0; i < len(s); i++ {
		h = uint32(s[i]) + (h << 6) + (h << 16) - h
	}
	return h
}

// ---------------------------------------------------------------- stick table

// stickTable maps client addresses to server names (stick on src) with an
// expiry and a size bound (least recently used entries are evicted).
type stickTable struct {
	mu     sync.Mutex
	size   int
	expire time.Duration
	m      map[netip.Addr]*list.Element
	ll     list.List
}

type stickEntry struct {
	key    netip.Addr
	server string
	exp    time.Time
}

func newStickTable(size int, expire time.Duration) *stickTable {
	return &stickTable{size: size, expire: expire, m: map[netip.Addr]*list.Element{}}
}

func (t *stickTable) configure(size int, expire time.Duration) {
	t.mu.Lock()
	t.size, t.expire = size, expire
	t.evictLocked(time.Now())
	t.mu.Unlock()
}

func (t *stickTable) get(k netip.Addr, now time.Time) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	el, ok := t.m[k]
	if !ok {
		return "", false
	}
	e := el.Value.(*stickEntry)
	if now.After(e.exp) {
		t.ll.Remove(el)
		delete(t.m, k)
		return "", false
	}
	return e.server, true
}

func (t *stickTable) put(k netip.Addr, server string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if el, ok := t.m[k]; ok {
		e := el.Value.(*stickEntry)
		e.server, e.exp = server, now.Add(t.expire)
		t.ll.MoveToFront(el)
		return
	}
	t.m[k] = t.ll.PushFront(&stickEntry{key: k, server: server, exp: now.Add(t.expire)})
	t.evictLocked(now)
}

func (t *stickTable) evictLocked(now time.Time) {
	for t.ll.Len() > 0 {
		el := t.ll.Back()
		e := el.Value.(*stickEntry)
		if t.ll.Len() <= t.size && !now.After(e.exp) {
			return
		}
		t.ll.Remove(el)
		delete(t.m, e.key)
	}
}

func (t *stickTable) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ll.Len()
}
