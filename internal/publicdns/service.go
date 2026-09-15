// Package publicdns is the public DNS integration: it manages records at the
// user's DNS provider (GoDaddy, Cloudflare) and creates the record a proxy
// host needs after each apply. Record changes are immediate and audited; it
// never overwrites or deletes records on its own.
package publicdns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/events"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

const (
	zonesTTL   = 2 * time.Minute
	recordsTTL = time.Minute
)

// Check statuses.
const (
	StatusUnmanaged = "unmanaged"
	StatusExcluded  = "excluded"
	StatusExists    = "exists"
	StatusWildcard  = "wildcard"
	StatusMissing   = "missing"
	StatusConflict  = "conflict"
	StatusError     = "error"
	StatusCreated   = "created"
)

// ErrDisabled is returned while the integration is turned off.
var ErrDisabled = httpx.Errorf(http.StatusConflict, "public_dns_disabled", "Public DNS is turned off. Enable it in Settings → Public DNS.")

type Service struct {
	app *core.App
	log *slog.Logger
	now func() time.Time
	// newProvider builds provider clients (replaced in tests).
	newProvider func(*model.DNSProvider) (provider, error)

	mu      sync.Mutex
	clients map[string]clientEntry  // DNS provider id → client
	zones   map[string]zonesEntry   // DNS provider id → zones
	records map[string]recordsEntry // zone → records
	syncMu  sync.Mutex
}

type clientEntry struct {
	key  string // provider type + credentials
	prov provider
}

type zonesEntry struct {
	zones []string
	err   error
	at    time.Time
}

type recordsEntry struct {
	records []Record
	at      time.Time
}

// ProviderStatus describes a selected DNS provider.
type ProviderStatus struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Supported bool     `json:"supported"`
	Zones     []string `json:"zones"`
	Error     string   `json:"error,omitempty"`
}

// Zone is a domain managed through a DNS provider.
type Zone struct {
	Name         string `json:"name"`
	ProviderID   string `json:"providerId"`
	ProviderName string `json:"providerName"`
	ProviderType string `json:"providerType"`
}

type Status struct {
	Enabled   bool             `json:"enabled"`
	PublicIP  string           `json:"publicIp"`
	Target    string           `json:"target"`
	Providers []ProviderStatus `json:"providers"`
	Zones     []Zone           `json:"zones"`
}

// DomainCheck is the DNS state of one proxy host domain.
type DomainCheck struct {
	Domain       string       `json:"domain"`
	Zone         string       `json:"zone,omitempty"`
	ProviderID   string       `json:"providerId,omitempty"`
	ProviderType string       `json:"providerType,omitempty"`
	Status       string       `json:"status"`
	Message      string       `json:"message,omitempty"`
	Records      []Record     `json:"records"`
	RelayRecords []Record     `json:"relayRecords"`
	Planned      *RecordInput `json:"planned,omitempty"`
}

func New(app *core.App) *Service {
	log := app.Log
	if log == nil {
		log = slog.Default()
	}
	s := &Service{app: app, log: log.With("svc", "publicdns"), now: time.Now, newProvider: newProvider,
		clients: map[string]clientEntry{}, zones: map[string]zonesEntry{}, records: map[string]recordsEntry{}}
	httpx.SettingsHooks[model.SettingsPublicDNS] = &httpx.SettingsHook{
		BeforeSave: func(r *http.Request, _, next any) error {
			set, ok := next.(*model.PublicDNSSettings)
			if !ok {
				return nil
			}
			return s.validateSettings(r.Context(), set)
		},
		AfterSave: func(*http.Request, any, any) { s.invalidate("") },
	}
	return s
}

func (s *Service) Start(ctx context.Context) error {
	go s.watchApplies(ctx)
	return nil
}

// watchApplies creates missing records after each successful apply.
func (s *Service) watchApplies(ctx context.Context) {
	ch, cancel := s.app.Bus.Subscribe(32)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Topic != events.ApplyFinished {
				continue
			}
			if data, _ := ev.Data.(map[string]any); data["status"] != "live" {
				continue
			}
			go func() {
				sctx, cancel := context.WithTimeout(core.WithActor(context.WithoutCancel(ctx), core.SystemActor), 5*time.Minute)
				defer cancel()
				set := s.settings(sctx)
				if !set.Enabled || !set.AutoCreate {
					return
				}
				if _, err := s.Sync(sctx, nil); err != nil && !errors.Is(err, ErrDisabled) {
					s.log.Warn("public dns: sync after apply", "err", err)
				}
			}()
		}
	}
}

