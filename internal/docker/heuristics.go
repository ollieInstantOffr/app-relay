package docker

import (
	"fmt"
	"net"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/network"

	"github.com/instantoffr/relay/internal/core"
)

// webPorts are well-known HTTP(S) ports in preference order.
var webPorts = []int{80, 443, 8080, 3000, 8000, 5000, 8081, 8096, 8123, 3001, 2283, 9000, 8443, 9090, 9443, 8888, 8008, 4000, 5055, 8384, 8989, 7878, 9696, 1880, 5678, 3100}

// httpsPorts default to an https upstream.
var httpsPorts = map[int]bool{443: true, 8443: true, 9443: true}

// nonHTTPPorts are well-known ports of databases, brokers, DNS, SSH…
var nonHTTPPorts = map[int]bool{
	5432: true, 3306: true, 6379: true, 27017: true, 1883: true, 8883: true, 5672: true, 53: true, 22: true,
	25: true, 465: true, 587: true, 143: true, 993: true, 110: true, 995: true, 11211: true, 9092: true,
	2181: true, 1433: true, 1521: true, 4222: true, 7687: true, 6380: true, 26379: true, 3478: true,
	51820: true, 1194: true, 445: true, 139: true, 389: true, 636: true, 5353: true, 123: true, 69: true,
}

// imagePorts maps image base names to their web UI port.
var imagePorts = map[string]int{
	"grafana": 3000, "prometheus": 9090, "loki": 3100, "alertmanager": 9093, "uptime-kuma": 3001,
	"portainer": 9000, "portainer-ce": 9000, "portainer-ee": 9000, "jellyfin": 8096, "home-assistant": 8123,
	"homeassistant": 8123, "immich-server": 2283, "nextcloud": 80, "vaultwarden": 80, "server": 0,
	"gitea": 3000, "forgejo": 3000, "adguardhome": 3000, "pihole": 80, "syncthing": 8384, "sonarr": 8989,
	"radarr": 7878, "prowlarr": 9696, "lidarr": 8686, "bazarr": 6767, "readarr": 8787, "qbittorrent": 8080,
	"transmission": 9091, "sabnzbd": 8080, "overseerr": 5055, "jellyseerr": 5055, "pms-docker": 32400,
	"plex": 32400, "tautulli": 8181, "node-red": 1880, "n8n": 5678, "code-server": 8443, "paperless-ngx": 8000,
	"mealie": 9000, "influxdb": 8086, "kibana": 5601, "whoami": 80, "nginx": 80, "httpd": 80, "caddy": 80,
	"wordpress": 80, "ghost": 2368, "homepage": 3000, "heimdall": 80, "authelia": 9091, "keycloak": 8080,
	"netdata": 19999, "frigate": 5000, "zigbee2mqtt": 8080, "esphome": 6052, "cadvisor": 8080,
	"filebrowser": 80, "photoprism": 2342, "audiobookshelf": 80, "navidrome": 4533, "calibre-web": 8083,
	"freshrss": 80, "miniflux": 8080, "linkding": 9090, "wikijs": 3000, "bookstack": 80, "outline": 3000,
	"mattermost-team-edition": 8065, "gotify": 80, "ntfy": 80, "minio": 9001, "traefik": 8080,
	"speedtest-tracker": 80, "dozzle": 8080, "it-tools": 80, "stirling-pdf": 8080, "actual-server": 5006,
	"changedetection.io": 5000, "searxng": 8080, "open-webui": 8080, "ollama": 11434,
}

// imageBase returns "grafana" for "docker.io/grafana/grafana:11.2@sha256:…".
func imageBase(image string) string {
	s := strings.ToLower(image)
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[:i]
	}
	return s
}

// Guess is the result of the port heuristic.
type Guess struct {
	HTTP       bool // looks like a web app (drives default selection and ordering only)
	Port       int  // best container port (0 = unknown)
	Scheme     string
	Reason     string // warning ("not HTTP (6379)") or why no port is known
	Candidates []int  // container ports, best first
}

// Hints are container details beyond port mappings that point at the app port.
type Hints struct {
	Env     []string // KEY=value
	Command string   // entrypoint + cmd
	Exposed []int    // image EXPOSE / compose expose (tcp)
}

// portEnvVars are environment variables apps commonly read their listen port from.
var portEnvVars = []string{"PORT", "APP_PORT", "HTTP_PORT", "SERVER_PORT", "LISTEN_PORT", "NUXT_PORT", "VITE_PORT"}

