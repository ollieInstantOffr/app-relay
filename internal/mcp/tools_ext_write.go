package mcp

// Write tools for host details, settings, operations, engines, Docker,
// certificates, backups and the blocklist. They call the REST API in-process
// (see bridge.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
)

// Settings documents reachable over MCP. security and mcp are deliberately
// excluded so an assistant can't loosen its own safeguards.
var mcpSettingsKeys = []any{
	model.SettingsGeneral, model.SettingsTLS, model.SettingsDefaultHost, model.SettingsHAProxy, model.SettingsDocker,
	model.SettingsNotifications, model.SettingsBackup, model.SettingsEngines, model.SettingsBlocklist,
}

func checkSettingsKey(key string) error {
	for _, k := range mcpSettingsKeys {
		if k == key {
			return nil
		}
	}
	return fmt.Errorf("settings %q are not available over MCP (use one of general, tls, default_host, haproxy, docker, notifications, backup, engines, blocklist)", key)
}

type hostConfigArgs struct {
	Host    string         `json:"host" jsonschema:"Host id or any of its domains"`
	Changes map[string]any `json:"changes" jsonschema:"JSON merge patch of the host object (get_host → config), for example {\"locations\": [...]}, {\"forwardAuth\": {\"enabled\": true, \"provider\": \"authelia\", \"verifyUrl\": \"http://127.0.0.1:9091/api/verify\"}}, {\"rateLimit\": {\"enabled\": true, \"requestsPerSecond\": 10, \"burst\": 20}}, {\"maxBodySize\": \"512m\"}, {\"hsts\": \"on\"}, {\"http3\": true}, {\"noIndex\": true}, {\"upstreamTlsVerify\": false}. Arrays replace, null removes"`
	Reason  string         `json:"reason,omitempty" jsonschema:"Why this change is needed; shown to the person approving it"`
}

type settingsUpdateArgs struct {
	Key     string         `json:"key" jsonschema:"Settings document to change"`
	Changes map[string]any `json:"changes" jsonschema:"JSON merge patch of the document returned by get_settings (secrets left blank keep their stored value)"`
	Reason  string         `json:"reason,omitempty" jsonschema:"Why this change is needed; shown to the person approving it"`
}

