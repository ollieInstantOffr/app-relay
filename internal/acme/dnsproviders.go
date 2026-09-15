package acme

// DNS-01 providers: lego provider construction from stored credentials and
// real API calls to verify credentials ("Test").

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awsroute53 "github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/smithy-go"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/providers/dns/cloudflare"
	"github.com/go-acme/lego/v4/providers/dns/cloudns"
	"github.com/go-acme/lego/v4/providers/dns/digitalocean"
	"github.com/go-acme/lego/v4/providers/dns/duckdns"
	"github.com/go-acme/lego/v4/providers/dns/dynu"
	legoexec "github.com/go-acme/lego/v4/providers/dns/exec"
	"github.com/go-acme/lego/v4/providers/dns/gandiv5"
	"github.com/go-acme/lego/v4/providers/dns/godaddy"
	"github.com/go-acme/lego/v4/providers/dns/hetzner"
	"github.com/go-acme/lego/v4/providers/dns/httpreq"
	"github.com/go-acme/lego/v4/providers/dns/hurricane"
	"github.com/go-acme/lego/v4/providers/dns/namecheap"
	"github.com/go-acme/lego/v4/providers/dns/netcup"
	"github.com/go-acme/lego/v4/providers/dns/pdns"
	"github.com/go-acme/lego/v4/providers/dns/route53"

	"github.com/instantoffr/relay/internal/model"
)