// runtimeHints map words in the image name or command to default dev-server ports.
var runtimeHints = []struct {
	words []string
	ports []int
}{
	{[]string{"flask"}, []int{5000}},
	{[]string{"gunicorn", "uvicorn", "django", "hypercorn", "daphne", "fastapi"}, []int{8000}},
	{[]string{"vite"}, []int{3000, 5173}},
	{[]string{"node", "nodejs", "next", "nuxt", "remix", "express", "npm", "pnpm", "yarn", "bun", "deno", "tsx", "nest", "rails", "puma"}, []int{3000}},
	{[]string{"python", "python3"}, []int{8000, 5000}},
}

var wordSplitRe = regexp.MustCompile(`[^a-z0-9]+`)

func runtimePorts(image, command string) []int {
	words := map[string]bool{}
	for _, w := range wordSplitRe.Split(strings.ToLower(imageBase(image)+" "+command), -1) {
		words[w] = true
	}
	for _, h := range runtimeHints {
		for _, w := range h.words {
			if words[w] {
				return h.ports
			}
		}
	}
	return nil
}

// rankPorts orders a port set: image hint, well-known web ports, other ports, known non-HTTP ports.
func rankPorts(set map[int]bool, hint int) []int {
	webIdx := map[int]int{}
	for i, p := range webPorts {
		webIdx[p] = i
	}
	rank := func(p int) (int, int) {
		switch i, web := webIdx[p]; {
		case p == hint:
			return 0, 0
		case web:
			return 1, i
		case nonHTTPPorts[p]:
			return 3, p
		default:
			return 2, p
		}
	}
	out := make([]int, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, ki := rank(out[i])
		rj, kj := rank(out[j])
		if ri != rj {
			return ri < rj
		}
		return ki < kj
	})
	return out
}

func labelScheme(labels map[string]string, port int) string {
	if s := strings.ToLower(labels["relay.scheme"]); s == "http" || s == "https" {
		return s
	}
	return SchemeForPort(port)
}

// portGuess describes a single chosen container port.
func portGuess(port int, labels map[string]string) Guess {
	g := Guess{HTTP: !nonHTTPPorts[port], Port: port, Scheme: labelScheme(labels, port)}
	if strings.TrimSpace(labels["relay.port"]) != "" {
		g.HTTP = true
	}
	if !g.HTTP {
		g.Reason = fmt.Sprintf("not HTTP (%d)", port)
	}
	return g
}

// Detect picks the app port of a container. Candidate priority: relay.port
// label → port env vars → exposed ports → published ports → image/command hints.
// Known non-HTTP ports only produce a warning; they never hide a container.
func Detect(image string, ports []core.ContainerPort, labels map[string]string, h Hints) Guess {
	var cands []int
	seen := map[int]bool{}
	add := func(p int) {
		if p > 0 && p < 65536 && !seen[p] {
			seen[p] = true
			cands = append(cands, p)
		}
	}
	explicit := 0
	if n, err := strconv.Atoi(strings.TrimSpace(labels["relay.port"])); err == nil && n > 0 && n < 65536 {
		explicit = n
		add(n)
	}
	env := map[string]string{}
	for _, kv := range h.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = strings.TrimSpace(v)
		}
	}
	for _, k := range portEnvVars {
		if n, err := strconv.Atoi(env[k]); err == nil && n > 0 && n < 65536 {
			if explicit == 0 {
				explicit = n
			}
			add(n)
		}
	}
	hint := imagePorts[imageBase(image)]
	exposed, published := map[int]bool{}, map[int]bool{}
	udp := false
	for _, p := range h.Exposed {
		exposed[p] = true
	}
	for _, p := range ports {
		switch {
		case p.Proto != "" && p.Proto != "tcp":
			udp = true
		case p.Private <= 0:
		case p.Public > 0:
			published[p.Private] = true
		default:
			exposed[p.Private] = true
		}
	}
	for _, p := range rankPorts(exposed, hint) {
		add(p)
	}
	for _, p := range rankPorts(published, hint) {
		add(p)
	}
	fromContainer := len(cands) // label/env/exposed/published candidates
	add(hint)
	for _, p := range runtimePorts(image, h.Command) {
		add(p)
	}
	if len(cands) == 0 {
		g := Guess{Scheme: "http", Reason: "no port detected — enter the app's port", Candidates: []int{}}
		if udp {
			g.Reason = "UDP only"
		}
		return g
	}
	pick := cands[0]
	if explicit == 0 && nonHTTPPorts[pick] {
		// Prefer a web-looking port the container actually has; name hints
		// only pick when the container has no ports of its own.
		limit := fromContainer
		if limit == 0 {
			limit = len(cands)
		}
		for _, p := range cands[:limit] {
			if !nonHTTPPorts[p] {
				pick = p
				break
			}
		}
	}
	g := portGuess(pick, labels)
	g.Candidates = append([]int{pick}, slices.DeleteFunc(slices.Clone(cands), func(p int) bool { return p == pick })...)
	return g
}

