package docker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"golang.org/x/crypto/ssh"

	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// LocalEndpointID is the id given to the endpoint migrated from the legacy
// single-endpoint settings.
const LocalEndpointID = "local"

const defaultRemoteSocket = "/var/run/docker.sock"

var endpointNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)

func typeFromURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return model.DockerTCP
	}
	switch u.Scheme {
	case "unix", "npipe":
		return model.DockerSocket
	case "ssh":
		return model.DockerSSH
	case "https":
		return model.DockerTLS
	}
	return model.DockerTCP
}

func hostOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// isLocal reports whether the endpoint's daemon runs on the same host as Relay
// (a mounted socket, or a socket proxy on loopback): containers are then
// reached by their IP, like before multi-host support.
func isLocal(ep model.DockerEndpoint) bool {
	if ep.Type == model.DockerSocket {
		return true
	}
	return ep.UpstreamAddress == "" && ep.Type != model.DockerSSH && isLoopbackHost(hostOf(ep.URL))
}

// effectiveUpstream is the address used for published ports of remote
// endpoints ("" for local endpoints, which use container IPs).
func effectiveUpstream(ep model.DockerEndpoint) string {
	if a := strings.TrimSpace(ep.UpstreamAddress); a != "" {
		return a
	}
	if isLocal(ep) {
		return ""
	}
	return hostOf(ep.URL)
}

// NormalizeSettings migrates the legacy single endpoint into Endpoints and
// fills missing ids, names and types. It reports whether anything changed.
func NormalizeSettings(s *model.DockerSettings) bool {
	changed := false
	if len(s.Endpoints) == 0 && strings.TrimSpace(s.Endpoint) != "" {
		t := typeFromURL(s.Endpoint)
		ep := model.DockerEndpoint{
			ID: LocalEndpointID, Name: "local", Type: t, URL: strings.TrimSpace(s.Endpoint),
			Enabled: true, AutoCreate: s.AutoCreate, AutoRemove: s.AutoRemove,
		}
		s.Endpoints = []model.DockerEndpoint{ep}
		changed = true
	}
	if s.Endpoints == nil {
		s.Endpoints = []model.DockerEndpoint{}
	}
	ids := map[string]bool{}
	for i := range s.Endpoints {
		ep := &s.Endpoints[i]
		if ep.ID == "" || ids[ep.ID] {
			ep.ID = store.NewID()
			changed = true
		}
		ids[ep.ID] = true
		if ep.Type == "" {
			ep.Type = typeFromURL(ep.URL)
			changed = true
		}
		if strings.TrimSpace(ep.Name) == "" {
			name := strings.ToLower(hostOf(ep.URL))
			if ep.Type == model.DockerSocket || name == "" {
				name = "local"
			}
			ep.Name = strings.Trim(regexp.MustCompile(`[^a-z0-9._-]+`).ReplaceAllString(name, "-"), "-")
			changed = true
		}
	}
	return changed
}

// legacyEndpoint keeps the deprecated single Endpoint field in step.
func legacyEndpoint(s model.DockerSettings) string {
	for _, ep := range s.Endpoints {
		if ep.Type == model.DockerSocket {
			return ep.URL
		}
	}
	if len(s.Endpoints) > 0 {
		return s.Endpoints[0].URL
	}
	return ""
}

// RedactSettings removes private keys (copying the slice) and flags which
// ones are stored.
func RedactSettings(s *model.DockerSettings) {
	eps := make([]model.DockerEndpoint, len(s.Endpoints))
	copy(eps, s.Endpoints)
	for i := range eps {
		eps[i].TLSKeySet = eps[i].TLSKey != ""
		eps[i].TLSKey = ""
		eps[i].SSHKeySet = eps[i].SSHKey != ""
		eps[i].SSHKey = ""
	}
	s.Endpoints = eps
}

// KeepEndpointSecrets carries stored private keys over when the client did
// not resend them, and drops secrets that don't belong to the endpoint type.
func KeepEndpointSecrets(prev []model.DockerEndpoint, next *model.DockerEndpoint) {
	for _, p := range prev {
		if next.ID == "" || p.ID != next.ID {
			continue
		}
		if next.TLSKey == "" {
			next.TLSKey = p.TLSKey
		}
		if next.SSHKey == "" {
			next.SSHKey = p.SSHKey
		}
	}
	if next.Type != model.DockerTLS {
		next.TLSCA, next.TLSCert, next.TLSKey = "", "", ""
	}
	if next.Type != model.DockerSSH {
		next.SSHKey, next.SSHKnownHost = "", ""
	}
	next.TLSKeySet, next.SSHKeySet = false, false
}

func parseSSHKey(pemKey string) (ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey([]byte(pemKey))
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, errors.New("the SSH key is protected by a passphrase; use a dedicated key without one")
		}
		return nil, fmt.Errorf("invalid SSH private key: %v", err)
	}
	return signer, nil
}

