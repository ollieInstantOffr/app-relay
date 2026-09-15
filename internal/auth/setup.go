package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// ipifyURL is the public IP echo service used by the setup network check.
var ipifyURL = "https://api.ipify.org"

type publicIPCache struct {
	mu         sync.Mutex
	ip         string
	at         time.Time // last success
	tried      time.Time // last attempt
	refreshing bool
}

func (s *Service) cachedPublicIP() string {
	s.publicIP.mu.Lock()
	defer s.publicIP.mu.Unlock()
	return s.publicIP.ip
}

func (s *Service) detectPublicIP(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	s.publicIP.mu.Lock()
	s.publicIP.tried = s.now()
	s.publicIP.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ipifyURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "relay/"+s.app.Config.Version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(string(body)))
	if err != nil {
		return "", errors.New("unexpected response")
	}
	s.publicIP.mu.Lock()
	s.publicIP.ip, s.publicIP.at = addr.String(), s.now()
	s.publicIP.mu.Unlock()
	return addr.String(), nil
}

// refreshPublicIPAsync re-detects the public IP in the background (hourly on
// success, at most every 5 minutes after a failure).
func (s *Service) refreshPublicIPAsync() {
	c := &s.publicIP
	now := s.now()
	c.mu.Lock()
	fresh := (c.ip != "" && now.Sub(c.at) < time.Hour) || now.Sub(c.tried) < 5*time.Minute
	if c.refreshing || fresh {
		c.mu.Unlock()
		return
	}
	c.refreshing = true
	c.mu.Unlock()
	go func() {
		if _, err := s.detectPublicIP(context.Background()); err != nil {
			s.app.Log.Debug("auth: public IP detection failed", "err", err)
		}
		c.mu.Lock()
		c.refreshing = false
		c.mu.Unlock()
	}()
}

// ---------------------------------------------------------------- step 1: admin

type setupAdminRequest struct {
	Username  string `json:"username"`
	Password  string `json:"password"`
	Enroll2FA bool   `json:"enroll2fa"`
}

// handleSetupAdmin creates the first admin and signs them in. It only works
// while there are no users; afterwards it returns 403.
func (s *Service) handleSetupAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.originOK(r) {
		httpx.WriteError(w, http.StatusForbidden, "cross_origin", "cross-origin request refused")
		return
	}
	ctx := r.Context()
	if n, err := s.app.Store.CountUsers(ctx); err != nil {
		httpx.Fail(w, r, err)
		return
	} else if n > 0 {
		httpx.WriteError(w, http.StatusForbidden, "setup_done", "Setup is already complete")
		return
	}
	var req setupAdminRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	uname := strings.TrimSpace(req.Username)
	e := model.Errs{}
	if msg := usernameError(uname); msg != "" {
		e.Add("username", "%s", msg)
	}
	if msg := passwordError(req.Password, uname); msg != "" {
		e.Add("password", "%s", msg)
	}
	if err := e.Err(); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	u := &store.User{Username: uname, Role: core.RoleAdmin, PasswordHash: hash}
	ok, err := s.app.Store.CreateFirstUser(ctx, u)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if !ok {
		httpx.WriteError(w, http.StatusForbidden, "setup_done", "Setup is already complete")
		return
	}
	actor := userActor(u, core.ClientIP(r), "")
	detail := "first admin · first-run setup"
	if req.Enroll2FA {
		detail += " · 2FA enrolment requested"
	}
	s.app.Audit(ctx, core.AuditEntry{Actor: &actor, Action: "user.create", Target: u.Username, Detail: detail})
	s.completeLogin(w, r, u, "setup", false)
}

// requireSetupAdmin: a signed-in admin, while general.setupDone is false.
func (s *Service) requireSetupAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a := httpx.Actor(r)
		switch {
		case a.Type != core.ActorUser:
			httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "sign in required")
		case !a.IsAdmin():
			httpx.WriteError(w, http.StatusForbidden, "admin_only", "only admins can do this")
		case s.general(r.Context()).SetupDone:
			httpx.WriteError(w, http.StatusForbidden, "setup_done", "Setup is already complete — use Settings instead")
		default:
			h(w, r)
		}
	}
}