// legoDNSProvider builds a lego DNS-01 provider from stored credentials
// (never from environment variables) and returns its propagation timeout.
func legoDNSProvider(p *model.DNSProvider) (challenge.Provider, time.Duration, error) {
	c := p.Credentials
	if p.Type == model.DNSProviderOther {
		return otherDNSProvider(c)
	}
	// NewDefaultConfig reads lego variables (TTL, timeouts): never while an
	// "Other" provider has temporarily set some (see dnsother.go).
	envMu.RLock()
	defer envMu.RUnlock()
	switch p.Type {
	case "gandiv5":
		cfg := gandiv5.NewDefaultConfig()
		cfg.PersonalAccessToken = c["personalAccessToken"]
		if cfg.PersonalAccessToken == "" {
			cfg.APIKey = c["apiKey"]
		}
		prov, err := gandiv5.NewDNSProviderConfig(cfg)
		return prov, cfg.PropagationTimeout, err
	case "godaddy":
		cfg := godaddy.NewDefaultConfig()
		cfg.APIKey, cfg.APISecret = c["apiKey"], c["apiSecret"]
		prov, err := godaddy.NewDNSProviderConfig(cfg)
		return prov, cfg.PropagationTimeout, err
	case "namecheap":
		cfg := namecheap.NewDefaultConfig()
		cfg.APIUser, cfg.APIKey, cfg.ClientIP = c["apiUser"], c["apiKey"], c["clientIp"]
		prov, err := namecheap.NewDNSProviderConfig(cfg) // detects the client IP when empty
		return prov, cfg.PropagationTimeout, err
	case "dynu":
		cfg := dynu.NewDefaultConfig()
		cfg.APIKey = c["apiKey"]
		prov, err := dynu.NewDNSProviderConfig(cfg)
		return prov, cfg.PropagationTimeout, err
	case "netcup":
		cfg := netcup.NewDefaultConfig()
		cfg.Customer, cfg.Key, cfg.Password = c["customerNumber"], c["apiKey"], c["apiPassword"]
		prov, err := netcup.NewDNSProviderConfig(cfg)
		return prov, cfg.PropagationTimeout, err
	case "cloudns":
		cfg := cloudns.NewDefaultConfig()
		cfg.AuthID, cfg.SubAuthID, cfg.AuthPassword = c["authId"], c["subAuthId"], c["authPassword"]
		if cfg.SubAuthID != "" {
			cfg.AuthID = ""
		}
		prov, err := cloudns.NewDNSProviderConfig(cfg)
		return prov, cfg.PropagationTimeout, err
	case "hurricane":
		tokens, err := model.ParseDomainTokens(c["tokens"])
		if err != nil {
			return nil, 0, err
		}
		cfg := hurricane.NewDefaultConfig()
		cfg.Credentials = tokens
		prov, err := hurricane.NewDNSProviderConfig(cfg)
		return prov, cfg.PropagationTimeout, err
	case "pdns":
		u, err := url.Parse(strings.TrimRight(c["apiUrl"], "/"))
		if err != nil {
			return nil, 0, err
		}
		cfg := pdns.NewDefaultConfig()
		cfg.Host, cfg.APIKey = u, c["apiKey"]
		if c["serverName"] != "" {
			cfg.ServerName = c["serverName"]
		}
		cfg.APIVersion = 1 // PowerDNS ≥ 4.0; avoids a detection request at construction
		prov, err := pdns.NewDNSProviderConfig(cfg)
		return prov, cfg.PropagationTimeout, err
	case "httpreq":
		u, err := url.Parse(c["endpoint"])
		if err != nil {
			return nil, 0, err
		}
		cfg := httpreq.NewDefaultConfig()
		cfg.Endpoint, cfg.Mode, cfg.Username, cfg.Password = u, c["mode"], c["username"], c["password"]
		prov, err := httpreq.NewDNSProviderConfig(cfg)
		return prov, cfg.PropagationTimeout, err
	case "exec":
		cfg := legoexec.NewDefaultConfig()
		cfg.Program, cfg.Mode = c["program"], c["mode"]
		prov, err := legoexec.NewDNSProviderConfig(cfg)
		return prov, cfg.PropagationTimeout, err
	case "cloudflare":
		cfg := cloudflare.NewDefaultConfig()
		cfg.AuthEmail, cfg.AuthKey = "", ""
		cfg.AuthToken = c["apiToken"]
		cfg.ZoneToken = c["zoneToken"]
		if cfg.ZoneToken == "" {
			cfg.ZoneToken = cfg.AuthToken
		}
		cfg.PropagationTimeout = 2 * time.Minute
		cfg.PollingInterval = 5 * time.Second
		prov, err := cloudflare.NewDNSProviderConfig(cfg)
		return prov, cfg.PropagationTimeout, err
	case "route53":
		cfg := route53.NewDefaultConfig()
		cfg.AccessKeyID = c["accessKeyId"]
		cfg.SecretAccessKey = c["secretAccessKey"]
		cfg.SessionToken = ""
		cfg.Region = c["region"]
		if cfg.Region == "" {
			cfg.Region = "us-east-1"
		}
		cfg.HostedZoneID = c["hostedZoneId"]
		cfg.AssumeRoleArn, cfg.ExternalID, cfg.PrivateZone = "", "", false
		cfg.PropagationTimeout = 3 * time.Minute
		cfg.PollingInterval = 5 * time.Second
		prov, err := route53.NewDNSProviderConfig(cfg)
		return prov, cfg.PropagationTimeout, err
	case "digitalocean":
		cfg := digitalocean.NewDefaultConfig()
		cfg.AuthToken = c["authToken"]
		cfg.PropagationTimeout = 2 * time.Minute
		cfg.PollingInterval = 5 * time.Second
		prov, err := digitalocean.NewDNSProviderConfig(cfg)
		return prov, cfg.PropagationTimeout, err
	case "hetzner":
		cfg := hetzner.NewDefaultConfig()
		cfg.APIToken = c["apiToken"]
		if cfg.APIToken == "" {
			cfg.APIKey = c["apiKey"]
		}
		cfg.PropagationTimeout = 4 * time.Minute
		cfg.PollingInterval = 10 * time.Second
		prov, err := hetzner.NewDNSProviderConfig(cfg)
		return prov, cfg.PropagationTimeout, err
	case "duckdns":
		cfg := duckdns.NewDefaultConfig()
		cfg.Token = c["token"]
		cfg.PropagationTimeout = 3 * time.Minute
		cfg.PollingInterval = 5 * time.Second
		prov, err := duckdns.NewDNSProviderConfig(cfg)
		return prov, cfg.PropagationTimeout, err
	}
	return nil, 0, fmt.Errorf("unsupported DNS provider type %q", p.Type)
}

// DNSTestResult is the outcome of verifying DNS provider credentials.
type DNSTestResult struct {
	Status string   `json:"status"` // ok | failed | unknown
	Zones  []string `json:"zones"`
	Error  string   `json:"error,omitempty"`
}

// API base URLs (variables so tests can point them at a local server).
var (
	cloudflareAPI   = "https://api.cloudflare.com/client/v4"
	digitaloceanAPI = "https://api.digitalocean.com"
	hetznerCloudAPI = "https://api.hetzner.cloud/v1"
	hetznerDNSAPI   = "https://dns.hetzner.com/api/v1"
	duckdnsAPI      = "https://www.duckdns.org"
	dnsHTTPClient   = &http.Client{Timeout: 20 * time.Second}
)