// ValidateEndpoint checks one endpoint; field keys are relative ("url").
func ValidateEndpoint(ep model.DockerEndpoint) model.Errs {
	errs := model.Errs{}
	if !endpointNameRe.MatchString(ep.Name) {
		errs.Add("name", "use lowercase letters, digits, dots or dashes (max 40)")
	}
	u, err := url.Parse(strings.TrimSpace(ep.URL))
	if err != nil || u.Scheme == "" {
		errs.Add("url", "enter a URL like unix:///var/run/docker.sock, tcp://10.0.0.5:2375 or ssh://user@10.0.0.5")
		u = &url.URL{}
	}
	switch ep.Type {
	case model.DockerSocket:
		if u.Scheme != "unix" || u.Path == "" {
			errs.Add("url", "use unix:///var/run/docker.sock")
		}
	case model.DockerTCP:
		if (u.Scheme != "tcp" && u.Scheme != "http") || u.Hostname() == "" {
			errs.Add("url", "use tcp://host:2375")
		}
	case model.DockerTLS:
		if (u.Scheme != "tcp" && u.Scheme != "https") || u.Hostname() == "" {
			errs.Add("url", "use tcp://host:2376")
		}
		if (ep.TLSCert == "") != (ep.TLSKey == "") {
			errs.Add("tlsCert", "provide both the client certificate and its key")
		}
		if ep.TLSCert != "" && ep.TLSKey != "" {
			if _, err := tls.X509KeyPair([]byte(ep.TLSCert), []byte(ep.TLSKey)); err != nil {
				errs.Add("tlsCert", "certificate and key don't match or aren't PEM: %v", err)
			}
		}
		if ep.TLSCA != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(ep.TLSCA)) {
			errs.Add("tlsCa", "not a PEM certificate")
		}
	case model.DockerSSH:
		if u.Scheme != "ssh" || u.Hostname() == "" {
			errs.Add("url", "use ssh://user@host:22")
		} else if u.User.Username() == "" {
			errs.Add("url", "include the SSH user, e.g. ssh://docker@10.0.0.5")
		}
		if ep.SSHKey == "" {
			errs.Add("sshKey", "paste the private key Relay should log in with")
		} else if _, err := parseSSHKey(ep.SSHKey); err != nil {
			errs.Add("sshKey", "%v", err)
		}
	default:
		errs.Add("type", "choose socket, tcp, tls or ssh")
	}
	if a := strings.TrimSpace(ep.UpstreamAddress); a != "" {
		if strings.ContainsAny(a, "/: ") && net.ParseIP(a) == nil {
			errs.Add("upstreamAddress", "enter an IP address or hostname without scheme or port")
		} else if net.ParseIP(a) == nil && !hostnameRe.MatchString(strings.ToLower(a)) {
			errs.Add("upstreamAddress", "enter an IP address or hostname")
		}
	}
	return errs
}

// endpointKey identifies the connection-relevant configuration.
func endpointKey(ep model.DockerEndpoint) string {
	return strings.Join([]string{ep.Type, ep.URL, ep.TLSCA, ep.TLSCert, ep.TLSKey, ep.SSHKey, ep.SSHKnownHost}, "\x00")
}

// ---------------------------------------------------------------- ssh host keys

// HostKeyMismatchError is returned when a server presents a different key than
// the trusted fingerprint.
type HostKeyMismatchError struct{ Expected, Got string }

func (e *HostKeyMismatchError) Error() string {
	return fmt.Sprintf("SSH host key changed: expected %s, got %s — if the host was reinstalled, reset the trusted key", e.Expected, e.Got)
}

// hostKeyDecision implements trust-on-first-use: with no known fingerprint the
// presented key is trusted (trustNew), otherwise it must match.
func hostKeyDecision(known, presented string) (trustNew bool, err error) {
	known = strings.TrimSpace(known)
	if known == "" {
		return true, nil
	}
	if known == presented {
		return false, nil
	}
	return false, &HostKeyMismatchError{Expected: known, Got: presented}
}

// ---------------------------------------------------------------- dialing

// dialed is a live connection to one Docker daemon.
type dialed struct {
	api         dockerAPI
	version     types.Version
	fingerprint string // SSH host key presented by the server
	closeFn     func()
}

func (d *dialed) Close() {
	if d.api != nil {
		d.api.Close()
	}
	if d.closeFn != nil {
		d.closeFn()
	}
}

type socketMissingError struct{ path string }

func (e *socketMissingError) Error() string { return "Docker socket not found at " + e.path }

func tlsConfig(ep model.DockerEndpoint) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: hostOf(ep.URL)}
	if ep.TLSCA != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(ep.TLSCA)) {
			return nil, errors.New("CA certificate is not valid PEM")
		}
		cfg.RootCAs = pool
	}
	if ep.TLSCert != "" || ep.TLSKey != "" {
		pair, err := tls.X509KeyPair([]byte(ep.TLSCert), []byte(ep.TLSKey))
		if err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}

func tcpHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return raw
	}
	port := u.Port()
	if port == "" {
		port = "2375"
		if u.Scheme == "https" {
			port = "2376"
		}
	}
	return "tcp://" + net.JoinHostPort(u.Hostname(), port)
}

// dialEndpoint connects to an endpoint and reads the daemon version.
func dialEndpoint(ctx context.Context, ep model.DockerEndpoint) (*dialed, error) {
	var (
		d   = &dialed{}
		cli *client.Client
		err error
	)
	switch ep.Type {
	case model.DockerSocket:
		u, perr := url.Parse(ep.URL)
		if perr != nil || u.Path == "" {
			return nil, fmt.Errorf("invalid socket URL %q", ep.URL)
		}
		if _, serr := os.Stat(u.Path); errors.Is(serr, os.ErrNotExist) {
			return nil, &socketMissingError{path: u.Path}
		}
		cli, err = client.NewClientWithOpts(client.WithHost(ep.URL), client.WithAPIVersionNegotiation())
	case model.DockerTCP:
		cli, err = client.NewClientWithOpts(client.WithHost(tcpHost(ep.URL)), client.WithAPIVersionNegotiation())
	case model.DockerTLS:
		cfg, cerr := tlsConfig(ep)
		if cerr != nil {
			return nil, cerr
		}
		tr := &http.Transport{TLSClientConfig: cfg, TLSHandshakeTimeout: 10 * time.Second}
		cli, err = client.NewClientWithOpts(client.WithHTTPClient(&http.Client{Transport: tr}), client.WithHost(tcpHost(ep.URL)), client.WithAPIVersionNegotiation())
	case model.DockerSSH:
		d, err = dialSSH(ctx, ep)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown endpoint type %q", ep.Type)
	}
	if err != nil {
		d.Close()
		return nil, err
	}
	if cli != nil {
		d.api = cli
	}
	vctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	d.version, err = d.api.ServerVersion(vctx)
	if err != nil {
		d.Close()
		return nil, friendlyDockerError(ep, err)
	}
	return d, nil
}

func friendlyDockerError(ep model.DockerEndpoint, err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "permission denied"):
		if ep.Type == model.DockerSSH {
			u, _ := url.Parse(ep.URL)
			return fmt.Errorf("permission denied on the Docker socket — add %s to the docker group on the host", u.User.Username())
		}
		return fmt.Errorf("permission denied on %s — mount the socket and give Relay access to it", strings.TrimPrefix(ep.URL, "unix://"))
	case strings.Contains(msg, "connection refused"):
		return fmt.Errorf("connection refused by %s — is the Docker API (or socket proxy) listening there?", hostOf(ep.URL))
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "i/o timeout"):
		return fmt.Errorf("timed out connecting to %s", hostOf(ep.URL))
	}
	return err
}

func dialSSH(ctx context.Context, ep model.DockerEndpoint) (*dialed, error) {
	u, err := url.Parse(ep.URL)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("invalid SSH URL %q", ep.URL)
	}
	user := u.User.Username()
	port := u.Port()
	if port == "" {
		port = "22"
	}
	sock := u.Path
	if sock == "" || sock == "/" {
		sock = defaultRemoteSocket
	}
	signer, err := parseSSHKey(ep.SSHKey)
	if err != nil {
		return nil, err
	}
	d := &dialed{}
	cfg := &ssh.ClientConfig{
		User:    user,
		Auth:    []ssh.AuthMethod{ssh.PublicKeys(signer)},
		Timeout: 10 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			d.fingerprint = ssh.FingerprintSHA256(key)
			_, err := hostKeyDecision(ep.SSHKnownHost, d.fingerprint)
			return err
		},
	}
	addr := net.JoinHostPort(u.Hostname(), port)
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, friendlyDockerError(ep, err)
	}
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		var mismatch *HostKeyMismatchError
		if errors.As(err, &mismatch) {
			return nil, mismatch
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, fmt.Errorf("SSH authentication failed for %s — add Relay's public key to ~%s/.ssh/authorized_keys", user, user)
		}
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	sc := ssh.NewClient(c, chans, reqs)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if _, _, err := sc.SendRequest("keepalive@openssh.com", true, nil); err != nil {
					sc.Close()
					return
				}
			}
		}
	}()
	d.closeFn = func() {
		select {
		case <-done:
		default:
			close(done)
		}
		sc.Close()
	}
	cli, err := client.NewClientWithOpts(
		client.WithHost("unix://"+sock),
		client.WithDialContext(func(ctx context.Context, _, _ string) (net.Conn, error) { return sc.Dial("unix", sock) }),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		d.Close()
		return nil, err
	}
	d.api = cli
	return d, nil
}
