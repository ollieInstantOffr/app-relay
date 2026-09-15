package balancer

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/instantoffr/relay/internal/balancer/spec"
)

// lbRuntime compiles a single backend without binding anything.
func lbRuntime(t *testing.T, algo string, servers ...spec.Server) *backend {
	t.Helper()
	cfg := baseConfig(t)
	b := httpBackend("be", servers...)
	b.Algorithm = algo
	cfg.Backends = []spec.Backend{b}
	rt, err := compile(cfg, compileEnv{log: newLogger(nil)})
	if err != nil {
		t.Fatal(err)
	}
	rt.commit(newLogger(nil))
	return rt.backends[0]
}

func srvSpec(name string, weight int) spec.Server {
	return spec.Server{Name: name, Address: "127.0.0.1", Port: 1, Weight: weight, State: spec.StateReady}
}

func backupSpec(name string, weight int) spec.Server {
	s := srvSpec(name, weight)
	s.Backup = true
	return s
}

func picks(be *backend, n int, pc func(i int) *pickCtx) map[string]int {
	out := map[string]int{}
	for i := range n {
		s := be.pick(pc(i))
		if s == nil {
			out["<nil>"]++
			continue
		}
		out[s.name]++
	}
	return out
}

func plain(int) *pickCtx { return &pickCtx{client: netip.MustParseAddr("10.0.0.1")} }

func TestRoundRobinWeightsAndRuntimeWeight(t *testing.T) {
	be := lbRuntime(t, spec.AlgoRoundRobin, srvSpec("a", 3), srvSpec("b", 1), srvSpec("zero", 0))
	if got := picks(be, 400, plain); got["a"] != 300 || got["b"] != 100 || got["zero"] != 0 {
		t.Fatalf("weighted: %v", got)
	}
	// Smooth: never more than 3 consecutive picks of a.
	seq := ""
	for range 8 {
		seq += be.pick(plain(0)).name
	}
	if strings.Contains(seq, "aaaa") || !strings.Contains(seq, "b") {
		t.Fatalf("not interleaved: %s", seq)
	}
	be.srvByName["b"].st.setWeight(3)
	if got := picks(be, 600, plain); got["a"] != 300 || got["b"] != 300 {
		t.Fatalf("after runtime weight change: %v", got)
	}
	be.srvByName["a"].st.setWeight(0)
	if got := picks(be, 10, plain); got["b"] != 10 {
		t.Fatalf("weight 0: %v", got)
	}
}

func TestStaticRR(t *testing.T) {
	be := lbRuntime(t, spec.AlgoStaticRR, srvSpec("a", 2), srvSpec("b", 1))
	if got := picks(be, 300, plain); got["a"] != 200 || got["b"] != 100 {
		t.Fatalf("%v", got)
	}
}

func TestLeastConn(t *testing.T) {
	be := lbRuntime(t, spec.AlgoLeastConn, srvSpec("a", 1), srvSpec("b", 1), srvSpec("c", 2))
	a, b, c := be.srvByName["a"], be.srvByName["b"], be.srvByName["c"]
	a.st.c.scur.Store(5)
	b.st.c.scur.Store(1)
	c.st.c.scur.Store(3) // (3+1)/2 = 2 > (1+1)/1 = 2 → tie with b broken by rotation
	got := picks(be, 100, plain)
	if got["a"] != 0 || got["b"] == 0 || got["c"] == 0 {
		t.Fatalf("ties must rotate between b and c: %v", got)
	}
	c.st.c.scur.Store(0)
	if s := be.pick(plain(0)); s != c {
		t.Fatalf("least loaded relative to weight: %s", s.name)
	}
	// Sessions assigned through setServer count immediately.
	be2 := lbRuntime(t, spec.AlgoLeastConn, srvSpec("a", 1), srvSpec("b", 1))
	var sessions []*session
	for range 10 {
		x := &session{be: be2}
		x.setServer(be2.pick(plain(0)), true)
		sessions = append(sessions, x)
	}
	if be2.srvByName["a"].st.c.scur.Load() != 5 || be2.srvByName["b"].st.c.scur.Load() != 5 {
		t.Fatal("leastconn did not spread concurrent sessions")
	}
	for _, x := range sessions {
		x.release()
	}
	if be2.srvByName["a"].st.c.lbtot.Load() != 5 || be2.st.c.lbtot.Load() != 10 {
		t.Fatal("lbtot")
	}
}

func TestSourceHash(t *testing.T) {
	be := lbRuntime(t, spec.AlgoSource, srvSpec("a", 3), srvSpec("b", 1))
	byIP := func(i int) *pickCtx {
		return &pickCtx{client: netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})}
	}
	got := picks(be, 4000, byIP)
	if got["a"] != 3000 || got["b"] != 1000 {
		t.Fatalf("source distribution: %v", got)
	}
	first := picks(be, 50, byIP)
	again := picks(be, 50, byIP)
	if fmt.Sprint(first) != fmt.Sprint(again) {
		t.Fatal("source hash is not stable")
	}
	c := byIP(7)
	s1 := be.pick(c)
	for range 20 {
		if be.pick(c) != s1 {
			t.Fatal("same client moved")
		}
	}
	// When its server goes down, the client moves; others keep theirs.
	s1.st.forceHealth(false)
	if s := be.pick(c); s == s1 || s == nil {
		t.Fatal("client stayed on a down server")
	}
	v6 := &pickCtx{client: netip.MustParseAddr("2001:db8::1")}
	if be.pick(v6) != be.pick(v6) {
		t.Fatal("ipv6 source hash not stable")
	}
}