// ---------------------------------------------------------------- step 2: network

type setupCheck struct {
	ID     string `json:"id"`
	Status string `json:"status"` // ok | warn | fail | unknown
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

type networkReport struct {
	Checks         []setupCheck `json:"checks"`
	LANCIDR        string       `json:"lanCidr"`
	LANDetected    bool         `json:"lanDetected"`
	PublicIP       string       `json:"publicIp"`
	AdminDomain    string       `json:"adminDomain"`
	Containers     *int         `json:"containers"`
	HTTPContainers *int         `json:"httpContainers"`
}

func (s *Service) handleSetupNetworkGet(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	g := s.general(ctx)
	lan, detected := detectLAN()
	if !detected {
		lan = g.LANCIDR
	}
	rep := networkReport{LANCIDR: lan, LANDetected: detected, AdminDomain: g.AdminDomain}
	checks := make([]setupCheck, 5)
	checks[1] = lanCheck(lan, detected)
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); checks[0] = s.checkPorts(ctx, g) }()
	go func() { defer wg.Done(); checks[2], rep.Containers, rep.HTTPContainers = s.checkDocker(ctx) }()
	go func() { defer wg.Done(); checks[3], rep.PublicIP = s.checkPublic(ctx, g) }()
	go func() { defer wg.Done(); checks[4] = s.checkHAProxy(ctx) }()
	wg.Wait()
	rep.Checks = checks
	httpx.WriteJSON(w, http.StatusOK, rep)
}

func shortErr(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && len(msg)-i > 8 {
		msg = msg[i+2:]
	}
	return truncate(msg, 160)
}

func (s *Service) checkPorts(ctx context.Context, g model.GeneralSettings) setupCheck {
	ports := []int{g.HTTPPort, g.HTTPSPort}
	label := fmt.Sprintf("Ports %d & %d", g.HTTPPort, g.HTTPSPort)
	owners := map[int]string{}
	agentOK := false
	if s.app.Nginx != nil {
		lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		res, err := s.app.Nginx.Listeners(lctx)
		cancel()
		if err == nil && res != nil {
			agentOK = true
			for _, l := range res.Listeners {
				if l.Proto != "" && l.Proto != "tcp" {
					continue
				}
				for _, p := range ports {
					if l.Port == p {
						owners[p] = l.Process
						if owners[p] == "" {
							owners[p] = "unknown process"
						}
					}
				}
			}
		}
	}
	if !agentOK {
		// Host networking: probe the ports from this process instead.
		var unknown []string
		for _, p := range ports {
			ln, err := net.Listen("tcp", ":"+strconv.Itoa(p))
			if err == nil {
				ln.Close()
				continue
			}
			if errors.Is(err, syscall.EADDRINUSE) {
				owners[p] = "another process"
			} else {
				unknown = append(unknown, fmt.Sprintf("%d (%s)", p, shortErr(err)))
			}
		}
		if len(unknown) > 0 && len(owners) == 0 {
			return setupCheck{ID: "ports", Status: "unknown", Title: "Couldn't check " + strings.ToLower(label[:1]) + label[1:],
				Detail: "The nginx agent isn't reachable and Relay can't probe " + strings.Join(unknown, ", ")}
		}
	}
	var busy []string
	nginxOwned := 0
	for _, p := range ports {
		owner, ok := owners[p]
		if !ok {
			continue
		}
		if strings.Contains(strings.ToLower(owner), "nginx") {
			nginxOwned++
			continue
		}
		busy = append(busy, fmt.Sprintf("%d (%s)", p, owner))
	}
	switch {
	case len(busy) > 0:
		return setupCheck{ID: "ports", Status: "fail", Title: "In use: port " + strings.Join(busy, ", "),
			Detail: "nginx can't bind while another process holds it — stop that process or change the ports in Settings → General"}
	case nginxOwned == len(ports):
		return setupCheck{ID: "ports", Status: "ok", Title: label + " are served by nginx", Detail: "nginx is already listening on both"}
	case !agentOK:
		return setupCheck{ID: "ports", Status: "ok", Title: label + " are free", Detail: "nginx will bind both · nginx agent not reachable yet"}
	default:
		return setupCheck{ID: "ports", Status: "ok", Title: label + " are free", Detail: "nginx will bind both"}
	}
}