func testDNSProvider(ctx context.Context, p *model.DNSProvider) DNSTestResult {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var zones []string
	var err error
	c := p.Credentials
	switch p.Type {
	case "cloudflare":
		zones, err = testCloudflare(ctx, c["apiToken"], c["zoneToken"])
	case "route53":
		zones, err = testRoute53(ctx, c)
	case "digitalocean":
		zones, err = testDigitalOcean(ctx, c["authToken"])
	case "hetzner":
		zones, err = testHetzner(ctx, c["apiToken"], c["apiKey"])
	case "duckdns":
		if c["domain"] == "" {
			return DNSTestResult{Status: "unknown", Zones: []string{}, Error: "Add your DuckDNS subdomain to test the token"}
		}
		zones, err = testDuckDNS(ctx, c["token"], c["domain"])
	case "gandiv5":
		zones, err = testGandi(ctx, c["personalAccessToken"], c["apiKey"])
	case "godaddy":
		zones, err = testGoDaddy(ctx, c["apiKey"], c["apiSecret"])
	case "namecheap":
		zones, err = testNamecheap(ctx, c["apiUser"], c["apiKey"], c["clientIp"])
	case "dynu":
		zones, err = testDynu(ctx, c["apiKey"])
	case "netcup":
		zones, err = testNetcup(ctx, c["customerNumber"], c["apiKey"], c["apiPassword"])
	case "cloudns":
		zones, err = testClouDNS(ctx, c["authId"], c["subAuthId"], c["authPassword"])
	case "hurricane":
		zones, err = testHurricane(c["tokens"])
	case "pdns":
		zones, err = testPowerDNS(ctx, c["apiUrl"], c["apiKey"], c["serverName"])
	case "httpreq":
		zones, err = testHTTPReq(ctx, c["endpoint"], c["username"], c["password"])
	case "exec":
		zones, err = testExec(c["program"])
	case model.DNSProviderOther:
		zones, err = testOther(c)
	default:
		err = fmt.Errorf("unsupported DNS provider type %q", p.Type)
	}
	if zones == nil {
		zones = []string{}
	}
	sort.Strings(zones)
	var nv notVerified
	if errors.As(err, &nv) {
		return DNSTestResult{Status: "unknown", Zones: zones, Error: nv.Error()}
	}
	if err != nil {
		return DNSTestResult{Status: "failed", Zones: zones, Error: err.Error()}
	}
	return DNSTestResult{Status: "ok", Zones: zones}
}