func TestURIHash(t *testing.T) {
	be := lbRuntime(t, spec.AlgoURI, srvSpec("a", 1), srvSpec("b", 1), srvSpec("c", 1))
	byPath := func(i int) *pickCtx { return &pickCtx{path: fmt.Sprintf("/asset/%d.js", i)} }
	got := picks(be, 3000, byPath)
	for _, n := range []string{"a", "b", "c"} {
		if got[n] < 600 {
			t.Fatalf("uri distribution: %v", got)
		}
	}
	p := &pickCtx{path: "/x/y"}
	s := be.pick(p)
	for range 10 {
		if be.pick(p) != s {
			t.Fatal("uri hash not stable")
		}
	}
}

func TestRandomWeighted(t *testing.T) {
	be := lbRuntime(t, spec.AlgoRandom, srvSpec("a", 3), srvSpec("b", 1))
	got := picks(be, 4000, plain)
	// Two draws keeping the less loaded one (all idle → first draw): ~75/25.
	if got["a"] < 2600 || got["a"] > 3400 {
		t.Fatalf("random distribution: %v", got)
	}
	be.srvByName["a"].st.c.scur.Store(100)
	got = picks(be, 1000, plain)
	if got["b"] < 350 {
		t.Fatalf("power of two choices should favour the idle server: %v", got)
	}
}

func TestFirst(t *testing.T) {
	be := lbRuntime(t, spec.AlgoFirst, srvSpec("a", 1), srvSpec("b", 100))
	if got := picks(be, 10, plain); got["a"] != 10 {
		t.Fatalf("%v", got)
	}
	be.srvByName["a"].st.setAdmin(spec.StateMaint)
	if got := picks(be, 10, plain); got["b"] != 10 {
		t.Fatalf("%v", got)
	}
}

func TestBackupServers(t *testing.T) {
	be := lbRuntime(t, spec.AlgoRoundRobin, srvSpec("a", 1), backupSpec("bk1", 1), backupSpec("bk2", 1), srvSpec("b", 1))
	if got := picks(be, 10, plain); got["bk1"]+got["bk2"] != 0 {
		t.Fatalf("backup used while actives are up: %v", got)
	}
	be.srvByName["a"].st.forceHealth(false)
	be.srvByName["b"].st.setAdmin(spec.StateDrain)
	if got := picks(be, 10, plain); got["bk1"] != 10 {
		t.Fatalf("only the first usable backup: %v", got)
	}
	be.srvByName["bk1"].st.setAdmin(spec.StateMaint)
	if got := picks(be, 10, plain); got["bk2"] != 10 {
		t.Fatalf("next backup: %v", got)
	}
	be.srvByName["b"].st.setAdmin(spec.StateReady)
	if got := picks(be, 10, plain); got["b"] != 10 {
		t.Fatalf("back to actives: %v", got)
	}
	if be.totalWeight() != 1 || be.usableCount(false) != 1 || be.usableCount(true) != 1 {
		t.Fatalf("weight %d act %d bck %d", be.totalWeight(), be.usableCount(false), be.usableCount(true))
	}
}

func TestDrainMaintDownSemantics(t *testing.T) {
	be := lbRuntime(t, spec.AlgoRoundRobin, srvSpec("a", 1), srvSpec("b", 1))
	a := be.srvByName["a"]
	cases := []struct {
		name          string
		apply         func()
		lb, persist   bool
		statusPattern string
	}{
		{"ready", func() {}, true, true, "no check"},
		{"drain", func() { a.st.setAdmin(spec.StateDrain) }, false, true, "DRAIN"},
		{"maint", func() { a.st.setAdmin(spec.StateMaint) }, false, false, "MAINT"},
		{"ready again", func() { a.st.setAdmin(spec.StateReady) }, true, true, "no check"},
		{"down", func() { a.st.forceHealth(false) }, false, false, "DOWN"},
		{"drain while down", func() { a.st.setAdmin(spec.StateDrain) }, false, false, "DOWN"},
	}
	for _, c := range cases {
		c.apply()
		if a.st.usableLB() != c.lb || a.st.usablePersist() != c.persist || a.st.snapshot().status != c.statusPattern {
			t.Errorf("%s: lb %v persist %v status %q", c.name, a.st.usableLB(), a.st.usablePersist(), a.st.snapshot().status)
		}
	}
	a.st.forceHealth(true)
	a.st.setAdmin(spec.StateReady)
	a.st.setWeight(0)
	if a.st.usableLB() || !a.st.usablePersist() {
		t.Error("weight 0 must behave like drain")
	}
}

func TestInitialStatesFromConfig(t *testing.T) {
	d, m := srvSpec("d", 1), srvSpec("m", 1)
	d.State, m.State = spec.StateDrain, spec.StateMaint
	be := lbRuntime(t, spec.AlgoRoundRobin, srvSpec("a", 1), d, m)
	if got := picks(be, 6, plain); got["a"] != 6 {
		t.Fatalf("%v", got)
	}
	if be.srvByName["d"].st.snapshot().status != "DRAIN" || be.srvByName["m"].st.snapshot().status != "MAINT" {
		t.Fatal("initial admin states")
	}
}
