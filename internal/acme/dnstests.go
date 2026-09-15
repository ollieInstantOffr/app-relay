package acme

// Credential checks ("Test") for the DNS providers added for NPM parity. Each
// uses one cheap read-only API call; providers without one report "unknown"
// with an honest message (notVerified) instead of pretending to be verified.

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/instantoffr/relay/internal/model"
)

// notVerified marks a test that could not verify credentials without side
// effects (status "unknown").
type notVerified struct{ msg string }

func (n notVerified) Error() string { return n.msg }

// API endpoints (variables so tests can point them at a local server).
var (
	gandiAPI       = "https://api.gandi.net/v5/livedns"
	godaddyAPI     = "https://api.godaddy.com"
	namecheapAPI   = "https://api.namecheap.com/xml.response"
	namecheapIPAPI = "https://dynamicdns.park-your-domain.com/getip"
	dynuAPI        = "https://api.dynu.com/v2"
	netcupAPI      = "https://ccp.netcup.net/run/webservice/servers/endpoint.php?JSON"
	cloudnsAPI     = "https://api.cloudns.net"
)

func rejected(status int, msg, fallback string) error {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Errorf("%s · credentials rejected", firstNonEmpty(msg, fallback))
	}
	return fmt.Errorf("%s (HTTP %d)", firstNonEmpty(msg, fallback), status)
}

func testGandi(ctx context.Context, pat, apiKey string) ([]string, error) {
	auth := "Bearer " + pat
	if pat == "" {
		auth = "Apikey " + apiKey
	}
	var raw json.RawMessage
	status, err := doJSON(ctx, gandiAPI+"/domains?per_page=500", map[string]string{"Authorization": auth}, &raw)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		return nil, rejected(status, e.Message, "Gandi refused the request")
	}
	var list []struct {
		FQDN string `json:"fqdn"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, errors.New("unexpected response from Gandi")
	}
	zones := []string{}
	for _, d := range list {
		zones = append(zones, d.FQDN)
	}
	return zones, nil
}

func testGoDaddy(ctx context.Context, key, secret string) ([]string, error) {
	var raw json.RawMessage
	status, err := doJSON(ctx, godaddyAPI+"/v1/domains?statuses=ACTIVE&limit=500", map[string]string{"Authorization": "sso-key " + key + ":" + secret}, &raw)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		if status == http.StatusForbidden && e.Code == "ACCESS_DENIED" {
			return nil, errors.New(firstNonEmpty(e.Message, "Access denied") + " · GoDaddy requires 10+ domains or Discount Domain Club for API access")
		}
		return nil, rejected(status, e.Message, "GoDaddy refused the request")
	}
	var list []struct {
		Domain string `json:"domain"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, errors.New("unexpected response from GoDaddy")
	}
	zones := []string{}
	for _, d := range list {
		zones = append(zones, d.Domain)
	}
	return zones, nil
}

