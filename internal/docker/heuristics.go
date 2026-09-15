package docker

import (
	"fmt"
	"net"
	"regexp"
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

// Guess is the result of the HTTP heuristic.
type Guess struct {
	HTTP   bool
	Port   int
	Scheme string
	Reason string
}

// SchemeForPort returns https for well-known TLS ports.
func SchemeForPort(port int) string {
	if httpsPorts[port] {
		return "https"
	}
	return "http"
}

// Classify decides whether a container looks like a web app and which port to
// proxy to.
func Classify(image string, ports []core.ContainerPort, labels map[string]string) Guess {
	tcp := []int{}
	seen := map[int]bool{}
	for _, p := range ports {
		if p.Proto != "" && p.Proto != "tcp" {
			continue
		}
		if p.Private > 0 && !seen[p.Private] {
			seen[p.Private] = true
			tcp = append(tcp, p.Private)
		}
	}
	sort.Ints(tcp)
	scheme := func(port int) string {
		if s := strings.ToLower(labels["relay.scheme"]); s == "http" || s == "https" {
			return s
		}
		return SchemeForPort(port)
	}
	if v := labels["relay.port"]; v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 && n < 65536 {
			return Guess{HTTP: true, Port: n, Scheme: scheme(n)}
		}
	}
	if hint := imagePorts[imageBase(image)]; hint > 0 && (len(tcp) == 0 || seen[hint]) {
		return Guess{HTTP: true, Port: hint, Scheme: scheme(hint)}
	}
	for _, wp := range webPorts {
		if seen[wp] {
			return Guess{HTTP: true, Port: wp, Scheme: scheme(wp)}
		}
	}
	candidates := []int{}
	for _, p := range tcp {
		if !nonHTTPPorts[p] {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) > 0 {
		return Guess{HTTP: true, Port: candidates[0], Scheme: scheme(candidates[0])}
	}
	if len(tcp) > 0 {
		return Guess{HTTP: false, Port: tcp[0], Scheme: "http", Reason: fmt.Sprintf("not HTTP (%d)", tcp[0])}
	}
	if len(ports) > 0 {
		return Guess{HTTP: false, Port: 0, Scheme: "http", Reason: "UDP only"}
	}
	return Guess{HTTP: false, Reason: "no exposed ports"}
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
