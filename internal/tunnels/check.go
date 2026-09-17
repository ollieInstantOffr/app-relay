package tunnels

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/httpx"
	"github.com/instantoffr/relay/internal/model"
)

// Setup check step states.
const (
	StepOK      = "ok"
	StepFail    = "fail"
	StepWaiting = "waiting"
)

// CheckStep is one line of the setup checklist.
type CheckStep struct {
	ID     string `json:"id"` // address | port | pair
	Status string `json:"status"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

// SetupCheck is POST /api/gateways/{id}/check.
type SetupCheck struct {
	Steps   []CheckStep    `json:"steps"`
	Paired  bool           `json:"paired"`
	Gateway *model.Gateway `json:"gateway"`
}

// Check walks through what a new gateway needs (the address resolves, the
// tunnel port answers, pairing) and pairs as soon as it can. The wizard calls
// it every few seconds while the user installs the gateway.
func (s *Service) Check(ctx context.Context, id string) (*SetupCheck, error) {
	g, err := s.app.Store.Gateways().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	host, port, err := model.SplitGatewayAddress(g.Address)
	if err != nil {
		return nil, err
	}
	out := &SetupCheck{}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	waiting := func(id, title string) CheckStep { return CheckStep{ID: id, Status: StepWaiting, Title: title} }

	// 1. The address.
	addrStep := CheckStep{ID: "address", Title: "Server address"}
	if ip := net.ParseIP(host); ip != nil {
		addrStep.Status, addrStep.Detail = StepOK, host
	} else {
		lctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		ips, err := net.DefaultResolver.LookupHost(lctx, host)
		cancel()
		if err != nil || len(ips) == 0 {
			addrStep.Status, addrStep.Detail = StepFail, fmt.Sprintf("%s doesn't resolve. Check the name, or use the server's IP address.", host)
		} else {
			addrStep.Status, addrStep.Detail = StepOK, fmt.Sprintf("%s → %s", host, strings.Join(ips, ", "))
		}
	}
	out.Steps = append(out.Steps, addrStep)

	// 2. The tunnel port.
	portTitle := fmt.Sprintf("Gateway answers on port %d", port)
	if addrStep.Status != StepOK {
		out.Steps = append(out.Steps, waiting("port", portTitle), waiting("pair", "Paired with this Relay"))
		out.Gateway = redactedGateway(g)
		return out, nil
	}
	portStep := CheckStep{ID: "port", Title: portTitle}
	if g.PairState == model.GatewayPaired {
		portStep.Status = StepOK
	} else {
		d := net.Dialer{Timeout: 4 * time.Second}
		c, err := d.DialContext(ctx, "tcp", addr)
		switch {
		case err == nil:
			c.Close()
			portStep.Status = StepOK
		case strings.Contains(err.Error(), "refused"):
			portStep.Status, portStep.Detail = StepWaiting, fmt.Sprintf("Nothing listens on port %d yet. The installer may still be running.", port)
		default:
			portStep.Status, portStep.Detail = StepWaiting, fmt.Sprintf("No answer on port %d (%s). Allow %d/tcp and %d/udp in the server's firewall and in your provider's firewall or security group.", port, shortNetErr(err), port, port)
		}
	}
	out.Steps = append(out.Steps, portStep)

	// 3. Pairing.
	pairStep := CheckStep{ID: "pair", Title: "Paired with this Relay"}
	switch {
	case g.PairState == model.GatewayPaired:
		pairStep.Status = StepOK
	case portStep.Status != StepOK:
		pairStep.Status = StepWaiting
	default:
		paired, err := s.Pair(ctx, g.ID)
		var he *httpx.HTTPError
		switch {
		case err == nil:
			g = paired
			pairStep.Status = StepOK
		case errors.As(err, &he) && (he.Code == "pairing_rejected" || he.Code == "token_expired" || he.Code == "no_pairing_token"):
			pairStep.Status, pairStep.Detail = StepFail, he.Message
		case errors.As(err, &he):
			pairStep.Status, pairStep.Detail = StepWaiting, he.Message
		default:
			return nil, err
		}
	}
	out.Steps = append(out.Steps, pairStep)
	out.Paired = g.PairState == model.GatewayPaired
	out.Gateway = redactedGateway(g)
	return out, nil
}

func redactedGateway(g *model.Gateway) *model.Gateway {
	c := *g
	c.Redact()
	return &c
}

// PublishTest is POST /api/gateways/{id}/test.
type PublishTest struct {
	Domain string `json:"domain"`
	// DNS: where the domain resolves and whether that is the gateway.
	DNSAddresses []string `json:"dnsAddresses"`
	DNSOK        bool     `json:"dnsOk"`
	Expected     []string `json:"expected"` // the gateway's addresses
	// Through the gateway: an HTTPS request with the domain's name.
	Reachable  bool   `json:"reachable"`
	StatusCode int    `json:"statusCode,omitempty"`
	TLSValid   bool   `json:"tlsValid"`
	Detail     string `json:"detail,omitempty"`
}

// TestPublished requests https://domain/ through the gateway's public HTTPS
// port (like a visitor would, without relying on DNS) and checks where the
// domain's DNS points.
func (s *Service) TestPublished(ctx context.Context, id, domain string) (*PublishTest, error) {
	g, err := s.app.Store.Gateways().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	domain = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(domain, ".")))
	if !model.ValidDNSName(domain, false) {
		return nil, httpx.Errorf(http.StatusBadRequest, "invalid_domain", "Enter a domain name like app.example.com")
	}
	host, _, err := model.SplitGatewayAddress(g.Address)
	if err != nil {
		return nil, err
	}
	out := &PublishTest{Domain: domain, DNSAddresses: []string{}, Expected: []string{}}

	lctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	expected := map[string]bool{}
	for _, ip := range g.PublicIPs {
		expected[ip] = true
	}
	if net.ParseIP(host) != nil {
		expected[host] = true
	} else if ips, err := net.DefaultResolver.LookupHost(lctx, host); err == nil {
		for _, ip := range ips {
			expected[ip] = true
		}
	}
	for ip := range expected {
		out.Expected = append(out.Expected, ip)
	}
	slices.Sort(out.Expected)
	if ips, err := net.DefaultResolver.LookupHost(lctx, domain); err == nil {
		out.DNSAddresses = ips
		for _, ip := range ips {
			if expected[ip] {
				out.DNSOK = true
			}
		}
	}

	dialAddr := net.JoinHostPort(host, "443")
	var peer []*x509.Certificate
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, dialAddr)
		},
		TLSClientConfig: &tls.Config{
			ServerName:         domain,
			InsecureSkipVerify: true, //nolint:gosec // the chain is verified below and reported, not required
			VerifyConnection: func(cs tls.ConnectionState) error {
				peer = cs.PeerCertificates
				return nil
			},
		},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+domain+"/", nil)
	req.Header.Set("User-Agent", "Relay-TunnelTest/1")
	resp, err := client.Do(req)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "EOF"), strings.Contains(msg, "reset"):
			out.Detail = "The gateway closed the connection: the domain isn't published through this gateway yet (publish it and apply), or the tunnel isn't connected."
		case strings.Contains(msg, "refused"):
			out.Detail = "Nothing answers on port 443 of the gateway server. Check that the gateway runs and 443/tcp is open."
		case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
			out.Detail = "No answer on port 443 of the gateway server. Allow 443/tcp in its firewall."
		default:
			out.Detail = msg
		}
		return out, nil
	}
	resp.Body.Close()
	out.Reachable, out.StatusCode = true, resp.StatusCode
	if len(peer) > 0 {
		inter := x509.NewCertPool()
		for _, c := range peer[1:] {
			inter.AddCert(c)
		}
		_, verr := peer[0].Verify(x509.VerifyOptions{DNSName: domain, Intermediates: inter})
		out.TLSValid = verr == nil
	}
	switch {
	case resp.StatusCode >= 500:
		out.Detail = fmt.Sprintf("Reached Relay through the gateway, but the app answered %d.", resp.StatusCode)
	case !out.TLSValid:
		out.Detail = "Reached Relay through the gateway. The certificate isn't trusted yet: request a Let's Encrypt certificate for the domain (DNS-01 recommended)."
	default:
		out.Detail = fmt.Sprintf("Reached Relay through the gateway (HTTP %d) with a valid certificate.", resp.StatusCode)
	}
	return out, nil
}