func getText(ctx context.Context, rawURL string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := dnsHTTPClient.Do(req)
	if err != nil {
		host := rawURL
		if u, perr := url.Parse(rawURL); perr == nil {
			host = u.Host
		}
		return 0, nil, fmt.Errorf("Could not reach %s: %v", host, unwrapURLError(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, body, err
}

func testNamecheap(ctx context.Context, user, key, clientIP string) ([]string, error) {
	if clientIP == "" {
		_, body, err := getText(ctx, namecheapIPAPI)
		if err != nil {
			return nil, fmt.Errorf("could not detect this server's public IP: %v", err)
		}
		clientIP = strings.TrimSpace(string(body))
	}
	q := url.Values{"ApiUser": {user}, "ApiKey": {key}, "UserName": {user}, "ClientIp": {clientIP}, "Command": {"namecheap.domains.getList"}, "PageSize": {"100"}}
	_, body, err := getText(ctx, namecheapAPI+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var resp struct {
		Status string `xml:"Status,attr"`
		Errors []struct {
			Number  string `xml:"Number,attr"`
			Message string `xml:",chardata"`
		} `xml:"Errors>Error"`
		Domains []struct {
			Name string `xml:"Name,attr"`
		} `xml:"CommandResponse>DomainGetListResult>Domain"`
	}
	if err := xml.Unmarshal(body, &resp); err != nil {
		return nil, errors.New("unexpected response from Namecheap")
	}
	if !strings.EqualFold(resp.Status, "OK") {
		if len(resp.Errors) > 0 {
			msg := strings.TrimSpace(resp.Errors[0].Message)
			if strings.Contains(strings.ToLower(msg), "ip") {
				msg += " · whitelist " + clientIP + " in Namecheap → Profile → Tools → API access"
			}
			return nil, errors.New(msg)
		}
		return nil, errors.New("Namecheap returned an error")
	}
	zones := []string{}
	for _, d := range resp.Domains {
		zones = append(zones, d.Name)
	}
	return zones, nil
}

func testDynu(ctx context.Context, key string) ([]string, error) {
	var resp struct {
		Domains []struct {
			Name string `json:"name"`
		} `json:"domains"`
		Message string `json:"message"`
	}
	status, err := doJSON(ctx, dynuAPI+"/dns", map[string]string{"API-Key": key}, &resp)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, rejected(status, resp.Message, "Dynu refused the request")
	}
	zones := []string{}
	for _, d := range resp.Domains {
		zones = append(zones, d.Name)
	}
	return zones, nil
}

func postJSON(ctx context.Context, rawURL string, in, out any) (int, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := dnsHTTPClient.Do(req)
	if err != nil {
		host := rawURL
		if u, perr := url.Parse(rawURL); perr == nil {
			host = u.Host
		}
		return 0, fmt.Errorf("Could not reach %s: %v", host, unwrapURLError(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return resp.StatusCode, fmt.Errorf("unexpected response (HTTP %d)", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func testNetcup(ctx context.Context, customer, key, password string) ([]string, error) {
	var login struct {
		Status       string          `json:"status"`
		ShortMessage string          `json:"shortmessage"`
		LongMessage  string          `json:"longmessage"`
		ResponseData json.RawMessage `json:"responsedata"`
	}
	if _, err := postJSON(ctx, netcupAPI, map[string]any{"action": "login", "param": map[string]string{"customernumber": customer, "apikey": key, "apipassword": password}}, &login); err != nil {
		return nil, err
	}
	if login.Status != "success" {
		return nil, fmt.Errorf("%s · credentials rejected", firstNonEmpty(login.LongMessage, login.ShortMessage, "Login failed"))
	}
	var data struct {
		SessionID string `json:"apisessionid"`
	}
	_ = json.Unmarshal(login.ResponseData, &data)
	if data.SessionID != "" {
		var ignored map[string]any
		_, _ = postJSON(ctx, netcupAPI, map[string]any{"action": "logout", "param": map[string]string{"customernumber": customer, "apikey": key, "apisessionid": data.SessionID}}, &ignored)
	}
	return []string{}, nil // netcup has no zone listing
}

func testClouDNS(ctx context.Context, authID, subAuthID, password string) ([]string, error) {
	q := url.Values{"auth-password": {password}}
	if subAuthID != "" {
		q.Set("sub-auth-id", subAuthID)
	} else {
		q.Set("auth-id", authID)
	}
	var login struct {
		Status      string `json:"status"`
		Description string `json:"statusDescription"`
	}
	if _, err := doJSON(ctx, cloudnsAPI+"/dns/login.json?"+q.Encode(), nil, &login); err != nil {
		return nil, err
	}
	if login.Status != "Success" {
		return nil, fmt.Errorf("%s · credentials rejected", firstNonEmpty(login.Description, "Login failed"))
	}
	q.Set("page", "1")
	q.Set("rows-per-page", "100")
	var raw json.RawMessage
	if _, err := doJSON(ctx, cloudnsAPI+"/dns/list-zones.json?"+q.Encode(), nil, &raw); err != nil {
		return nil, err
	}
	var list []struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(raw, &list) // {} when there are no zones
	zones := []string{}
	for _, z := range list {
		zones = append(zones, z.Name)
	}
	return zones, nil
}

func testHurricane(tokens string) ([]string, error) {
	m, err := model.ParseDomainTokens(tokens)
	if err != nil {
		return nil, err
	}
	zones := make([]string, 0, len(m))
	for d := range m {
		zones = append(zones, d)
	}
	sort.Strings(zones)
	return zones, notVerified{"Format looks valid (not verified) · Hurricane Electric has no read-only API"}
}

func testPowerDNS(ctx context.Context, apiURL, key, server string) ([]string, error) {
	server = firstNonEmpty(server, "localhost")
	var raw json.RawMessage
	status, err := doJSON(ctx, strings.TrimRight(apiURL, "/")+"/api/v1/servers/"+url.PathEscape(server)+"/zones", map[string]string{"X-API-Key": key}, &raw)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("PowerDNS server %q not found (HTTP 404) · check the API URL and server ID", server)
	}
	if status >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return nil, rejected(status, e.Error, "PowerDNS refused the request")
	}
	var list []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, errors.New("unexpected response from PowerDNS · is this the API URL (webserver-port)?")
	}
	zones := []string{}
	for _, z := range list {
		zones = append(zones, strings.TrimSuffix(z.Name, "."))
	}
	return zones, nil
}

func testHTTPReq(ctx context.Context, endpoint, user, password string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if user != "" || password != "" {
		req.SetBasicAuth(user, password)
	}
	resp, err := dnsHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Could not reach %s: %v", req.URL.Host, unwrapURLError(err))
	}
	resp.Body.Close()
	return nil, notVerified{fmt.Sprintf("Endpoint answered (HTTP %d) · present/cleanup not called, so credentials are not verified", resp.StatusCode)}
}

func testExec(program string) ([]string, error) {
	st, err := os.Stat(program)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("%s does not exist in the relay container", program)
	case err != nil:
		return nil, err
	case st.IsDir():
		return nil, fmt.Errorf("%s is a directory", program)
	case st.Mode()&0o111 == 0:
		return nil, fmt.Errorf("%s is not executable (chmod +x)", program)
	}
	return nil, notVerified{"Program found and executable · not run, so DNS changes are not verified"}
}

func testOther(creds map[string]string) ([]string, error) {
	if _, _, err := otherDNSProvider(creds); err != nil {
		return nil, err
	}
	return nil, notVerified{"Credentials look valid (not verified) · lego accepted the configuration"}
}