var virtualIfacePrefixes = []string{"docker", "br-", "veth", "virbr", "cni", "flannel", "cali", "vxlan", "tun", "tap", "wg",
	"tailscale", "zt", "utun", "bridge", "podman", "lxc", "lxd", "vmnet", "vboxnet", "kube", "weave", "awdl", "llw", "anpi", "ap"}

func virtualInterface(name string) bool {
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// detectLAN returns the first private IPv4 network of a physical-looking interface.
func detectLAN() (string, bool) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", false
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || virtualInterface(ifc.Name) {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || !ip4.IsPrivate() {
				continue
			}
			ones, bits := ipn.Mask.Size()
			if bits != 32 || ones == 0 {
				continue
			}
			return netip.PrefixFrom(netip.AddrFrom4([4]byte(ip4)), ones).Masked().String(), true
		}
	}
	return "", false
}

func lanCheck(lan string, detected bool) setupCheck {
	if detected {
		return setupCheck{ID: "lan", Status: "ok", Title: "LAN detected · " + lan, Detail: "Used for the default lan-only access list"}
	}
	return setupCheck{ID: "lan", Status: "warn", Title: "LAN not detected", Detail: "Enter your LAN range below — it's used for the default lan-only access list"}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func (s *Service) checkDocker(ctx context.Context) (setupCheck, *int, *int) {
	if s.app.Docker == nil {
		return setupCheck{ID: "docker", Status: "unknown", Title: "Docker discovery unavailable", Detail: "The Docker service isn't running in this build"}, nil, nil
	}
	dctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	cs, err := s.app.Docker.Containers(dctx)
	if err != nil {
		return setupCheck{ID: "docker", Status: "warn", Title: "Docker socket not found",
			Detail: shortErr(err) + " · mount /var/run/docker.sock to suggest containers as upstreams"}, nil, nil
	}
	n, h := len(cs), 0
	for _, c := range cs {
		if c.HTTP && c.HostID == "" && c.BackendID == "" && c.UpstreamHost != "" && c.State == "running" {
			h++
		}
	}
	ds, _ := store.LoadSettings[model.DockerSettings](ctx, s.app.Store, model.SettingsDocker)
	return setupCheck{ID: "docker", Status: "ok", Title: fmt.Sprintf("Docker socket found · %d containers", n),
		Detail: "Discovery " + onOff(ds.Enabled) + ", auto-create " + onOff(ds.AutoCreate)}, &n, &h
}

func (s *Service) checkPublic(ctx context.Context, g model.GeneralSettings) (setupCheck, string) {
	ip, err := s.detectPublicIP(ctx)
	if err != nil {
		return setupCheck{ID: "public", Status: "unknown", Title: "Public IP unknown",
			Detail: fmt.Sprintf("Couldn't reach api.ipify.org (%s). HTTP-01 certificates need ports %d/%d forwarded to this machine.", shortErr(err), g.HTTPPort, g.HTTPSPort)}, ""
	}
	port := g.HTTPSPort
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err == nil {
		conn.Close()
		return setupCheck{ID: "public", Status: "ok", Title: fmt.Sprintf("Public IP %s · port %d answered", ip, port),
			Detail: "Tested from this machine — something answered on the public address, but only a check from outside proves your router forwards it"}, ip
	}
	return setupCheck{ID: "public", Status: "warn", Title: fmt.Sprintf("Public IP %s · port %d not verified from outside", ip, port),
		Detail: "Forward 80/443 on your router, or use DNS-01 certs and a VPN instead. Routers without hairpin NAT fail this check even when forwarding works."}, ip
}

func (s *Service) checkHAProxy(ctx context.Context) setupCheck {
	if s.app.HAProxy == nil {
		return setupCheck{ID: "haproxy", Status: "unknown", Title: "HAProxy engine", Detail: "Agent not configured"}
	}
	hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	st, err := s.app.HAProxy.Status(hctx)
	if err != nil {
		return setupCheck{ID: "haproxy", Status: "warn", Title: "HAProxy agent not reachable",
			Detail: shortErr(err) + " · is the relay-haproxy container running?"}
	}
	detail := "Installed"
	if st.Version != "" {
		detail += " · " + st.Version
	}
	if st.Running {
		detail += " · running"
	} else {
		detail += " · starts when you create the first backend"
	}
	return setupCheck{ID: "haproxy", Status: "ok", Title: "HAProxy engine", Detail: detail}
}

type networkRequest struct {
	LANCIDR     string `json:"lanCidr"`
	AdminDomain string `json:"adminDomain"`
}

type networkResult struct {
	AccessListID    string `json:"accessListId"`
	AdminHostID     string `json:"adminHostId,omitempty"`
	AdminRestricted bool   `json:"adminRestricted"`
	LANCIDR         string `json:"lanCidr"`
	AdminDomain     string `json:"adminDomain"`
}

func (s *Service) handleSetupNetworkPost(w http.ResponseWriter, r *http.Request) {
	var req networkRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	ctx := r.Context()
	e := model.Errs{}
	prefix, err := netip.ParsePrefix(strings.TrimSpace(req.LANCIDR))
	switch {
	case err != nil:
		e.Add("lanCidr", "Enter a network like 192.168.1.0/24")
	case prefix.Masked() != prefix:
		e.Add("lanCidr", "Host bits are set — did you mean %s?", prefix.Masked())
	}
	domain := model.HostNormalizeDomain(req.AdminDomain)
	if domain != "" {
		if msg := adminDomainError(domain); msg != "" {
			e.Add("adminDomain", "%s", msg)
		} else if name, err := s.adminDomainConflict(ctx, domain); err != nil {
			httpx.Fail(w, r, err)
			return
		} else if name != "" {
			e.Add("adminDomain", "Already used by proxy host %s", name)
		}
	}
	if err := e.Err(); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	acl, err := s.upsertLANList(r, prefix.String())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	g := s.general(ctx)
	g.LANCIDR = prefix.String()
	g.AdminDomain = domain
	g.Defaults.AccessListID = acl.ID
	if ip := s.cachedPublicIP(); ip != "" {
		g.PublicIP = ip
	}
	if err := s.app.Store.PutSettings(ctx, model.SettingsGeneral, g); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	s.app.Changed(ctx, "settings", model.SettingsGeneral, model.SettingsGeneral, core.ActionUpdated)

	// Restrict the admin UI to the LAN list — only when the admin running
	// setup is inside it, so the wizard can never lock them out.
	sec := s.security(ctx)
	res := networkResult{AccessListID: acl.ID, LANCIDR: g.LANCIDR, AdminDomain: domain}
	if addr, err := netip.ParseAddr(core.ClientIP(r)); err == nil && (addr.Unmap().IsLoopback() || ipAllowedByRules(acl.Rules, addr)) {
		sec.AdminAccessListID = acl.ID
		if err := s.app.Store.PutSettings(ctx, model.SettingsSecurity, sec); err != nil {
			httpx.Fail(w, r, err)
			return
		}
		s.invalidate()
		res.AdminRestricted = true
	}
	host, err := s.syncAdminHost(r, g, sec)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if host != nil {
		res.AdminHostID = host.ID
	}
	detail := "LAN " + g.LANCIDR
	if domain != "" {
		detail += " · admin UI " + domain
	}
	if res.AdminRestricted {
		detail += " · admin UI restricted to lan-only"
	}
	s.app.Audit(ctx, core.AuditEntry{Action: "setup.network", Target: g.InstanceName, Detail: detail})
	httpx.WriteJSON(w, http.StatusOK, res)
}

const lanListName = "lan-only"

// upsertLANList creates or updates the "lan-only" access list: allow the LAN
// and this machine, deny everyone else.
func (s *Service) upsertLANList(r *http.Request, cidr string) (*model.AccessList, error) {
	ctx := r.Context()
	repo := s.app.Store.AccessLists()
	lists, err := repo.List(ctx)
	if err != nil {
		return nil, err
	}
	var prev *model.AccessList
	for i := range lists {
		if lists[i].Name == lanListName {
			p := lists[i]
			prev = &p
			break
		}
	}
	next := model.AccessList{Name: lanListName, Description: "Only devices on your local network", BasicAuth: model.BasicAuth{Users: []model.BasicAuthUser{}}}
	if prev != nil {
		next = *prev
		next.BasicAuth.Users = append([]model.BasicAuthUser(nil), prev.BasicAuth.Users...)
	}
	next.Rules = []model.IPRule{
		{Action: "allow", CIDR: cidr, Note: "LAN"},
		{Action: "allow", CIDR: "127.0.0.1", Note: "this machine"},
		{Action: "deny", CIDR: "all", Note: "everyone else"},
	}
	var prevAny any
	if prev != nil {
		prevAny = prev
	}
	if sk, ok := any(&next).(model.SecretKeeper); ok {
		if err := sk.KeepSecrets(prevAny); err != nil {
			return nil, err
		}
	}
	for i := range next.Rules {
		if next.Rules[i].ID == "" {
			next.Rules[i].ID = "r" + strings.ToLower(store.NewID()[:8])
		}
	}
	if h := httpx.AccessListHooks.BeforeSave; h != nil {
		if err := h(r, prev, &next); err != nil {
			return nil, err
		}
	}
	if v, ok := any(&next).(model.Validator); ok {
		if err := v.Validate(); err != nil {
			return nil, err
		}
	}
	if prev == nil {
		if err := repo.Create(ctx, &next); err != nil {
			return nil, err
		}
		s.app.Audit(ctx, core.AuditEntry{Action: "access_list.create", Target: next.Name, Detail: "allow " + cidr + " · first-run setup", Result: "saved"})
		s.app.Changed(ctx, model.KindAccessList, next.ID, next.Name, core.ActionCreated)
	} else {
		if err := repo.Update(ctx, &next); err != nil {
			return nil, err
		}
		s.app.Audit(ctx, core.AuditEntry{Action: "access_list.update", Target: next.Name, Detail: "allow " + cidr + " · first-run setup", Result: "saved"})
		s.app.Changed(ctx, model.KindAccessList, next.ID, next.Name, core.ActionUpdated)
	}
	if h := httpx.AccessListHooks.AfterSave; h != nil {
		h(r, prev, &next)
	}
	s.invalidate()
	return &next, nil
}

// ---------------------------------------------------------------- step 3: finish

type finishResponse struct {
	Applied        bool   `json:"applied"`
	Version        int64  `json:"version,omitempty"`
	Error          string `json:"error,omitempty"`
	Containers     *int   `json:"containers"`
	HTTPContainers *int   `json:"httpContainers"`
}

func (s *Service) handleSetupFinish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	g := s.general(ctx)
	g.SetupDone = true
	if err := s.app.Store.PutSettings(ctx, model.SettingsGeneral, g); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	s.app.Audit(ctx, core.AuditEntry{Action: "setup.finish", Target: g.InstanceName, Detail: "first-run setup completed"})
	out := finishResponse{}
	if s.app.Engine == nil {
		out.Error = "The engine service isn't available"
	} else {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		v, err := s.app.Engine.Apply(actx, core.ApplyOptions{Summary: "Initial configuration"})
		cancel()
		switch {
		case err != nil:
			out.Error = err.Error()
		case v == nil:
			out.Error = "Apply returned no version"
		case v.Status == "failed" || v.Status == "rolled_back" || v.Error != "":
			out.Version = v.ID
			out.Error = v.Error
			if out.Error == "" {
				out.Error = "Apply " + v.Status
			}
		default:
			out.Applied = true
			out.Version = v.ID
		}
	}
	_, out.Containers, out.HTTPContainers = s.checkDocker(ctx)
	httpx.WriteJSON(w, http.StatusOK, out)
}