func (s *Service) settings(ctx context.Context) model.PublicDNSSettings {
	set, err := store.LoadSettings[model.PublicDNSSettings](ctx, s.app.Store, model.SettingsPublicDNS)
	if err != nil {
		return model.DefaultPublicDNS()
	}
	set.Normalize()
	return set
}

func (s *Service) validateSettings(ctx context.Context, set *model.PublicDNSSettings) error {
	set.Normalize()
	e := model.Errs{}
	if err := set.Validate(); err != nil {
		var ve *model.ValidationError
		if !errors.As(err, &ve) {
			return err
		}
		for k, v := range ve.Fields {
			e[k] = v
		}
	}
	for _, id := range set.ProviderIDs {
		p, err := s.app.Store.DNSProviders().Get(ctx, id)
		if err != nil {
			e.Add("providerIds", "One of the selected DNS providers no longer exists")
			break
		}
		if !slices.Contains(model.PublicDNSProviderTypes, p.Type) {
			e.Add("providerIds", "%s DNS providers can't manage records yet (supported: GoDaddy, Cloudflare)", p.Type)
			break
		}
	}
	return e.Err()
}

func (s *Service) invalidate(zone string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if zone == "" {
		s.zones = map[string]zonesEntry{}
		s.records = map[string]recordsEntry{}
		return
	}
	delete(s.records, zone)
}

// ---------------------------------------------------------------- providers and zones

type zoneInfo struct {
	Zone
	prov provider
}

// client returns a (cached) client for a DNS provider.
func (s *Service) client(p *model.DNSProvider) (provider, error) {
	parts := []string{p.Type}
	for k, v := range p.Credentials {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts[1:])
	key := strings.Join(parts, "\x00")
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.clients[p.ID]; ok && c.key == key {
		return c.prov, nil
	}
	prov, err := s.newProvider(p)
	if err != nil {
		return nil, err
	}
	s.clients[p.ID] = clientEntry{key: key, prov: prov}
	return prov, nil
}

// managed returns the zones of the selected providers and each provider's state.
func (s *Service) managed(ctx context.Context, set model.PublicDNSSettings) ([]zoneInfo, []ProviderStatus) {
	var zones []zoneInfo
	statuses := []ProviderStatus{}
	seen := map[string]bool{}
	for _, id := range set.ProviderIDs {
		p, err := s.app.Store.DNSProviders().Get(ctx, id)
		if err != nil {
			statuses = append(statuses, ProviderStatus{ID: id, Zones: []string{}, Error: "This DNS provider no longer exists"})
			continue
		}
		st := ProviderStatus{ID: p.ID, Name: p.Name, Type: p.Type, Supported: slices.Contains(model.PublicDNSProviderTypes, p.Type), Zones: []string{}}
		prov, err := s.client(p)
		if err != nil {
			st.Error = err.Error()
			statuses = append(statuses, st)
			continue
		}
		list, err := s.providerZones(ctx, p.ID, prov)
		if err != nil {
			st.Error = err.Error()
		}
		for _, z := range list {
			st.Zones = append(st.Zones, z)
			if seen[z] {
				continue // the first provider listing a zone manages it
			}
			seen[z] = true
			zones = append(zones, zoneInfo{Zone: Zone{Name: z, ProviderID: p.ID, ProviderName: p.Name, ProviderType: p.Type}, prov: prov})
		}
		statuses = append(statuses, st)
	}
	sort.Slice(zones, func(i, j int) bool { return zones[i].Name < zones[j].Name })
	return zones, statuses
}

func (s *Service) providerZones(ctx context.Context, id string, prov provider) ([]string, error) {
	s.mu.Lock()
	e, ok := s.zones[id]
	s.mu.Unlock()
	if ok && s.now().Sub(e.at) < zonesTTL {
		return e.zones, e.err
	}
	list, err := prov.Zones(ctx)
	s.mu.Lock()
	s.zones[id] = zonesEntry{zones: list, err: err, at: s.now()}
	s.mu.Unlock()
	return list, err
}

func (s *Service) zone(ctx context.Context, set model.PublicDNSSettings, name string) (*zoneInfo, error) {
	name = trimDot(name)
	zones, _ := s.managed(ctx, set)
	for i := range zones {
		if zones[i].Name == name {
			return &zones[i], nil
		}
	}
	return nil, httpx.Errorf(http.StatusNotFound, "zone_not_found", fmt.Sprintf("%s isn't a domain of the connected DNS providers", name))
}

