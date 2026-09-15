package npmimport

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// row is one NPM database row keyed by column name.
type row map[string]any

func (r row) str(k string) string {
	switch v := r[k].(type) {
	case nil:
		return ""
	case string:
		return v
	case []byte:
		return string(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		if v {
			return "1"
		}
		return "0"
	}
	return fmt.Sprint(r[k])
}

func (r row) num(k string) int {
	switch v := r[k].(type) {
	case int64:
		return int(v)
	case float64:
		return int(v)
	case bool:
		if v {
			return 1
		}
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(r.str(k)))
	return n
}

func (r row) flag(k string) bool {
	s := strings.ToLower(strings.TrimSpace(r.str(k)))
	return s == "1" || s == "true"
}

func (r row) has(k string) bool { _, ok := r[k]; return ok }

func (r row) deleted() bool { return r.flag("is_deleted") }

// enabled defaults to true when the column is missing (older NPM versions).
func (r row) enabled() bool { return !r.has("enabled") || r.flag("enabled") }

func tableExists(ctx context.Context, db *sql.DB, name string) bool {
	var n int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	return n > 0
}

func readTable(ctx context.Context, db *sql.DB, table string) ([]row, error) {
	if !tableExists(ctx, db, table) {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT * FROM `+table+` ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", table, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []row{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := row{}
		for i, c := range cols {
			r[c] = vals[i]
		}
		if !r.deleted() {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

func domainList(s string) []string {
	var raw []string
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		for _, part := range strings.Split(s, ",") {
			raw = append(raw, part)
		}
	}
	out := []string{}
	seen := map[string]bool{}
	for _, d := range raw {
		d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// ---------------------------------------------------------------- plan

type Base struct {
	NPMID      int    `json:"npmId"`
	Name       string `json:"name"`
	Detail     string `json:"detail"`
	Conflict   string `json:"conflict,omitempty"`
	ExistingID string `json:"-"` // entity to overwrite when the conflict is resolvable
	// Overwritable is set on conflicting items that "overwrite" replaces
	// (the others are skipped either way).
	Overwritable bool     `json:"overwritable,omitempty"`
	Warnings     []string `json:"warnings"`
}

func (b *Base) warn(format string, args ...any) {
	b.Warnings = append(b.Warnings, fmt.Sprintf(format, args...))
}

type AccessItem struct {
	Base
	List model.AccessList
}

type CertItem struct {
	Base
	Cert       model.Certificate
	Chain, Key []byte
	dnsType    string // NPM dns_provider for DNS-01 certificates
}

type HostItem struct {
	Base
	Host      model.ProxyHost
	AccessNPM int
	CertNPM   int
}

type RedirectItem struct {
	Base
	Redirect model.Redirect
	CertNPM  int
}

type StreamItem struct {
	Base
	Stream model.Stream
}

type Plan struct {
	AccessLists []*AccessItem
	Certs       []*CertItem
	Hosts       []*HostItem
	Redirects   []*RedirectItem
	Streams     []*StreamItem
	Warnings    []string
}

// existing is the current Relay state used for conflict detection.
type existing struct {
	hosts       []model.ProxyHost
	redirects   []model.Redirect
	streams     []model.Stream
	accessLists []model.AccessList
	certs       []model.Certificate
	dns         []model.DNSProvider
}

func loadExisting(ctx context.Context, st *store.Store) (*existing, error) {
	e := &existing{}
	var err error
	if e.hosts, err = st.Hosts().List(ctx); err != nil {
		return nil, err
	}
	if e.redirects, err = st.Redirects().List(ctx); err != nil {
		return nil, err
	}
	if e.streams, err = st.Streams().List(ctx); err != nil {
		return nil, err
	}
	if e.accessLists, err = st.AccessLists().List(ctx); err != nil {
		return nil, err
	}
	if e.certs, err = st.Certificates().List(ctx); err != nil {
		return nil, err
	}
	if e.dns, err = st.DNSProviders().List(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

// domainConflict returns a message and (when exactly one entity of wantKind
// owns all overlapping domains) its id.
func (e *existing) domainConflict(domains []string, wantKind string) (string, string) {
	want := map[string]bool{}
	for _, d := range domains {
		want[d] = true
	}
	owners := map[string]string{} // id → kind
	var first string
	for _, h := range e.hosts {
		for _, d := range h.Domains {
			if want[strings.ToLower(d)] {
				owners[h.ID] = model.KindHost
				if first == "" {
					first = fmt.Sprintf("%s already exists as a proxy host", d)
				}
			}
		}
	}
	for _, r := range e.redirects {
		for _, d := range r.Domains {
			if want[strings.ToLower(d)] {
				owners[r.ID] = model.KindRedirect
				if first == "" {
					first = fmt.Sprintf("%s already exists as a redirect", d)
				}
			}
		}
	}
	if len(owners) == 0 {
		return "", ""
	}
	if len(owners) == 1 {
		for id, kind := range owners {
			if kind == wantKind {
				return first, id
			}
		}
	}
	if len(owners) > 1 {
		first += " (and overlaps other entries; resolve manually)"
	}
	return first, ""
}

// BuildPlan reads an NPM database and converts it. dataDir is the NPM data
// folder used to locate certificate files ("" when only the DB was uploaded).
func BuildPlan(ctx context.Context, db *sql.DB, dataDir string, ex *existing) (*Plan, error) {
	if !tableExists(ctx, db, "proxy_host") {
		return nil, errors.New("this is not a Nginx Proxy Manager database (no proxy_host table)")
	}
	p := &Plan{}
	now := time.Now().UTC()

	// Access lists.
	lists, err := readTable(ctx, db, "access_list")
	if err != nil {
		return nil, err
	}
	clients, err := readTable(ctx, db, "access_list_client")
	if err != nil {
		return nil, err
	}
	auths, err := readTable(ctx, db, "access_list_auth")
	if err != nil {
		return nil, err
	}
	usedNames := map[string]bool{}
	for _, r := range lists {
		it := convertAccessList(r, clients, auths)
		// Sanitised names can collide within one import (Relay requires
		// unique names); existing Relay lists are handled as conflicts.
		if base := it.Name; usedNames[strings.ToLower(base)] {
			for n := 2; usedNames[strings.ToLower(it.Name)]; n++ {
				it.Name = fmt.Sprintf("%s-%d", base, n)
			}
			it.List.Name = it.Name
			it.warn("renamed to %s because another NPM access list has the same name", it.Name)
		}
		usedNames[strings.ToLower(it.Name)] = true
		for _, l := range ex.accessLists {
			if strings.EqualFold(l.Name, it.List.Name) {
				it.Conflict = "an access list named " + l.Name + " already exists"
				it.ExistingID = l.ID
			}
		}
		p.AccessLists = append(p.AccessLists, it)
	}

	// Certificates.
	certs, err := readTable(ctx, db, "certificate")
	if err != nil {
		return nil, err
	}
	for _, r := range certs {
		it := convertCert(r, dataDir, now)
		if it.Cert.Challenge == model.ChallengeDNS01 {
			for _, d := range ex.dns {
				if strings.EqualFold(d.Type, it.dnsType) || strings.HasPrefix(strings.ToLower(it.dnsType), strings.ToLower(d.Type)) {
					it.Cert.DNSProviderID = d.ID
					break
				}
			}
			if it.Cert.DNSProviderID == "" {
				it.warn("no %s DNS provider in Relay: add it under Settings → Default TLS first, otherwise this certificate is skipped", orDash(it.dnsType))
			} else {
				it.Warnings = slices.DeleteFunc(it.Warnings, func(w string) bool { return strings.HasPrefix(w, "uses DNS-01") })
			}
		}
		sortedWant := sortedCopy(it.Cert.Domains)
		for _, c := range ex.certs {
			sameFile := it.Cert.Fingerprint != "" && strings.EqualFold(c.Fingerprint, it.Cert.Fingerprint)
			if sameFile || strings.Join(sortedCopy(lower(c.Domains)), ",") == strings.Join(sortedWant, ",") {
				it.Conflict = "a certificate for " + strings.Join(c.Domains, ", ") + " already exists"
				it.ExistingID = c.ID
			}
		}
		p.Certs = append(p.Certs, it)
	}

	// Proxy hosts.
	hosts, err := readTable(ctx, db, "proxy_host")
	if err != nil {
		return nil, err
	}
	for _, r := range hosts {
		it := convertHost(r)
		it.Conflict, it.ExistingID = ex.domainConflict(it.Host.Domains, model.KindHost)
		p.Hosts = append(p.Hosts, it)
	}

	// Redirection hosts.
	redirects, err := readTable(ctx, db, "redirection_host")
	if err != nil {
		return nil, err
	}
	for _, r := range redirects {
		it := convertRedirect(r)
		it.Conflict, it.ExistingID = ex.domainConflict(it.Redirect.Domains, model.KindRedirect)
		p.Redirects = append(p.Redirects, it)
	}

	// Streams.
	streams, err := readTable(ctx, db, "stream")
	if err != nil {
		return nil, err
	}
	for _, r := range streams {
		it := convertStream(r)
		for _, s := range ex.streams {
			if s.ListenPorts == it.Stream.ListenPorts && protocolsOverlap(s.Protocol, it.Stream.Protocol) {
				it.Conflict = fmt.Sprintf("port %s is already used by stream %s", s.ListenPorts, s.Name)
				it.ExistingID = s.ID
			}
		}
		p.Streams = append(p.Streams, it)
	}

	if tableExists(ctx, db, "dead_host") {
		if dead, err := readTable(ctx, db, "dead_host"); err == nil && len(dead) > 0 {
			p.Warnings = append(p.Warnings, fmt.Sprintf("%d 404 hosts are not imported (use Settings → Default host instead)", len(dead)))
		}
	}
	missing := 0
	for _, c := range p.Certs {
		if c.Chain == nil {
			missing++
		}
	}
	if dataDir == "" && missing > 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("Only the database was uploaded, so %d certificate files are not available: Let's Encrypt certificates are created as pending and must be requested again. Point Relay at the NPM data folder to import the files.", missing))
	}
	return p, nil
}

func lower(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.ToLower(s)
	}
	return out
}

func sortedCopy(ss []string) []string {
	c := append([]string(nil), ss...)
	sort.Strings(c)
	return c
}

func protocolsOverlap(a, b string) bool {
	return a == b || a == "both" || b == "both"
}

// ---------------------------------------------------------------- converters

func convertAccessList(r row, clients, auths []row) *AccessItem {
	id := r.num("id")
	it := &AccessItem{Base: Base{NPMID: id, Name: accessListName(r.str("name"), id), Warnings: []string{}}}
	if orig := strings.TrimSpace(r.str("name")); orig != "" && orig != it.Name {
		it.warn("renamed from %q: Relay allows letters, digits, space, dot, dash and underscore", orig)
	}
	l := model.AccessList{
		Name:        it.Name,
		Description: "Imported from Nginx Proxy Manager",
		Rules:       []model.IPRule{},
		BasicAuth:   model.BasicAuth{Realm: "Restricted", Users: []model.BasicAuthUser{}},
		SatisfyAny:  r.flag("satisfy_any"),
	}
	hasAll := false
	for _, c := range clients {
		if c.num("access_list_id") != id {
			continue
		}
		action := strings.ToLower(c.str("directive"))
		if action != "allow" && action != "deny" {
			action = "allow"
		}
		addr := strings.TrimSpace(c.str("address"))
		if addr == "" {
			continue
		}
		if addr == "all" {
			hasAll = true
		}
		l.Rules = append(l.Rules, model.IPRule{ID: store.NewID(), Action: action, CIDR: addr})
	}
	if len(l.Rules) > 0 && !hasAll {
		// NPM ends every client list with "deny all".
		l.Rules = append(l.Rules, model.IPRule{ID: store.NewID(), Action: "deny", CIDR: "all", Note: "NPM default"})
	}
	hadUsers := false
	for _, a := range auths {
		if a.num("access_list_id") != id {
			continue
		}
		user := strings.TrimSpace(a.str("username"))
		// NPM stores basic-auth passwords in plain text (it only hashes them
		// into its htpasswd files); they are bcrypt-hashed on commit.
		pass := a.str("password")
		if user == "" {
			continue
		}
		hadUsers = true
		if !basicAuthUserRe.MatchString(user) {
			it.warn("user %q has characters Relay does not allow in user names and was skipped", user)
			continue
		}
		if pass == "" {
			it.warn("user %s has no password and was skipped", user)
			continue
		}
		u := model.BasicAuthUser{Username: user}
		if strings.HasPrefix(pass, "$2a$") || strings.HasPrefix(pass, "$2b$") || strings.HasPrefix(pass, "$2y$") {
			u.PasswordHash = pass
		} else {
			u.Password = pass // hashed on commit
		}
		l.BasicAuth.Users = append(l.BasicAuth.Users, u)
	}
	// A list whose users were all skipped keeps basic auth on, so it fails
	// validation on commit (and its hosts are skipped) instead of silently
	// becoming an IP-only or empty list.
	l.BasicAuth.Enabled = len(l.BasicAuth.Users) > 0 || hadUsers
	// NPM renders "satisfy any|all" unconditionally; it only has an effect
	// with both IP rules and users, and Relay requires basic auth for it.
	l.SatisfyAny = l.SatisfyAny && l.BasicAuth.Enabled
	// NPM strips the Authorization header unless "Pass Auth to upstream" is
	// on; Relay (like plain nginx) always passes it on.
	if len(l.BasicAuth.Users) > 0 && r.has("pass_auth") && !r.flag("pass_auth") {
		it.warn("NPM removed the Authorization header before proxying (Pass Auth off); Relay passes it to the upstream")
	}
	it.List = l
	it.Detail = fmt.Sprintf("%d rules · %d users", len(l.Rules), len(l.BasicAuth.Users))
	return it
}

var (
	accessListNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$`)
	accessListBadRe  = regexp.MustCompile(`[^A-Za-z0-9 ._-]+`)
	basicAuthUserRe  = regexp.MustCompile(`^[A-Za-z0-9._@+-]{1,64}$`)
	// NPM skips its default "/" location when the advanced config has one.
	defaultLocationRe = regexp.MustCompile(`(?m)^(?:.*;)?\s*location\s*/\s*\{`)
)

// accessListName converts an NPM access list name to one Relay accepts.
func accessListName(name string, id int) string {
	name = strings.TrimSpace(name)
	if accessListNameRe.MatchString(name) {
		return name
	}
	clean := strings.Join(strings.Fields(accessListBadRe.ReplaceAllString(name, " ")), " ")
	clean = strings.TrimLeft(clean, " ._-")
	if len(clean) > 64 {
		clean = strings.TrimRight(clean[:64], " ")
	}
	if !accessListNameRe.MatchString(clean) {
		return fmt.Sprintf("npm-access-%d", id)
	}
	return clean
}

// npmOffline returns NPM's own nginx error for an entity it could not load.
func npmOffline(meta map[string]any) string {
	if online, ok := meta["nginx_online"].(bool); !ok || online {
		return ""
	}
	msg, _ := meta["nginx_err"].(string)
	msg, _, _ = strings.Cut(strings.TrimSpace(msg), "\n")
	msg = strings.TrimPrefix(msg, "nginx: ")
	if msg == "" {
		msg = "unknown error"
	}
	return "NPM reported this entry as offline (" + msg + "); check the target before applying"
}

func metaMap(r row) map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal([]byte(r.str("meta")), &m)
	return m
}

func convertCert(r row, dataDir string, now time.Time) *CertItem {
	id := r.num("id")
	domains := domainList(r.str("domain_names"))
	it := &CertItem{Base: Base{NPMID: id, Warnings: []string{}}}
	name := strings.TrimSpace(r.str("nice_name"))
	if name == "" && len(domains) > 0 {
		name = domains[0]
	}
	it.Name = name
	meta := metaMap(r)
	c := model.Certificate{
		Name: name, Domains: domains, Challenge: model.ChallengeHTTP01, History: []model.CertEvent{},
	}
	switch strings.ToLower(r.str("provider")) {
	case "letsencrypt":
		c.Provider = model.CertLetsEncrypt
		c.AutoRenew = true
		if v, ok := meta["dns_challenge"].(bool); ok && v {
			c.Challenge = model.ChallengeDNS01
			provider, _ := meta["dns_provider"].(string)
			it.dnsType = provider
			it.warn("uses DNS-01 via %s: connect the DNS provider in Relay before the next renewal", orDash(provider))
		}
	default:
		c.Provider = model.CertCustom
	}
	var dirs []string
	if dataDir != "" {
		if c.Provider == model.CertLetsEncrypt {
			sub := fmt.Sprintf("npm-%d", id)
			dirs = []string{
				filepath.Join(dataDir, "letsencrypt", "live", sub),
				filepath.Join(filepath.Dir(dataDir), "letsencrypt", "live", sub),
				filepath.Join("/etc/letsencrypt/live", sub),
			}
		} else {
			dirs = []string{filepath.Join(dataDir, "custom_ssl", fmt.Sprintf("npm-%d", id))}
		}
	}
	for _, d := range dirs {
		chain, err1 := os.ReadFile(filepath.Join(d, "fullchain.pem"))
		key, err2 := os.ReadFile(filepath.Join(d, "privkey.pem"))
		if err1 == nil && err2 == nil {
			it.Chain, it.Key = chain, key
			break
		}
	}
	if it.Chain == nil && c.Provider == model.CertCustom {
		// NPM also keeps uploaded custom certificates in the database, so
		// they survive an upload of just database.sqlite.
		crt, _ := meta["certificate"].(string)
		key, _ := meta["certificate_key"].(string)
		if strings.Contains(crt, "BEGIN CERTIFICATE") && strings.Contains(key, "PRIVATE KEY") {
			chain := strings.TrimRight(crt, "\r\n") + "\n"
			if inter, _ := meta["intermediate_certificate"].(string); strings.Contains(inter, "BEGIN CERTIFICATE") {
				chain += strings.TrimRight(inter, "\r\n") + "\n"
			}
			it.Chain, it.Key = []byte(chain), []byte(strings.TrimRight(key, "\r\n")+"\n")
		}
	}
	parseFailed := false
	if it.Chain != nil {
		if err := fillCertInfo(&c, it.Chain, now); err != nil {
			it.warn("certificate file could not be parsed: %v", err)
			it.Chain, it.Key = nil, nil
			parseFailed = true
		}
	}
	if it.Chain == nil {
		if parseFailed {
			it.Warnings = append(it.Warnings, "created without files: request or upload it again after import")
		}
		c.Status = model.CertStatusPending
		if c.Provider == model.CertLetsEncrypt {
			c.LastError = "Imported from Nginx Proxy Manager without certificate files — request it again"
			if !parseFailed {
				it.warn("certificate files not found: created as pending, request it again after import")
			}
		} else {
			c.Status = model.CertStatusFailed
			c.LastError = "Imported from Nginx Proxy Manager without certificate files — upload the certificate again"
			if !parseFailed {
				it.warn("custom certificate files not found: upload them again after import")
			}
		}
	}
	c.History = append(c.History, model.CertEvent{At: now, Message: "Imported from Nginx Proxy Manager", Result: "ok"})
	it.Cert = c
	it.Detail = strings.Join(c.Domains, ", ")
	if len(c.Domains) == 0 {
		it.warn("certificate has no domains")
	}
	return it
}

func orDash(s string) string {
	if s == "" {
		return "an unknown provider"
	}
	return s
}

// fillCertInfo parses a PEM chain into certificate metadata.
func fillCertInfo(c *model.Certificate, chain []byte, now time.Time) error {
	var certs []*x509.Certificate
	rest := chain
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		x, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return err
		}
		certs = append(certs, x)
	}
	if len(certs) == 0 {
		return errors.New("no certificate in fullchain.pem")
	}
	leaf := certs[0]
	nb, na := leaf.NotBefore.UTC(), leaf.NotAfter.UTC()
	c.NotBefore, c.NotAfter = &nb, &na
	c.Issuer = leaf.Issuer.CommonName
	if c.Issuer == "" && len(leaf.Issuer.Organization) > 0 {
		c.Issuer = leaf.Issuer.Organization[0]
	}
	sum := sha256.Sum256(leaf.Raw)
	hexes := make([]string, len(sum))
	for i, b := range sum {
		hexes[i] = fmt.Sprintf("%02X", b)
	}
	c.Fingerprint = strings.Join(hexes, ":")
	switch k := leaf.PublicKey.(type) {
	case *rsa.PublicKey:
		c.KeyType = fmt.Sprintf("RSA %d", k.N.BitLen())
	case *ecdsa.PublicKey:
		c.KeyType = "ECDSA " + k.Curve.Params().Name
	case ed25519.PublicKey:
		c.KeyType = "Ed25519"
	}
	c.Chain = []string{}
	for _, x := range certs {
		c.Chain = append(c.Chain, x.Subject.CommonName)
	}
	// NPM derives a custom certificate's domain_names from the subject CN
	// only; the SANs are what the certificate really covers.
	if (len(c.Domains) == 0 || c.Provider == model.CertCustom) && len(leaf.DNSNames) > 0 {
		c.Domains = lower(leaf.DNSNames)
	}
	c.Status = model.CertStatusValid
	if now.After(na) {
		c.Status = model.CertStatusExpired
	}
	return nil
}

type npmLocation struct {
	Path           string `json:"path"`
	AdvancedConfig string `json:"advanced_config"`
	ForwardScheme  string `json:"forward_scheme"`
	ForwardHost    string `json:"forward_host"`
	ForwardPort    any    `json:"forward_port"`
}

func anyInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

// splitHostPath turns NPM's "10.0.0.5/api" forward host into host + path.
func splitHostPath(h string) (string, string) {
	h = strings.TrimSpace(h)
	if i := strings.Index(h, "/"); i > 0 {
		return h[:i], h[i:]
	}
	return h, ""
}

func scheme(s string) string {
	if strings.ToLower(s) == "https" {
		return "https"
	}
	return "http"
}

func convertHost(r row) *HostItem {
	domains := domainList(r.str("domain_names"))
	it := &HostItem{Base: Base{NPMID: r.num("id"), Warnings: []string{}}, AccessNPM: r.num("access_list_id"), CertNPM: r.num("certificate_id")}
	if len(domains) > 0 {
		it.Name = domains[0]
	}
	host, path := splitHostPath(r.str("forward_host"))
	h := model.ProxyHost{
		Domains:       domains,
		Enabled:       r.enabled(),
		Upstream:      model.Upstream{Scheme: scheme(r.str("forward_scheme")), Host: host, Port: r.num("forward_port"), Path: path},
		Websockets:    r.flag("allow_websocket_upgrade"),
		BlockExploits: r.flag("block_exploits"),
		CacheAssets:   r.flag("caching_enabled"),
		ForceHTTPS:    r.flag("ssl_forced"),
		HTTP2:         r.flag("http2_support"),
		HSTS:          "inherit",
		Locations:     []model.Location{},
		GeoBlock:      model.GeoBlock{AllowCountries: []string{}},
		CustomNginx:   strings.TrimSpace(r.str("advanced_config")),
		Source:        model.SourceImport,
		SourceRef:     fmt.Sprintf("npm:proxy_host:%d", r.num("id")),
	}
	// NPM only sends HSTS on hosts with a certificate and Force SSL.
	if r.flag("hsts_enabled") && r.flag("ssl_forced") {
		h.HSTS = "on"
		if r.flag("hsts_subdomains") {
			it.warn("HSTS includeSubDomains is controlled globally in Relay (Settings → Default TLS)")
		}
	}
	if r.flag("trust_forwarded_proto") {
		it.warn("\"Trust upstream forwarded proto headers\" is not supported: Relay sends its own X-Forwarded-Proto")
	}
	if h.CustomNginx != "" {
		h.CustomNginx = liftDirectives(&h, h.CustomNginx, it)
	}
	if h.CustomNginx != "" {
		it.warn("custom nginx configuration was copied as-is — review it before applying (Relay Edge does not run it)")
		if defaultLocationRe.MatchString(h.CustomNginx) {
			it.warn("the custom nginx configuration defines location /, which clashes with Relay's own / location: move it into Locations before applying")
		}
	}
	if msg := npmOffline(metaMap(r)); msg != "" {
		it.warn("%s", msg)
	}
	var locs []npmLocation
	if s := r.str("locations"); s != "" && s != "null" {
		if err := json.Unmarshal([]byte(s), &locs); err != nil {
			it.warn("custom locations could not be read: %v", err)
		}
	}
	for _, l := range locs {
		lh, lp := splitHostPath(l.ForwardHost)
		up := model.Upstream{Scheme: scheme(l.ForwardScheme), Host: lh, Port: anyInt(l.ForwardPort), Path: lp}
		path := strings.TrimSpace(l.Path)
		switch {
		case path == "":
			continue
		case path == "/":
			// NPM drops its default location when a custom "/" exists.
			h.Upstream = up
			it.warn("custom location / became the host's forward target")
		case !strings.HasPrefix(path, "/") || strings.ContainsAny(path, " \t\r\n\"'{};\\"):
			it.warn("location %s uses an nginx modifier or characters Relay does not support and was skipped", path)
			continue
		default:
			h.Locations = append(h.Locations, model.Location{
				ID: store.NewID(), Path: path, Kind: model.LocationProxy, Upstream: up,
				// NPM renders "proxy_pass scheme://host:port/path", which
				// replaces the location prefix with the path.
				StripPrefix: lp != "",
				Websockets:  h.Websockets, Headers: []model.Header{},
			})
		}
		if strings.TrimSpace(l.AdvancedConfig) != "" {
			it.warn("location %s has custom nginx configuration that was not imported", path)
		}
	}
	it.Host = h
	it.Detail = fmt.Sprintf("%s://%s:%d%s", h.Upstream.Scheme, h.Upstream.Host, h.Upstream.Port, h.Upstream.Path)
	return it
}

var (
	directiveRe = regexp.MustCompile(`^(client_max_body_size|proxy_read_timeout|proxy_send_timeout)\s+([^;\s]+)\s*;\s*(#.*)?$`)
	durationRe  = regexp.MustCompile(`^([0-9]+)(ms|s|m|h)?$`)
)

// liftDirectives moves top-level advanced_config directives Relay has host
// settings for (body size, proxy timeouts) into those settings, so they work
// with nginx and Relay Edge alike. It returns the configuration left over.
func liftDirectives(h *model.ProxyHost, conf string, it *HostItem) string {
	var keep []string
	depth := 0
	for _, line := range strings.Split(strings.ReplaceAll(conf, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if depth == 0 {
			if m := directiveRe.FindStringSubmatch(t); m != nil && liftDirective(h, m[1], strings.ToLower(m[2]), it) {
				continue
			}
		}
		depth += strings.Count(t, "{") - strings.Count(t, "}")
		if depth < 0 {
			depth = 0
		}
		keep = append(keep, line)
	}
	out := strings.TrimSpace(strings.Join(keep, "\n"))
	// Only comments left: nothing to run.
	onlyComments := true
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			onlyComments = false
			break
		}
	}
	if onlyComments {
		return ""
	}
	return out
}

func liftDirective(h *model.ProxyHost, name, value string, it *HostItem) bool {
	switch name {
	case "client_max_body_size":
		if !regexp.MustCompile(`^[0-9]+[kmg]?$`).MatchString(value) {
			return false
		}
		h.MaxBodySize = value
		it.warn("client_max_body_size %s became the host's Max body size", value)
		return true
	default:
		m := durationRe.FindStringSubmatch(value)
		if m == nil {
			return false
		}
		n, _ := strconv.Atoi(m[1])
		switch m[2] {
		case "ms":
			n = (n + 999) / 1000
		case "m":
			n *= 60
		case "h":
			n *= 3600
		}
		if n <= 0 || n > 86400 {
			return false
		}
		if name == "proxy_read_timeout" {
			h.ProxyReadTimeout = n
		} else {
			h.ProxySendTimeout = n
		}
		it.warn("%s %s became the host's %s timeout (%ds)", name, value, strings.TrimPrefix(strings.TrimSuffix(name, "_timeout"), "proxy_"), n)
		return true
	}
}

func convertRedirect(r row) *RedirectItem {
	domains := domainList(r.str("domain_names"))
	it := &RedirectItem{Base: Base{NPMID: r.num("id"), Warnings: []string{}}, CertNPM: r.num("certificate_id")}
	if len(domains) > 0 {
		it.Name = domains[0]
	}
	target := strings.TrimSpace(r.str("forward_domain_name"))
	sch := strings.ToLower(r.str("forward_scheme"))
	switch sch {
	case "http", "https":
	case "", "auto", "$scheme":
		sch = "https"
		if r.has("forward_scheme") {
			it.warn("scheme \"auto\" was converted to https")
		}
	default:
		sch = "https"
	}
	to := target
	if !strings.Contains(target, "://") {
		to = sch + "://" + target
	}
	code := r.num("forward_http_code")
	switch code {
	case 301, 302, 307, 308:
	case 0:
		code = 301
	default:
		it.warn("HTTP %d is not supported, using 302", code)
		code = 302
	}
	it.Redirect = model.Redirect{
		Domains: domains, FromPath: "", To: to, Code: code, KeepPath: r.flag("preserve_path"),
		ForceHTTPS: r.flag("ssl_forced"), Enabled: r.enabled(),
	}
	if strings.TrimSpace(r.str("advanced_config")) != "" {
		it.warn("custom nginx configuration was not imported")
	}
	if r.flag("hsts_enabled") && r.flag("ssl_forced") {
		it.warn("HSTS on redirects is controlled globally in Relay (Settings → Default TLS)")
	}
	if msg := npmOffline(metaMap(r)); msg != "" {
		it.warn("%s", msg)
	}
	it.Detail = fmt.Sprintf("%d → %s", code, to)
	return it
}

func convertStream(r row) *StreamItem {
	it := &StreamItem{Base: Base{NPMID: r.num("id"), Warnings: []string{}}}
	tcp, udp := r.flag("tcp_forwarding"), r.flag("udp_forwarding")
	proto := "tcp"
	switch {
	case tcp && udp:
		proto = "both"
	case udp:
		proto = "udp"
	case !tcp:
		it.warn("neither TCP nor UDP forwarding was enabled; imported as TCP")
	}
	in := r.num("incoming_port")
	fwdPort := r.num("forwarding_port")
	name := fmt.Sprintf("npm-stream-%d", in)
	fwd := strconv.Itoa(fwdPort)
	if fwdPort == in {
		fwd = ""
	}
	it.Stream = model.Stream{
		Name: name, Protocol: proto, ListenAddress: "0.0.0.0", ListenPorts: strconv.Itoa(in),
		ForwardHost: strings.TrimSpace(r.str("forwarding_host")), ForwardPorts: fwd, IdleTimeout: "10m", Enabled: r.enabled(),
	}
	if r.num("certificate_id") > 0 {
		it.warn("TLS termination for streams is not supported; imported as plain %s", proto)
	}
	if msg := npmOffline(metaMap(r)); msg != "" {
		it.warn("%s", msg)
	}
	it.Name = name
	it.Detail = fmt.Sprintf("%s :%d → %s:%d", proto, in, it.Stream.ForwardHost, fwdPort)
	return it
}