// Classify is Detect without env/command hints.
func Classify(image string, ports []core.ContainerPort, labels map[string]string) Guess {
	return Detect(image, ports, labels, Hints{})
}

// SchemeForPort returns https for well-known TLS ports.
func SchemeForPort(port int) string {
	if httpsPorts[port] {
		return "https"
	}
	return "http"
}

// defaultNetworks are Docker's built-in networks; user networks are preferred.
var defaultNetworks = map[string]bool{"bridge": true, "host": true, "none": true}

// PickIP chooses the address Relay should use for a container.
func PickIP(networkMode string, networks map[string]*network.EndpointSettings) string {
	if networkMode == "host" {
		return "127.0.0.1"
	}
	names := make([]string, 0, len(networks))
	for n, ep := range networks {
		if ep != nil && ep.IPAddress != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if ep := networks[networkMode]; ep != nil && ep.IPAddress != "" && !defaultNetworks[networkMode] {
		return ep.IPAddress
	}
	for _, n := range names {
		if !defaultNetworks[n] {
			return networks[n].IPAddress
		}
	}
	for _, n := range names {
		return networks[n].IPAddress
	}
	return ""
}

// ---------------------------------------------------------------- labels

// LabelSpec is the parsed relay.* label set of a container.
type LabelSpec struct {
	Domains    []string
	Port       int
	Scheme     string // "" = derive from port
	TLS        string // auto | letsencrypt | off
	Access     string // access list name
	Backend    string // backend name
	Websockets *bool
}

var hostnameRe = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ParseLabels reads relay.* labels. ok is false when the container has no
// relay.host / relay.backend label or sets relay.enable=false.
func ParseLabels(labels map[string]string) (spec LabelSpec, ok bool, err error) {
	get := func(k string) string { return strings.TrimSpace(labels["relay."+k]) }
	if v := get("enable"); v != "" {
		if b, perr := strconv.ParseBool(v); perr == nil && !b {
			return spec, false, nil
		}
	}
	hostLabel, backend := get("host"), get("backend")
	if hostLabel == "" && backend == "" {
		return spec, false, nil
	}
	ok = true
	var problems []string
	for _, d := range strings.Split(hostLabel, ",") {
		d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
		if d == "" {
			continue
		}
		if net.ParseIP(d) != nil || !hostnameRe.MatchString(d) {
			problems = append(problems, fmt.Sprintf("relay.host: invalid domain %q", d))
			continue
		}
		spec.Domains = append(spec.Domains, d)
	}
	if v := get("port"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1 || n > 65535 {
			problems = append(problems, fmt.Sprintf("relay.port: invalid port %q", v))
		} else {
			spec.Port = n
		}
	}
	switch v := strings.ToLower(get("scheme")); v {
	case "", "http", "https":
		spec.Scheme = v
	default:
		problems = append(problems, fmt.Sprintf("relay.scheme: use http or https, not %q", v))
	}
	switch v := strings.ToLower(get("tls")); v {
	case "", "auto":
		spec.TLS = "auto"
	case "letsencrypt", "off":
		spec.TLS = v
	default:
		spec.TLS = "auto"
		problems = append(problems, fmt.Sprintf("relay.tls: use auto, letsencrypt or off, not %q", v))
	}
	spec.Access = get("access")
	spec.Backend = backend
	if v := get("websockets"); v != "" {
		b, perr := strconv.ParseBool(v)
		if perr != nil {
			problems = append(problems, fmt.Sprintf("relay.websockets: use true or false, not %q", v))
		} else {
			spec.Websockets = &b
		}
	}
	if hostLabel != "" && len(spec.Domains) == 0 && backend == "" {
		ok = false
	}
	if len(problems) > 0 {
		err = fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return spec, ok, err
}

// labelHash identifies a relay.* label set (to detect label edits).
func labelHash(labels map[string]string) string {
	keys := []string{}
	for k := range labels {
		if strings.HasPrefix(k, "relay.") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + labels[k] + "\n")
	}
	return b.String()
}

// DomainFromPattern renders "{name}.home.lan" for a container name.
func DomainFromPattern(pattern, name string) string {
	if pattern == "" {
		pattern = "{name}.home.lan"
	}
	clean := strings.Trim(regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(strings.ToLower(name), "-"), "-")
	return strings.ReplaceAll(pattern, "{name}", clean)
}

// serverName makes a HAProxy-safe server name.
func serverName(name string) string {
	s := regexp.MustCompile(`[^A-Za-z0-9_.:-]+`).ReplaceAllString(name, "-")
	if s == "" {
		return "server"
	}
	return s
}