// zoneFor returns the zone owning domain (longest match).
func zoneFor(zones []zoneInfo, domain string) *zoneInfo {
	var best *zoneInfo
	for i := range zones {
		z := &zones[i]
		if (domain == z.Name || strings.HasSuffix(domain, "."+z.Name)) && (best == nil || len(z.Name) > len(best.Name)) {
			best = z
		}
	}
	return best
}

// zoneRecords returns a zone's records (cached), annotated with the proxy
// hosts they serve.
func (s *Service) zoneRecords(ctx context.Context, z *zoneInfo, fresh bool) ([]Record, error) {
	s.mu.Lock()
	e, ok := s.records[z.Name]
	s.mu.Unlock()
	if ok && !fresh && s.now().Sub(e.at) < recordsTTL {
		return e.records, nil
	}
	list, err := z.prov.Records(ctx, z.Name)
	if err != nil {
		return nil, err
	}
	hosts, _ := s.app.Store.Hosts().List(ctx)
	for i := range list {
		r := &list[i]
		r.ReadOnly = readOnly(*r)
		if !addressTypes[r.Type] {
			continue
		}
		for _, h := range hosts {
			for _, d := range h.Domains {
				if strings.EqualFold(trimDot(d), r.FQDN) && !slices.Contains(r.Hosts, h.ID) {
					r.Hosts = append(r.Hosts, h.ID)
				}
			}
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Name != list[j].Name {
			return list[i].Name < list[j].Name
		}
		return list[i].Type < list[j].Type
	})
	s.mu.Lock()
	s.records[z.Name] = recordsEntry{records: list, at: s.now()}
	s.mu.Unlock()
	return list, nil
}

// target is where Relay's records point: the configured target or the
// detected public IP.
func (s *Service) target(ctx context.Context, set model.PublicDNSSettings) (target, publicIP string) {
	if gen, err := store.LoadSettings[model.GeneralSettings](ctx, s.app.Store, model.SettingsGeneral); err == nil {
		publicIP = strings.TrimSpace(gen.PublicIP)
	}
	target = set.Target
	if target == "" && set.RecordType == "A" {
		target = publicIP
	}
	return target, publicIP
}

// ---------------------------------------------------------------- status and records

func (s *Service) Status(ctx context.Context) Status {
	set := s.settings(ctx)
	target, ip := s.target(ctx, set)
	st := Status{Enabled: set.Enabled, PublicIP: ip, Target: target, Providers: []ProviderStatus{}, Zones: []Zone{}}
	if !set.Enabled {
		return st
	}
	zones, providers := s.managed(ctx, set)
	st.Providers = providers
	for _, z := range zones {
		st.Zones = append(st.Zones, z.Zone)
	}
	return st
}

func (s *Service) Records(ctx context.Context, zone string) (*zoneInfo, []Record, error) {
	set := s.settings(ctx)
	if !set.Enabled {
		return nil, nil, ErrDisabled
	}
	z, err := s.zone(ctx, set, zone)
	if err != nil {
		return nil, nil, err
	}
	list, err := s.zoneRecords(ctx, z, false)
	return z, list, err
}

func (s *Service) find(ctx context.Context, z *zoneInfo, id string) (*Record, error) {
	list, err := s.zoneRecords(ctx, z, true)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].ID == id {
			return &list[i], nil
		}
	}
	return nil, httpx.Errorf(http.StatusNotFound, "record_not_found", "That record no longer exists; refresh the list")
}

// lookup returns the record matching in after a write.
func (s *Service) lookup(ctx context.Context, z *zoneInfo, in RecordInput) Record {
	list, err := s.zoneRecords(ctx, z, true)
	if err == nil {
		for _, r := range list {
			if r.Type == in.Type && r.Name == in.Name && strings.EqualFold(trimDot(r.Data), trimDot(in.Data)) {
				return r
			}
		}
	}
	return Record{Type: in.Type, Name: in.Name, FQDN: fqdnOf(in.Name, z.Name), Data: in.Data, TTL: in.TTL, Priority: in.Priority, Proxied: in.Proxied}
}