type defaultHostArgs struct {
	Action        string `json:"action" jsonschema:"close (drop the connection, recommended on public servers), 404 (neutral not-found page), redirect (to redirectTo) or host (serve the proxy host hostId)"`
	RedirectTo    string `json:"redirectTo,omitempty" jsonschema:"Target URL for action redirect"`
	HostID        string `json:"hostId,omitempty" jsonschema:"Proxy host id for action host"`
	CertificateID string `json:"certificateId,omitempty" jsonschema:"Certificate presented to unknown TLS names (empty: self-signed placeholder)"`
	Reason        string `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

type proxyEngineArgs struct {
	Engine string `json:"engine" jsonschema:"nginx (default, runs custom nginx snippets) or edge (Relay Edge, beta)"`
	Reason string `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

type reasonOnlyArgs struct {
	Reason string `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

type rollbackArgs struct {
	Version int64  `json:"version" jsonschema:"Config version id to restore (see list_versions)"`
	Reason  string `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

type engineActionArgs struct {
	Engine string `json:"engine" jsonschema:"nginx, edge (Relay Edge) or haproxy"`
	Action string `json:"action" jsonschema:"start, stop or reload"`
	Reason string `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

type upgradeEngineArgs struct {
	Engine  string `json:"engine" jsonschema:"nginx or haproxy"`
	Version string `json:"version" jsonschema:"Version to install, e.g. 1.31.5 (see get_updates)"`
	Reason  string `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

type upgradeRelayArgs struct {
	RestartEngines bool   `json:"restartEngines,omitempty" jsonschema:"Also restart the engine containers so they run the new agent (traffic pauses 1–3 s)"`
	Reason         string `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

type dockerItem struct {
	EndpointID  string `json:"endpointId" jsonschema:"Docker host id from list_containers"`
	ContainerID string `json:"containerId" jsonschema:"Container id from list_containers"`
	Domain      string `json:"domain" jsonschema:"Domain for the new host"`
	Port        int    `json:"port" jsonschema:"App port inside the container, e.g. 3000"`
	Scheme      string `json:"scheme,omitempty" jsonschema:"http (default) or https"`
}

type dockerHostsArgs struct {
	Items         []dockerItem `json:"items" jsonschema:"Containers to create hosts for"`
	CertificateID string       `json:"certificateId,omitempty" jsonschema:"Certificate for the new hosts (empty: auto)"`
	AccessListID  string       `json:"accessListId,omitempty" jsonschema:"Access list for the new hosts"`
	KeepInSync    bool         `json:"keepInSync,omitempty" jsonschema:"Update the upstream when the container is recreated"`
	Reason        string       `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

type certRefArgs struct {
	Certificate string `json:"certificate" jsonschema:"Certificate id, name or one of its domains"`
	Reason      string `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

type blockArgs struct {
	CIDR   string `json:"cidr" jsonschema:"IP address or CIDR, e.g. 203.0.113.7 or 203.0.113.0/24"`
	Note   string `json:"note,omitempty" jsonschema:"Why it is blocked (block_ip only)"`
	Reason string `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

type exposeArgs struct {
	Backend     string `json:"backend" jsonschema:"Backend name or id (HTTP mode)"`
	Domain      string `json:"domain" jsonschema:"Public domain, e.g. api.example.com"`
	Certificate struct {
		Mode          string `json:"mode" jsonschema:"request (Let's Encrypt), existing or none"`
		CertificateID string `json:"certificateId,omitempty" jsonschema:"For mode existing"`
		Challenge     string `json:"challenge,omitempty" jsonschema:"http-01 or dns-01 for mode request"`
		DNSProviderID string `json:"dnsProviderId,omitempty" jsonschema:"DNS provider for dns-01"`
	} `json:"certificate" jsonschema:"How the domain gets HTTPS"`
	ForceHTTPS bool `json:"forceHttps,omitempty" jsonschema:"Redirect HTTP to HTTPS"`
	Websockets bool `json:"websockets,omitempty" jsonschema:"Allow websocket upgrades"`
	Access     struct {
		Mode         string `json:"mode" jsonschema:"public or list"`
		AccessListID string `json:"accessListId,omitempty" jsonschema:"For mode list"`
	} `json:"access" jsonschema:"Who can reach it"`
	ForwardAuth   map[string]any `json:"forwardAuth,omitempty" jsonschema:"Optional single sign-on: {enabled, provider, verifyUrl, signInUrl, passRemoteUser, passRemoteGroups, skipWellKnown}"`
	RateLimit     map[string]any `json:"rateLimit,omitempty" jsonschema:"Optional: {enabled, requestsPerSecond, burst, exemptAccessListId}"`
	BlockExploits bool           `json:"blockExploits,omitempty" jsonschema:"Block common exploit paths"`
	NoIndex       bool           `json:"noIndex,omitempty" jsonschema:"Hide from search engines"`
	Reason        string         `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

func (s *Service) registerExtWriteTools() {
	addWrite(s, toolInfo{Name: "update_host_config", Title: "Change any host setting",
		Description: "Change any setting of a proxy host (found by id or domain) with a JSON merge patch of its full configuration as returned by get_host: locations (per-path rules), forward auth / single sign-on, rate limiting, headers, timeouts, upload size, HSTS, HTTP/3, cipher profile, geo-blocking, no-index, upstream TLS verification, custom nginx snippet. Validated like the UI; saved to pending changes, not live until apply_changes. May wait for human approval."},
		nil, false, s.planUpdateHostConfig)
	addWrite(s, toolInfo{Name: "update_settings", Title: "Change Relay settings",
		Description: "Change a settings document (general, tls, default_host, haproxy, docker, notifications, backup, engines, blocklist) with a JSON merge patch of what get_settings returns. Security and MCP settings, the admin UI port and the admin domain can't be changed over MCP. Settings that affect the proxy become pending changes. May wait for human approval."},
		map[string][]any{"key": mcpSettingsKeys}, false, s.planUpdateSettings)
	addWrite(s, toolInfo{Name: "set_default_host", Title: "Set the default host",
		Description: "Choose what requests for unknown domains (or the bare IP) get: close the connection, a 404 page, a redirect, or a proxy host. Saved to pending changes; not live until apply_changes. May wait for human approval."},
		map[string][]any{"action": {"close", "404", "redirect", "host"}}, false, s.planSetDefaultHost)
	addWrite(s, toolInfo{Name: "set_proxy_engine", Title: "Switch the proxy engine",
		Description: "Select nginx or Relay Edge (beta) as the reverse proxy engine. Saved as a pending change; apply_changes then validates the new engine, stops the old one, starts the new one on the same ports, health-checks every host and rolls back automatically on failure. All configuration is shared by both engines. May wait for human approval."},
		map[string][]any{"engine": {"nginx", "edge"}}, false, s.planSetProxyEngine)
	addWrite(s, toolInfo{Name: "discard_changes", Title: "Discard pending changes",
		Description: "Throw away every pending change (not only yours) and return the editable configuration to the live version. Check get_pending_changes first. May wait for human approval."},
		nil, true, s.planDiscard)
	addWrite(s, toolInfo{Name: "rollback_version", Title: "Roll back to a config version",
		Description: "Restore an earlier config version (see list_versions). The old version goes through the full apply pipeline (validate, reload, health check) and becomes a new version. May wait for human approval."},
		nil, true, s.planRollback)
	addWrite(s, toolInfo{Name: "engine_action", Title: "Start, stop or reload an engine",
		Description: "Start, stop or reload nginx, Relay Edge or HAProxy through its agent. Stopping the active proxy engine takes every host offline. May wait for human approval."},
		map[string][]any{"engine": {"nginx", "edge", "haproxy"}, "action": {"start", "stop", "reload"}}, true, s.planEngineAction)
	addWrite(s, toolInfo{Name: "check_for_updates", Title: "Check for updates now",
		Description: "Check GitHub for new Relay commits and Docker Hub for new nginx and HAProxy releases, then return the result (same as get_updates, but fresh)."},
		nil, false, s.planCheckUpdates)
	addWrite(s, toolInfo{Name: "upgrade_engine", Title: "Upgrade nginx or HAProxy",
		Description: "Upgrade the nginx or HAProxy container to an official image version: pull, validate the live config on it, swap the container and health-check, rolling back automatically on failure. Runs in the background; poll get_updates. May wait for human approval."},
		map[string][]any{"engine": {"nginx", "haproxy"}}, true, s.planUpgradeEngine)
	addWrite(s, toolInfo{Name: "upgrade_relay", Title: "Upgrade Relay",
		Description: "Upgrade Relay itself (including Relay Edge) from its git checkout: pull main, rebuild, restart and roll back to the previous image if it doesn't become healthy. Relay restarts during the upgrade, so this MCP connection drops briefly. May wait for human approval."},
		nil, true, s.planUpgradeRelay)
	addWrite(s, toolInfo{Name: "create_hosts_from_docker", Title: "Create hosts from Docker containers",
		Description: "Create proxy hosts for containers found by Docker discovery (see list_containers): one host per item with its domain and app port. Stopped local containers get a disabled host that turns on when they start. Saved to pending changes. May wait for human approval."},
		nil, false, s.planDockerHosts)
	addWrite(s, toolInfo{Name: "expose_backend", Title: "Expose a backend online",
		Description: "Put an HTTP load balancer backend on a public domain: creates the reverse proxy host and the local HAProxy frontend, optionally requesting a certificate and adding access control, forward auth or rate limiting. Saved to pending changes; not live until apply_changes. May wait for human approval."},
		nil, false, s.planExpose)
	addWrite(s, toolInfo{Name: "renew_certificate", Title: "Renew a certificate",
		Description: "Renew an ACME certificate now (normally automatic). Runs in the background; check list_certificates. May wait for human approval."},
		nil, false, s.planCertAction("renew"))
	addWrite(s, toolInfo{Name: "delete_certificate", Title: "Delete a certificate",
		Description: "Delete a certificate that no host, redirect or default host uses. May wait for human approval."},
		nil, true, s.planCertAction("delete"))
	addWrite(s, toolInfo{Name: "create_backup", Title: "Create a backup",
		Description: "Create an encrypted backup of all configuration now (needs a backup passphrase in Settings → Backup & restore). May wait for human approval."},
		nil, false, s.planBackup)
	addWrite(s, toolInfo{Name: "block_ip", Title: "Block an IP address",
		Description: "Add an address or CIDR to the global blocklist: every host answers it with 403. Takes effect on the next apply_changes. May wait for human approval."},
		nil, false, s.planBlock(true))
	addWrite(s, toolInfo{Name: "unblock_ip", Title: "Unblock an IP address",
		Description: "Remove an address or CIDR from the global blocklist. Takes effect on the next apply_changes. May wait for human approval."},
		nil, true, s.planBlock(false))
}

// ---------------------------------------------------------------- hosts

func (s *Service) planUpdateHostConfig(ctx context.Context, c *call, in hostConfigArgs) (*plan, error) {
	if len(in.Changes) == 0 {
		return nil, errors.New("changes is required: the host fields to change")
	}
	actx := core.WithActor(ctx, c.actor)
	h, err := s.findHost(actx, in.Host)
	if err != nil {
		return nil, err
	}
	if !c.scope.allowsAll(h.Domains) {
		return nil, fmt.Errorf("host %s is outside this token's scope (%s)", first(h.Domains), c.scope)
	}
	var cur map[string]any
	if err := s.apiCall(actx, http.MethodGet, "/hosts/"+url.PathEscape(h.ID), nil, &cur); err != nil {
		return nil, err
	}
	patch, _ := json.Marshal(in.Changes)
	next, err := mergePatch(cur, patch)
	if err != nil {
		return nil, err
	}
	next["id"] = h.ID
	if domains := mStrs(next["domains"]); !c.scope.allowsAll(domains) {
		return nil, fmt.Errorf("the new domains %s are outside this token's scope (%s)", strings.Join(domains, ", "), c.scope)
	}
	name := first(h.Domains)
	return &plan{
		Summary: fmt.Sprintf("Change settings of host %s", bold(name)),
		Target:  name,
		Preview: jsonDiff(cur, next),
		Detail:  "update_host_config " + h.ID,
		Exec: func(ctx context.Context) (*outcome, error) {
			var saved map[string]any
			if err := s.apiCall(ctx, http.MethodPut, "/hosts/"+url.PathEscape(h.ID), next, &saved); err != nil {
				return nil, err
			}
			return &outcome{Text: fmt.Sprintf("Updated host %s. %s", name, pendingNote), Structured: saved}, nil
		},
	}, nil
}

// ---------------------------------------------------------------- settings

func (s *Service) planSettingsPatch(ctx context.Context, c *call, key string, changes map[string]any, summary string) (*plan, error) {
	if err := requireUnrestricted(c, "changing settings"); err != nil {
		return nil, err
	}
	if err := checkSettingsKey(key); err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return nil, errors.New("changes is required: the settings to change")
	}
	var cur map[string]any
	if err := s.apiCall(core.WithActor(ctx, c.actor), http.MethodGet, "/settings/"+key, nil, &cur); err != nil {
		return nil, err
	}
	patch, _ := json.Marshal(changes)
	next, err := mergePatch(cur, patch)
	if err != nil {
		return nil, err
	}
	if key == model.SettingsGeneral {
		for _, f := range []string{"adminPort", "adminDomain"} {
			if fmt.Sprint(cur[f]) != fmt.Sprint(next[f]) {
				return nil, fmt.Errorf("%s can't be changed over MCP (it could lock people out of Relay); change it in Settings → General", f)
			}
		}
	}
	return &plan{
		Summary: summary,
		Target:  key + " settings",
		Preview: jsonDiff(cur, next),
		Detail:  "settings " + key,
		Exec: func(ctx context.Context) (*outcome, error) {
			var saved map[string]any
			if err := s.apiCall(ctx, http.MethodPut, "/settings/"+key, next, &saved); err != nil {
				return nil, err
			}
			return &outcome{Text: fmt.Sprintf("Saved %s settings. Changes that affect the proxy are pending until apply_changes runs.", key), Structured: saved}, nil
		},
	}, nil
}

func (s *Service) planUpdateSettings(ctx context.Context, c *call, in settingsUpdateArgs) (*plan, error) {
	key := strings.TrimSpace(in.Key)
	return s.planSettingsPatch(ctx, c, key, in.Changes, fmt.Sprintf("Change %s settings", bold(key)))
}

func (s *Service) planSetDefaultHost(ctx context.Context, c *call, in defaultHostArgs) (*plan, error) {
	changes := map[string]any{"action": in.Action, "redirectTo": in.RedirectTo, "hostId": in.HostID, "certificateId": in.CertificateID}
	return s.planSettingsPatch(ctx, c, model.SettingsDefaultHost, changes, fmt.Sprintf("Set the default host to %s", bold(in.Action)))
}

func (s *Service) planSetProxyEngine(ctx context.Context, c *call, in proxyEngineArgs) (*plan, error) {
	if err := requireUnrestricted(c, "switching the proxy engine"); err != nil {
		return nil, err
	}
	engine := strings.ToLower(strings.TrimSpace(in.Engine))
	if engine != "nginx" && engine != "edge" {
		return nil, errors.New("engine must be nginx or edge")
	}
	var gen map[string]any
	if err := s.apiCall(core.WithActor(ctx, c.actor), http.MethodGet, "/settings/"+model.SettingsGeneral, nil, &gen); err != nil {
		return nil, err
	}
	cur := mStr(gen["proxyEngine"])
	if cur == "" {
		cur = "nginx"
	}
	if cur == engine {
		return nil, fmt.Errorf("%s is already the selected proxy engine", engineLabel(engine))
	}
	pl, err := s.planSettingsPatch(ctx, c, model.SettingsGeneral, map[string]any{"proxyEngine": engine},
		fmt.Sprintf("Switch the proxy engine from %s to %s", bold(engineLabel(cur)), bold(engineLabel(engine))))
	if err != nil {
		return nil, err
	}
	exec := pl.Exec
	pl.Exec = func(ctx context.Context) (*outcome, error) {
		out, err := exec(ctx)
		if err == nil {
			out.Text = fmt.Sprintf("Selected %s as the proxy engine. The switch is a pending change: apply_changes validates %s, stops %s, starts %s and rolls back automatically if a host stops working.",
				engineLabel(engine), engineLabel(engine), engineLabel(cur), engineLabel(engine))
		}
		return out, err
	}
	return pl, nil
}

func engineLabel(e string) string {
	switch e {
	case "edge":
		return "Relay Edge"
	case "haproxy":
		return "HAProxy"
	}
	return "nginx"
}

// ---------------------------------------------------------------- versions

func (s *Service) planDiscard(ctx context.Context, c *call, _ reasonOnlyArgs) (*plan, error) {
	if err := requireUnrestricted(c, "discarding pending changes"); err != nil {
		return nil, err
	}
	var pending map[string]any
	if err := s.apiCall(core.WithActor(ctx, c.actor), http.MethodGet, "/pending", nil, &pending); err != nil {
		return nil, err
	}
	count := int(numberOf(pending["count"]))
	if count == 0 {
		return nil, errors.New("there are no pending changes to discard")
	}
	return &plan{
		Summary: fmt.Sprintf("Discard %s", bold(fmt.Sprintf("%d pending change%s", count, plural(count)))),
		Target:  "pending changes",
		Preview: prettyJSON(pending),
		Detail:  fmt.Sprintf("discard %d", count),
		Exec: func(ctx context.Context) (*outcome, error) {
			var out map[string]any
			if err := s.apiCall(ctx, http.MethodPost, "/pending/discard", map[string]any{}, &out); err != nil {
				return nil, err
			}
			return &outcome{Text: fmt.Sprintf("Discarded %d pending change%s; the configuration matches the live version again.", count, plural(count)), Structured: out}, nil
		},
	}, nil
}

func (s *Service) planRollback(ctx context.Context, c *call, in rollbackArgs) (*plan, error) {
	if err := requireUnrestricted(c, "rolling back"); err != nil {
		return nil, err
	}
	if in.Version <= 0 {
		return nil, errors.New("version is required (see list_versions)")
	}
	id := strconv.FormatInt(in.Version, 10)
	var v map[string]any
	if err := s.apiCall(core.WithActor(ctx, c.actor), http.MethodGet, "/versions/"+id, nil, &v); err != nil {
		return nil, err
	}
	return &plan{
		Summary: fmt.Sprintf("Roll back to config version %s", bold("v"+id)),
		Target:  "v" + id,
		Preview: fmt.Sprintf("Restore v%s (%s, %s by %s)", id, mStr(v["status"]), mStr(v["createdAt"]), mStr(v["actor"])),
		Detail:  "rollback v" + id,
		Exec: func(ctx context.Context) (*outcome, error) {
			var out map[string]any
			if err := s.apiCall(ctx, http.MethodPost, "/versions/"+id+"/rollback", map[string]any{}, &out); err != nil {
				return nil, err
			}
			o := &outcome{Text: fmt.Sprintf("Rolled back to v%s: the old configuration is live again as a new version.", id), Structured: out}
			if nv := int64(numberOf(out["id"])); nv > 0 {
				o.Version = &nv
			}
			return o, nil
		},
	}, nil
}

// ---------------------------------------------------------------- engines

func (s *Service) planEngineAction(ctx context.Context, c *call, in engineActionArgs) (*plan, error) {
	if err := requireUnrestricted(c, "controlling engines"); err != nil {
		return nil, err
	}
	engine, action := strings.ToLower(in.Engine), strings.ToLower(in.Action)
	if engine != "nginx" && engine != "edge" && engine != "haproxy" {
		return nil, errors.New("engine must be nginx, edge or haproxy")
	}
	if action != "start" && action != "stop" && action != "reload" {
		return nil, errors.New("action must be start, stop or reload")
	}
	return &plan{
		Summary: fmt.Sprintf("%s %s", strings.ToUpper(action[:1])+action[1:], bold(engineLabel(engine))),
		Target:  engine,
		Preview: fmt.Sprintf("%s the %s engine through its agent", action, engineLabel(engine)),
		Detail:  engine + " " + action,
		Exec: func(ctx context.Context) (*outcome, error) {
			var out map[string]any
			if err := s.apiCall(ctx, http.MethodPost, "/engines/"+engine+"/"+action, map[string]any{}, &out); err != nil {
				return nil, err
			}
			return &outcome{Text: fmt.Sprintf("%s: %s done.", engineLabel(engine), action), Structured: out}, nil
		},
	}, nil
}

func (s *Service) planCheckUpdates(ctx context.Context, c *call, _ reasonOnlyArgs) (*plan, error) {
	return &plan{
		Summary: "Check for Relay, nginx and HAProxy updates",
		Target:  "updates",
		Preview: "Query GitHub and Docker Hub for new versions",
		Detail:  "check updates",
		Exec: func(ctx context.Context) (*outcome, error) {
			var out map[string]any
			if err := s.apiCall(ctx, http.MethodPost, "/engines/updates/check", map[string]any{}, &out); err != nil {
				return nil, err
			}
			return &outcome{Text: updatesSummary(out), Structured: out}, nil
		},
	}, nil
}

func updatesSummary(u map[string]any) string {
	var parts []string
	if r, ok := u["relay"].(map[string]any); ok {
		if r["updateAvailable"] == true {
			parts = append(parts, fmt.Sprintf("Relay %s → %s available (%d new commits)", mStr(r["version"]), firstNonEmptyStr(mStr(r["remoteVersion"]), mStr(r["remoteHead"])), int(numberOf(r["behind"]))))
		} else {
			parts = append(parts, "Relay "+mStr(r["version"])+" is up to date")
		}
	}
	for _, e := range []string{"nginx", "haproxy"} {
		info, ok := u[e].(map[string]any)
		if !ok {
			continue
		}
		latest, _ := info["latest"].(map[string]any)
		switch {
		case info["inactive"] == true:
			parts = append(parts, engineLabel(e)+" is not in use")
		case info["updateAvailable"] == true && latest != nil:
			parts = append(parts, fmt.Sprintf("%s %s → %s available", engineLabel(e), mStr(info["version"]), mStr(latest["version"])))
		default:
			parts = append(parts, engineLabel(e)+" "+mStr(info["version"])+" is up to date")
		}
	}
	if len(parts) == 0 {
		return "No update information."
	}
	return strings.Join(parts, "; ") + "."
}

func (s *Service) planUpgradeEngine(ctx context.Context, c *call, in upgradeEngineArgs) (*plan, error) {
	if err := requireUnrestricted(c, "upgrading engines"); err != nil {
		return nil, err
	}
	engine := strings.ToLower(in.Engine)
	if engine != "nginx" && engine != "haproxy" {
		return nil, errors.New("engine must be nginx or haproxy")
	}
	version := strings.TrimPrefix(strings.TrimSpace(in.Version), "v")
	if version == "" {
		return nil, errors.New("version is required (see get_updates)")
	}
	return &plan{
		Summary: fmt.Sprintf("Upgrade %s to %s", bold(engineLabel(engine)), bold(version)),
		Target:  engine,
		Preview: fmt.Sprintf("Pull %s:%s-alpine, validate the live config on it, swap the container, health-check; roll back automatically on failure", engine, version),
		Detail:  engine + " → " + version,
		Exec: func(ctx context.Context) (*outcome, error) {
			var job map[string]any
			if err := s.apiCall(ctx, http.MethodPost, "/engines/"+engine+"/upgrade", map[string]any{"version": version}, &job); err != nil {
				return nil, err
			}
			return &outcome{Text: fmt.Sprintf("Started upgrading %s to %s in the background; poll get_updates or get_engine_status.", engineLabel(engine), version), Structured: job}, nil
		},
	}, nil
}

func (s *Service) planUpgradeRelay(ctx context.Context, c *call, in upgradeRelayArgs) (*plan, error) {
	if err := requireUnrestricted(c, "upgrading Relay"); err != nil {
		return nil, err
	}
	var u map[string]any
	if err := s.apiCall(core.WithActor(ctx, c.actor), http.MethodGet, "/engines/updates", nil, &u); err != nil {
		return nil, err
	}
	r, _ := u["relay"].(map[string]any)
	if r == nil || r["canUpdate"] != true {
		reason := "run check_for_updates first"
		if r != nil {
			reason = firstNonEmptyStr(mStr(r["blocker"]), mStr(r["checkError"]), reason)
		}
		return nil, fmt.Errorf("Relay can't be upgraded from here right now: %s", reason)
	}
	target := firstNonEmptyStr(mStr(r["remoteVersion"]), mStr(r["remoteHead"]))
	return &plan{
		Summary: fmt.Sprintf("Upgrade Relay from %s to %s", bold(mStr(r["version"])), bold(target)),
		Target:  "relay",
		Preview: fmt.Sprintf("git pull (fast-forward) in %s, docker compose build, docker compose up -d, wait until healthy (roll back otherwise)%s", mStr(r["workingDir"]), map[bool]string{true: ", then restart the engines", false: ""}[in.RestartEngines]),
		Detail:  "relay → " + target,
		Exec: func(ctx context.Context) (*outcome, error) {
			var job map[string]any
			if err := s.apiCall(ctx, http.MethodPost, "/engines/relay/update", map[string]any{"restartEngines": in.RestartEngines}, &job); err != nil {
				return nil, err
			}
			return &outcome{Text: "Started the Relay upgrade. Relay restarts during it, so this connection drops for a moment; reconnect and check get_updates.", Structured: job}, nil
		},
	}, nil
}

// ---------------------------------------------------------------- docker, expose

func (s *Service) planDockerHosts(ctx context.Context, c *call, in dockerHostsArgs) (*plan, error) {
	if len(in.Items) == 0 {
		return nil, errors.New("items is required: the containers to create hosts for")
	}
	domains := make([]string, 0, len(in.Items))
	for _, it := range in.Items {
		domains = append(domains, it.Domain)
	}
	if !c.scope.allowsAll(domains) {
		return nil, fmt.Errorf("domains %s are outside this token's scope (%s)", strings.Join(domains, ", "), c.scope)
	}
	body := map[string]any{"items": in.Items, "certificateId": in.CertificateID, "accessListId": in.AccessListID, "keepInSync": in.KeepInSync}
	return &plan{
		Summary: fmt.Sprintf("Create %s from Docker: %s", bold(fmt.Sprintf("%d host%s", len(in.Items), plural(len(in.Items)))), bold(strings.Join(domains, ", "))),
		Target:  strings.Join(domains, ", "),
		Preview: prettyJSON(body),
		Detail:  fmt.Sprintf("docker hosts %d", len(in.Items)),
		Exec: func(ctx context.Context) (*outcome, error) {
			var out map[string]any
			if err := s.apiCall(ctx, http.MethodPost, "/docker/hosts", body, &out); err != nil {
				return nil, err
			}
			created, _ := out["created"].([]any)
			return &outcome{Text: fmt.Sprintf("Created %d host%s from Docker. %s", len(created), plural(len(created)), pendingNote), Structured: out}, nil
		},
	}, nil
}

func (s *Service) planExpose(ctx context.Context, c *call, in exposeArgs) (*plan, error) {
	actx := core.WithActor(ctx, c.actor)
	backend, err := s.findEntity(actx, entityKinds[3], in.Backend)
	if err != nil {
		return nil, err
	}
	name := mStr(backend["name"])
	if !c.scope.allows(name) || !c.scope.allows(in.Domain) {
		return nil, fmt.Errorf("backend %s or domain %s is outside this token's scope (%s)", name, in.Domain, c.scope)
	}
	body := map[string]any{
		"backendId": mStr(backend["id"]), "domain": in.Domain, "certificate": in.Certificate, "forceHttps": in.ForceHTTPS,
		"websockets": in.Websockets, "access": in.Access, "blockExploits": in.BlockExploits, "noIndex": in.NoIndex, "applyNow": false,
	}
	if in.ForwardAuth != nil {
		body["forwardAuth"] = in.ForwardAuth
	}
	if in.RateLimit != nil {
		body["rateLimit"] = in.RateLimit
	}
	return &plan{
		Summary: fmt.Sprintf("Expose backend %s on %s", bold(name), bold(in.Domain)),
		Target:  name + " → " + in.Domain,
		Preview: prettyJSON(body),
		Detail:  "expose " + name + " " + in.Domain,
		Exec: func(ctx context.Context) (*outcome, error) {
			var out map[string]any
			if err := s.apiCall(ctx, http.MethodPost, "/lb/expose", body, &out); err != nil {
				return nil, err
			}
			return &outcome{Text: fmt.Sprintf("Exposed backend %s on %s. %s", name, in.Domain, pendingNote), Structured: out}, nil
		},
	}, nil
}

// ---------------------------------------------------------------- certificates, backups, blocklist

func (s *Service) planCertAction(action string) func(ctx context.Context, c *call, in certRefArgs) (*plan, error) {
	return func(ctx context.Context, c *call, in certRefArgs) (*plan, error) {
		ref := strings.TrimSpace(in.Certificate)
		if ref == "" {
			return nil, errors.New("certificate is required (id, name or domain)")
		}
		certs, err := s.app.Store.Certificates().List(ctx)
		if err != nil {
			return nil, err
		}
		var match *model.Certificate
		for i := range certs {
			ct := &certs[i]
			if ct.ID == ref || strings.EqualFold(ct.Name, ref) || hasFold(ct.Domains, ref) {
				if match != nil && match.ID != ct.ID {
					return nil, fmt.Errorf("%q matches several certificates; pass the id", ref)
				}
				match = ct
			}
		}
		if match == nil {
			return nil, fmt.Errorf("no certificate matches %q", ref)
		}
		if !c.scope.allowsAll(match.Domains) {
			return nil, fmt.Errorf("certificate %s is outside this token's scope (%s)", match.Name, c.scope)
		}
		id, name := match.ID, match.Name
		verb := map[string]string{"renew": "Renew", "delete": "Delete"}[action]
		return &plan{
			Summary: fmt.Sprintf("%s certificate %s", verb, bold(name)),
			Target:  name,
			Preview: fmt.Sprintf("%s %s (%s)", verb, name, strings.Join(match.Domains, ", ")),
			Detail:  action + " certificate " + id,
			Exec: func(ctx context.Context) (*outcome, error) {
				var out map[string]any
				method, path := http.MethodPost, "/certificates/"+url.PathEscape(id)+"/renew"
				if action == "delete" {
					method, path = http.MethodDelete, "/certificates/"+url.PathEscape(id)
				}
				var body any
				if method == http.MethodPost {
					body = map[string]any{}
				}
				if err := s.apiCall(ctx, method, path, body, &out); err != nil {
					return nil, err
				}
				text := "Renewal of " + name + " started; check list_certificates for the result."
				if action == "delete" {
					text = "Deleted certificate " + name + "."
				}
				return &outcome{Text: text, Structured: out}, nil
			},
		}, nil
	}
}

func (s *Service) planBackup(ctx context.Context, c *call, _ reasonOnlyArgs) (*plan, error) {
	if err := requireUnrestricted(c, "creating backups"); err != nil {
		return nil, err
	}
	return &plan{
		Summary: "Create a " + bold("backup") + " now",
		Target:  "backup",
		Preview: "Encrypted archive of hosts, backends, access lists, certificates, users and settings",
		Detail:  "create backup",
		Exec: func(ctx context.Context) (*outcome, error) {
			var row map[string]any
			if err := s.apiCall(ctx, http.MethodPost, "/backups", map[string]any{}, &row); err != nil {
				return nil, err
			}
			return &outcome{Text: "Created backup " + mStr(row["file"]) + ".", Structured: row}, nil
		},
	}, nil
}

func (s *Service) planBlock(block bool) func(ctx context.Context, c *call, in blockArgs) (*plan, error) {
	return func(ctx context.Context, c *call, in blockArgs) (*plan, error) {
		if err := requireUnrestricted(c, "changing the blocklist"); err != nil {
			return nil, err
		}
		cidr := strings.TrimSpace(in.CIDR)
		if cidr == "" {
			return nil, errors.New("cidr is required, e.g. 203.0.113.7 or 203.0.113.0/24")
		}
		verb := "Block"
		if !block {
			verb = "Unblock"
		}
		return &plan{
			Summary: fmt.Sprintf("%s %s", verb, bold(cidr)),
			Target:  cidr,
			Preview: fmt.Sprintf("%s %s on every host%s", verb, cidr, map[bool]string{true: " · note: " + in.Note, false: ""}[block && in.Note != ""]),
			Detail:  strings.ToLower(verb) + " " + cidr,
			Exec: func(ctx context.Context) (*outcome, error) {
				var out map[string]any
				var err error
				if block {
					err = s.apiCall(ctx, http.MethodPost, "/blocklist", map[string]any{"cidr": cidr, "note": in.Note}, &out)
				} else {
					err = s.apiCall(ctx, http.MethodDelete, "/blocklist/"+url.PathEscape(cidr), nil, &out)
				}
				if err != nil {
					return nil, err
				}
				return &outcome{Text: fmt.Sprintf("%sed %s. Takes effect on the next apply_changes.", verb, cidr), Structured: out}, nil
			},
		}, nil
	}
}

// ---------------------------------------------------------------- helpers

func numberOf(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func firstNonEmptyStr(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