func doJSON(ctx context.Context, rawURL string, headers map[string]string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := dnsHTTPClient.Do(req)
	if err != nil {
		host := rawURL
		if u, perr := url.Parse(rawURL); perr == nil {
			host = u.Host
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, fmt.Errorf("Timed out reaching %s", host)
		}
		return 0, fmt.Errorf("Could not reach %s: %v", host, unwrapURLError(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil && resp.StatusCode < 300 {
			return resp.StatusCode, fmt.Errorf("unexpected response (HTTP %d)", resp.StatusCode)
		}
	}
	return resp.StatusCode, nil
}

func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

type cfResponse struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

func (r cfResponse) err(status int) error {
	if len(r.Errors) > 0 {
		msg := r.Errors[0].Message
		if status == http.StatusUnauthorized || status == http.StatusForbidden || r.Errors[0].Code == 1000 || r.Errors[0].Code == 9109 {
			return fmt.Errorf("%s · credentials rejected", msg)
		}
		return errors.New(msg)
	}
	return fmt.Errorf("Cloudflare returned HTTP %d", status)
}

func testCloudflare(ctx context.Context, token, zoneToken string) ([]string, error) {
	auth := func(t string) map[string]string { return map[string]string{"Authorization": "Bearer " + t} }
	var verify cfResponse
	vStatus, vErr := doJSON(ctx, cloudflareAPI+"/user/tokens/verify", auth(token), &verify)
	if vErr != nil {
		return nil, vErr
	}
	listToken := token
	if zoneToken != "" {
		listToken = zoneToken
	}
	var zr cfResponse
	zStatus, err := doJSON(ctx, cloudflareAPI+"/zones?per_page=50", auth(listToken), &zr)
	if err != nil {
		return nil, err
	}
	if !zr.Success {
		if !verify.Success {
			return nil, verify.err(vStatus)
		}
		return nil, zr.err(zStatus)
	}
	var list []struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(zr.Result, &list)
	zones := make([]string, 0, len(list))
	for _, z := range list {
		zones = append(zones, z.Name)
	}
	if len(zones) == 0 {
		return zones, errors.New("Token is valid but can't see any zone · grant Zone · Zone · Read")
	}
	return zones, nil
}

func testRoute53(ctx context.Context, c map[string]string) ([]string, error) {
	region := c["region"]
	if region == "" {
		region = "us-east-1"
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(c["accessKeyId"], c["secretAccessKey"], "")),
	)
	if err != nil {
		return nil, err
	}
	client := awsroute53.NewFromConfig(cfg)
	out, err := client.ListHostedZones(ctx, &awsroute53.ListHostedZonesInput{MaxItems: aws.Int32(100)})
	if err != nil {
		var ae smithy.APIError
		if errors.As(err, &ae) {
			switch ae.ErrorCode() {
			case "InvalidClientTokenId", "SignatureDoesNotMatch", "UnrecognizedClientException", "IncompleteSignature":
				return nil, fmt.Errorf("%s · credentials rejected", ae.ErrorCode())
			case "AccessDenied", "AccessDeniedException":
				return nil, fmt.Errorf("%s · the key needs route53:ListHostedZones", ae.ErrorCode())
			}
			return nil, fmt.Errorf("%s · %s", ae.ErrorCode(), ae.ErrorMessage())
		}
		return nil, fmt.Errorf("Could not reach Route 53: %v", err)
	}
	zones := []string{}
	foundID := c["hostedZoneId"] == ""
	for _, z := range out.HostedZones {
		name := strings.TrimSuffix(aws.ToString(z.Name), ".")
		if z.Config != nil && z.Config.PrivateZone {
			name += " (private)"
		}
		zones = append(zones, name)
		if strings.HasSuffix(aws.ToString(z.Id), "/"+c["hostedZoneId"]) || aws.ToString(z.Id) == c["hostedZoneId"] {
			foundID = true
		}
	}
	if !foundID {
		return zones, fmt.Errorf("Hosted zone %s is not visible to these credentials", c["hostedZoneId"])
	}
	return zones, nil
}

func testDigitalOcean(ctx context.Context, token string) ([]string, error) {
	var resp struct {
		Domains []struct {
			Name string `json:"name"`
		} `json:"domains"`
		ID      string `json:"id"`
		Message string `json:"message"`
	}
	status, err := doJSON(ctx, digitaloceanAPI+"/v2/domains?per_page=200", map[string]string{"Authorization": "Bearer " + token}, &resp)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, fmt.Errorf("%s · credentials rejected", firstNonEmpty(resp.Message, "Unable to authenticate"))
	}
	if status >= 300 {
		return nil, fmt.Errorf("DigitalOcean returned HTTP %d: %s", status, resp.Message)
	}
	zones := []string{}
	for _, d := range resp.Domains {
		zones = append(zones, d.Name)
	}
	return zones, nil
}

func testHetzner(ctx context.Context, apiToken, apiKey string) ([]string, error) {
	var resp struct {
		Zones []struct {
			Name string `json:"name"`
		} `json:"zones"`
		Message string `json:"message"`
		Error   struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	var status int
	var err error
	if apiToken != "" {
		status, err = doJSON(ctx, hetznerCloudAPI+"/zones?per_page=50", map[string]string{"Authorization": "Bearer " + apiToken}, &resp)
	} else {
		status, err = doJSON(ctx, hetznerDNSAPI+"/zones?per_page=100", map[string]string{"Auth-API-Token": apiKey}, &resp)
	}
	if err != nil {
		return nil, err
	}
	msg := firstNonEmpty(resp.Error.Message, resp.Message)
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return nil, fmt.Errorf("%s · credentials rejected", firstNonEmpty(msg, "Unauthorized"))
	}
	if status >= 300 {
		return nil, fmt.Errorf("Hetzner returned HTTP %d: %s", status, msg)
	}
	zones := []string{}
	for _, z := range resp.Zones {
		zones = append(zones, z.Name)
	}
	return zones, nil
}

func testDuckDNS(ctx context.Context, token, domain string) ([]string, error) {
	domain = strings.TrimSuffix(domain, ".duckdns.org")
	q := url.Values{"domains": {domain}, "token": {token}, "txt": {"relay-test"}, "clear": {"true"}, "verbose": {"true"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, duckdnsAPI+"/update?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := dnsHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Could not reach www.duckdns.org: %v", unwrapURLError(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "OK") {
		return nil, errors.New("KO · token or subdomain rejected by DuckDNS")
	}
	return []string{domain + ".duckdns.org"}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