func (s *Service) Create(ctx context.Context, zone string, in RecordInput) (Record, error) {
	set := s.settings(ctx)
	if !set.Enabled {
		return Record{}, ErrDisabled
	}
	z, err := s.zone(ctx, set, zone)
	if err != nil {
		return Record{}, err
	}
	if err := normalizeInput(&in, z.Name, z.prov.MinTTL(), set.TTL); err != nil {
		return Record{}, err
	}
	err = z.prov.Create(ctx, z.Name, in)
	s.audit(ctx, "dns.record_create", fqdnOf(in.Name, z.Name), in.Type+" "+in.Data, err)
	if err != nil {
		return Record{}, err
	}
	s.invalidate(z.Name)
	return s.lookup(ctx, z, in), nil
}

func (s *Service) Update(ctx context.Context, zone, id string, in RecordInput) (Record, error) {
	set := s.settings(ctx)
	if !set.Enabled {
		return Record{}, ErrDisabled
	}
	z, err := s.zone(ctx, set, zone)
	if err != nil {
		return Record{}, err
	}
	cur, err := s.find(ctx, z, id)
	if err != nil {
		return Record{}, err
	}
	if cur.ReadOnly {
		return Record{}, httpx.Errorf(http.StatusConflict, "record_read_only", "This record can't be changed from Relay")
	}
	if err := normalizeInput(&in, z.Name, z.prov.MinTTL(), set.TTL); err != nil {
		return Record{}, err
	}
	err = z.prov.Update(ctx, z.Name, *cur, in)
	s.audit(ctx, "dns.record_update", fqdnOf(in.Name, z.Name), fmt.Sprintf("%s %s → %s %s", cur.Type, cur.Data, in.Type, in.Data), err)
	if err != nil {
		return Record{}, err
	}
	s.invalidate(z.Name)
	return s.lookup(ctx, z, in), nil
}

func (s *Service) Delete(ctx context.Context, zone, id string) error {
	set := s.settings(ctx)
	if !set.Enabled {
		return ErrDisabled
	}
	z, err := s.zone(ctx, set, zone)
	if err != nil {
		return err
	}
	cur, err := s.find(ctx, z, id)
	if err != nil {
		return err
	}
	if cur.ReadOnly {
		return httpx.Errorf(http.StatusConflict, "record_read_only", "This record can't be deleted from Relay")
	}
	err = z.prov.Delete(ctx, z.Name, *cur)
	s.audit(ctx, "dns.record_delete", cur.FQDN, cur.Type+" "+cur.Data, err)
	if err != nil {
		return err
	}
	s.invalidate(z.Name)
	return nil
}

func (s *Service) audit(ctx context.Context, action, target, detail string, err error) {
	result := "ok"
	if err != nil {
		result, detail = "failed", detail+" · "+err.Error()
	}
	s.app.Audit(ctx, core.AuditEntry{Action: action, Target: target, Detail: detail, Result: result})
}

// ---------------------------------------------------------------- host domains

// Check reports the DNS state of domains.
func (s *Service) Check(ctx context.Context, domains []string) ([]DomainCheck, error) {
	set := s.settings(ctx)
	if !set.Enabled {
		return nil, ErrDisabled
	}
	return s.check(ctx, set, domains, false), nil
}

func (s *Service) check(ctx context.Context, set model.PublicDNSSettings, domains []string, fresh bool) []DomainCheck {
	zones, providers := s.managed(ctx, set)
	target, _ := s.target(ctx, set)
	out := make([]DomainCheck, 0, len(domains))
	seen := map[string]bool{}
	refreshed := map[string]bool{}
	for _, raw := range domains {
		d := trimDot(raw)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		c := DomainCheck{Domain: d, Records: []Record{}, RelayRecords: []Record{}}
		if !model.ValidDNSName(d, true) {
			c.Status, c.Message = StatusUnmanaged, "Not a public domain name"
			out = append(out, c)
			continue
		}
		z := zoneFor(zones, d)
		if z == nil {
			c.Status, c.Message = StatusUnmanaged, "Not in a domain of the connected DNS providers"
			for _, p := range providers {
				if p.Error != "" {
					c.Message += " (" + p.Name + ": " + p.Error + ")"
				}
			}
			out = append(out, c)
			continue
		}
		c.Zone, c.ProviderID, c.ProviderType = z.Name, z.ProviderID, z.ProviderType
		if slices.Contains(set.ExcludedZones, z.Name) {
			c.Status, c.Message = StatusExcluded, z.Name+" is excluded from automatic changes"
			out = append(out, c)
			continue
		}
		name := relName(d, z.Name)
		if target != "" && !(set.RecordType == "CNAME" && name == "@") {
			c.Planned = &RecordInput{Type: set.RecordType, Name: name, Data: target, TTL: set.TTL}
		}
		records, err := s.zoneRecords(ctx, z, fresh && !refreshed[z.Name])
		refreshed[z.Name] = true
		if err != nil {
			c.Status, c.Message = StatusError, err.Error()
			out = append(out, c)
			continue
		}
		exact := addressRecords(records, name)
		switch {
		case len(exact) > 0:
			c.Records = exact
			c.RelayRecords = pointing(exact, set, target)
			if len(c.RelayRecords) > 0 {
				c.Status, c.Message = StatusExists, "Points to "+target
			} else {
				c.Status, c.Message = StatusConflict, "Points to "+exact[0].Data+"; Relay doesn't change it"
			}
			c.Planned = nil
		case wildcardFor(records, name) != nil:
			w := wildcardFor(records, name)
			c.Records = w
			c.RelayRecords = pointing(w, set, target)
			c.Status, c.Message = StatusWildcard, "Covered by "+w[0].FQDN
			c.Planned = nil
		default:
			c.Status = StatusMissing
			if c.Planned == nil {
				if set.RecordType == "CNAME" && name == "@" {
					c.Message = "A domain itself can't be a CNAME; add an A record by hand"
				} else {
					c.Message = "Relay doesn't know its public IP yet; set a target in Settings → Public DNS"
				}
			} else {
				c.Message = fmt.Sprintf("No record yet; Relay can create %s %s → %s", c.Planned.Type, d, c.Planned.Data)
			}
		}
		out = append(out, c)
	}
	return out
}

