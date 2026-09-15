package engines

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/instantoffr/relay/internal/core"
)

// Docker Hub tag listing for the official images.
const hubBase = "https://hub.docker.com"

var releaseTagRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)-alpine$`)

type semver [3]int

func parseVersion(v string) (semver, bool) {
	m := regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`).FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return semver{}, false
	}
	var s semver
	for i := 0; i < 3; i++ {
		s[i], _ = strconv.Atoi(m[i+1])
	}
	return s, true
}

func (a semver) less(b semver) bool {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func (a semver) String() string { return fmt.Sprintf("%d.%d.%d", a[0], a[1], a[2]) }

// Channels per engine. nginx: even minor = stable, odd = mainline.
// HAProxy: even minor = LTS branch, "latest" = newest release of any branch.
var engineChannels = map[string][]string{"nginx": {"stable", "mainline"}, "haproxy": {"lts", "latest"}}

func inChannel(engine, channel string, v semver) bool {
	switch engine + "/" + channel {
	case "nginx/stable":
		return v[1]%2 == 0
	case "nginx/mainline":
		return v[1]%2 == 1
	case "haproxy/lts":
		return v[1]%2 == 0
	case "haproxy/latest":
		return true
	}
	return false
}

// newestInChannel returns the highest release of a channel.
func newestInChannel(engine, channel string, releases []core.EngineRelease) *core.EngineRelease {
	var best *core.EngineRelease
	var bestV semver
	for i := range releases {
		v, ok := parseVersion(releases[i].Version)
		if !ok || !inChannel(engine, channel, v) {
			continue
		}
		if best == nil || bestV.less(v) {
			r := releases[i]
			best, bestV = &r, v
		}
	}
	return best
}

// hubClient lists `X.Y.Z-alpine` tags of library/<repo> with ETag caching.
type hubClient struct {
	base string
	http *http.Client

	mu    sync.Mutex
	etags map[string]cachedPage
}

type cachedPage struct {
	etag string
	body []byte
}

type hubPage struct {
	Next    string `json:"next"`
	Results []struct {
		Name          string     `json:"name"`
		LastUpdated   *time.Time `json:"last_updated"`
		TagLastPushed *time.Time `json:"tag_last_pushed"`
	} `json:"results"`
}

func newHubClient() *hubClient {
	return &hubClient{base: hubBase, http: &http.Client{Timeout: 20 * time.Second}, etags: map[string]cachedPage{}}
}

func (h *hubClient) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Relay-UpdateCheck/1")
	h.mu.Lock()
	cached, hasCache := h.etags[u]
	h.mu.Unlock()
	if hasCache && cached.etag != "" {
		req.Header.Set("If-None-Match", cached.etag)
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && hasCache {
		return cached.body, nil
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("Docker Hub rate limit reached, try again later")
		}
		return nil, fmt.Errorf("Docker Hub returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if etag := resp.Header.Get("ETag"); etag != "" {
		h.mu.Lock()
		h.etags[u] = cachedPage{etag: etag, body: body}
		h.mu.Unlock()
	}
	return body, nil
}

// releases returns every X.Y.Z-alpine release of library/<repo>, newest first.
func (h *hubClient) releases(ctx context.Context, repo string) ([]core.EngineRelease, error) {
	u := fmt.Sprintf("%s/v2/namespaces/library/repositories/%s/tags?page_size=100&name=%s", h.base, url.PathEscape(repo), url.QueryEscape("alpine"))
	seen := map[string]bool{}
	out := []core.EngineRelease{}
	cutoff := time.Now().AddDate(-2, 0, 0)
	for page := 0; page < 15 && u != ""; page++ {
		body, err := h.get(ctx, u)
		if err != nil {
			return nil, err
		}
		var p hubPage
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, fmt.Errorf("Docker Hub: unexpected response: %w", err)
		}
		recent := false
		for _, t := range p.Results {
			date := t.TagLastPushed
			if date == nil {
				date = t.LastUpdated
			}
			if date != nil && date.After(cutoff) {
				recent = true
			}
			m := releaseTagRe.FindStringSubmatch(t.Name)
			if m == nil {
				continue
			}
			v := m[1] + "." + m[2] + "." + m[3]
			if seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, core.EngineRelease{Version: v, Tag: t.Name, Image: repo + ":" + t.Name, Date: date})
		}
		// Tags are ordered by last update; stop once pages are all old.
		if !recent {
			break
		}
		u = p.Next
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("Docker Hub listed no %s X.Y.Z-alpine tags", repo)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := parseVersion(out[i].Version)
		b, _ := parseVersion(out[j].Version)
		return b.less(a)
	})
	if len(out) > 300 {
		out = out[:300]
	}
	return out, nil
}

// changesURL links to upstream release notes.
func changesURL(engine, version string) string {
	if engine == "nginx" {
		return "https://nginx.org/en/CHANGES"
	}
	if v, ok := parseVersion(version); ok {
		return fmt.Sprintf("https://www.haproxy.org/download/%d.%d/src/CHANGELOG", v[0], v[1])
	}
	return "https://www.haproxy.org/"
}
