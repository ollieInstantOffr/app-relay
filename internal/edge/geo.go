package edge

import (
	"fmt"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"

	"github.com/instantoffr/relay/internal/model"
)

// geoStore holds the country database for hosts with allowCountries. The
// file is re-read when it changes on disk (Relay refreshes it monthly), so no
// reload is needed after an update.
type geoStore struct {
	mu      sync.Mutex // loading
	path    string
	cur     atomic.Pointer[geoDB]
	checked atomic.Int64 // unix nanos of the last change check
}

type geoDB struct {
	reader  *maxminddb.Reader
	modTime time.Time
	size    int64
}

const geoCheckInterval = time.Minute

func newGeoStore() *geoStore { return &geoStore{} }

// load opens path unless that unchanged file is already loaded. On error the
// loaded database stays.
func (g *geoStore) load(path string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("geoipDatabase: %w", err)
	}
	if cur := g.cur.Load(); cur != nil && g.path == path && cur.modTime.Equal(st.ModTime()) && cur.size == st.Size() {
		return nil
	}
	// Read into memory rather than mmap: replaced readers are simply garbage
	// collected while in-flight lookups finish.
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("geoipDatabase: %w", err)
	}
	r, err := maxminddb.OpenBytes(b)
	if err != nil {
		return fmt.Errorf("geoipDatabase %s: %w", path, err)
	}
	g.path = path
	g.cur.Store(&geoDB{reader: r, modTime: st.ModTime(), size: st.Size()})
	g.checked.Store(time.Now().UnixNano())
	return nil
}

func (g *geoStore) maybeRefresh() {
	now := time.Now().UnixNano()
	last := g.checked.Load()
	if now-last < int64(geoCheckInterval) || !g.checked.CompareAndSwap(last, now) {
		return
	}
	go func() {
		g.mu.Lock()
		path := g.path
		g.mu.Unlock()
		if path != "" {
			_ = g.load(path)
		}
	}()
}

type geoRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	RegisteredCountry struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"registered_country"`
}

// country returns the ISO code for ip ("" when unknown). Like the nginx geo
// include, the registered country is used when the country is missing.
func (g *geoStore) country(ip netip.Addr) string {
	g.maybeRefresh()
	db := g.cur.Load()
	if db == nil {
		return ""
	}
	var rec geoRecord
	if err := db.reader.Lookup(ip.Unmap().WithZone("")).Decode(&rec); err != nil {
		return ""
	}
	if rec.Country.ISOCode != "" {
		return rec.Country.ISOCode
	}
	return rec.RegisteredCountry.ISOCode
}

// allowed: local and private addresses always pass; any other address must
// resolve to one of the countries (unknown addresses are denied, as in nginx).
func (g *geoStore) allowed(countries map[string]bool, ip netip.Addr) bool {
	if !ip.IsValid() || model.IsLocalAddr(ip) {
		return true
	}
	return countries[g.country(ip)]
}