func addressRecords(records []Record, name string) []Record {
	var out []Record
	for _, r := range records {
		if r.Name == name && addressTypes[r.Type] {
			out = append(out, r)
		}
	}
	return out
}

// wildcardFor returns the address records of the wildcard covering name
// ("*.dev" for "app.dev", "*" for "app").
func wildcardFor(records []Record, name string) []Record {
	if name == "@" {
		return nil
	}
	parent := ""
	if i := strings.IndexByte(name, '.'); i >= 0 {
		parent = name[i+1:]
	}
	w := "*"
	if parent != "" {
		w = "*." + parent
	}
	if w == name {
		return nil
	}
	return addressRecords(records, w)
}

func pointing(records []Record, set model.PublicDNSSettings, target string) []Record {
	out := []Record{}
	if target == "" {
		return out
	}
	for _, r := range records {
		if r.Type == set.RecordType && strings.EqualFold(trimDot(r.Data), target) {
			out = append(out, r)
		}
	}
	return out
}

// Sync creates the missing records of enabled proxy hosts (all of them, or
// hostIDs). Existing and conflicting records are never changed.
func (s *Service) Sync(ctx context.Context, hostIDs []string) ([]DomainCheck, error) {
	set := s.settings(ctx)
	if !set.Enabled {
		return nil, ErrDisabled
	}
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	hosts, err := s.app.Store.Hosts().List(ctx)
	if err != nil {
		return nil, err
	}
	var domains []string
	for _, h := range hosts {
		if !h.Enabled || (len(hostIDs) > 0 && !slices.Contains(hostIDs, h.ID)) {
			continue
		}
		domains = append(domains, h.Domains...)
	}
	results := s.check(ctx, set, domains, true)
	zones, _ := s.managed(ctx, set)
	for i := range results {
		c := &results[i]
		if c.Status != StatusMissing || c.Planned == nil {
			continue
		}
		z := zoneFor(zones, c.Domain)
		if z == nil {
			continue
		}
		in := *c.Planned
		if err := normalizeInput(&in, z.Name, z.prov.MinTTL(), set.TTL); err != nil {
			c.Status, c.Message = StatusError, err.Error()
			continue
		}
		err := z.prov.Create(ctx, z.Name, in)
		s.audit(ctx, "dns.record_create", c.Domain, in.Type+" "+in.Data+" · for a proxy host", err)
		if err != nil {
			c.Status, c.Message = StatusError, err.Error()
			s.app.Activity(ctx, "dns.record", "error", "Couldn't create DNS record "+c.Domain, c.Domain, err.Error())
			continue
		}
		s.invalidate(z.Name)
		created := s.lookup(ctx, z, in)
		c.Status, c.Message = StatusCreated, fmt.Sprintf("Created %s → %s", in.Type, in.Data)
		c.Records, c.RelayRecords, c.Planned = []Record{created}, []Record{created}, nil
		s.app.Activity(ctx, "dns.record", "info", "Created DNS record "+c.Domain, c.Domain, in.Type+" → "+in.Data)
	}
	return results, nil
}
