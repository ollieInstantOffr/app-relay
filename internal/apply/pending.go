package apply

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// entityState is the rendering-relevant projection of one entity.
type entityState struct {
	kind  string
	id    string
	name  string
	sum   string
	value any
}

var kindOrder = map[string]int{
	model.KindHost: 0, model.KindRedirect: 1, model.KindStream: 2, model.KindAccessList: 3,
	model.KindCertificate: 4, model.KindBackend: 5, model.KindFrontend: 6, "settings": 7,
}

var settingsLabels = map[string]string{
	model.SettingsGeneral:     "General settings",
	model.SettingsTLS:         "Default TLS settings",
	model.SettingsDefaultHost: "Default host",
	model.SettingsHAProxy:     "HAProxy engine settings",
	model.SettingsBlocklist:   "Blocked IPs",
}

func jsonSum(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func clearMeta(m *model.Meta) { m.CreatedAt, m.UpdatedAt = time.Time{}, time.Time{} }

func firstOr(ss []string, def string) string {
	if len(ss) > 0 && ss[0] != "" {
		return ss[0]
	}
	return def
}

// projections maps kind/id to the parts of each entity that affect rendering.
func projections(s *model.Snapshot) map[string]entityState {
	out := map[string]entityState{}
	add := func(kind, id, name string, v any) {
		out[kind+"/"+id] = entityState{kind: kind, id: id, name: name, sum: jsonSum(v), value: v}
	}
	for _, h := range s.Hosts {
		clearMeta(&h.Meta)
		add(model.KindHost, h.ID, firstOr(h.Domains, h.ID), h)
	}
	for _, r := range s.Redirects {
		clearMeta(&r.Meta)
		name := firstOr(r.Domains, r.ID)
		if p := strings.TrimSpace(r.FromPath); p != "" && p != "/" {
			name += p
		}
		add(model.KindRedirect, r.ID, name, r)
	}
	for _, st := range s.Streams {
		clearMeta(&st.Meta)
		add(model.KindStream, st.ID, st.Name, st)
	}
	for _, al := range s.AccessLists {
		clearMeta(&al.Meta)
		users := make([]model.BasicAuthUser, len(al.BasicAuth.Users))
		for i, u := range al.BasicAuth.Users {
			users[i] = model.BasicAuthUser{Username: u.Username, PasswordHash: u.PasswordHash}
		}
		al.BasicAuth.Users = users
		al.Description = ""
		add(model.KindAccessList, al.ID, al.Name, al)
	}
	for _, c := range s.Certificates {
		// Only what changes rendering: domains and whether it was issued
		// (renewals, status and autoRenew don't create pending changes).
		add(model.KindCertificate, c.ID, c.Name, struct {
			Domains   []string `json:"domains"`
			Issued    bool     `json:"issued"`
			NotBefore bool     `json:"notBefore"`
		}{c.Domains, c.NotAfter != nil && !c.NotAfter.IsZero(), c.NotBefore != nil && !c.NotBefore.IsZero()})
	}
	for _, b := range s.Backends {
		clearMeta(&b.Meta)
		add(model.KindBackend, b.ID, b.Name, b)
	}
	for _, f := range s.Frontends {
		clearMeta(&f.Meta)
		add(model.KindFrontend, f.ID, f.Name, f)
	}
	add("settings", model.SettingsGeneral, settingsLabels[model.SettingsGeneral], struct {
		HTTPPort  int  `json:"httpPort"`
		HTTPSPort int  `json:"httpsPort"`
		HTTP3     bool `json:"http3"`
	}{s.General.HTTPPort, s.General.HTTPSPort, s.General.HTTP3})
	add("settings", model.SettingsTLS, settingsLabels[model.SettingsTLS], struct {
		CipherProfile string             `json:"cipherProfile"`
		HSTS          model.HSTSSettings `json:"hsts"`
		OCSPStapling  bool               `json:"ocspStapling"`
	}{s.TLS.CipherProfile, s.TLS.HSTS, s.TLS.OCSPStapling})
	add("settings", model.SettingsDefaultHost, settingsLabels[model.SettingsDefaultHost], s.DefaultHost)
	add("settings", model.SettingsHAProxy, settingsLabels[model.SettingsHAProxy], s.HAProxy)
	cidrs := []string{}
	for _, e := range s.Blocklist.Entries {
		cidrs = append(cidrs, e.CIDR)
	}
	sort.Strings(cidrs)
	add("settings", model.SettingsBlocklist, settingsLabels[model.SettingsBlocklist], cidrs)
	return out
}

// defaultSnapshot is the baseline used before anything was applied.
func defaultSnapshot() *model.Snapshot {
	return &model.Snapshot{
		General:     store.DefaultGeneral(),
		TLS:         store.DefaultTLS(),
		DefaultHost: store.DefaultDefaultHost(),
		HAProxy:     store.DefaultHAProxy(),
		Blocklist:   model.BlocklistSettings{Entries: []model.BlockEntry{}},
	}
}

// computePending diffs the desired snapshot against the live one. live nil
// means nothing was applied yet: every entity counts as created and settings
// count when they differ from the defaults.
func computePending(cur, live *model.Snapshot) []core.PendingItem {
	if live == nil {
		live = defaultSnapshot()
	}
	cm, lm := projections(cur), projections(live)
	items := []core.PendingItem{}
	for key, c := range cm {
		l, ok := lm[key]
		switch {
		case !ok:
			items = append(items, core.PendingItem{Kind: c.kind, ID: c.id, Name: c.name, Action: core.ActionCreated})
		case l.sum != c.sum:
			items = append(items, core.PendingItem{Kind: c.kind, ID: c.id, Name: c.name, Action: core.ActionUpdated})
		}
	}
	for key, l := range lm {
		if _, ok := cm[key]; !ok {
			items = append(items, core.PendingItem{Kind: l.kind, ID: l.id, Name: l.name, Action: core.ActionDeleted})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if kindOrder[items[i].Kind] != kindOrder[items[j].Kind] {
			return kindOrder[items[i].Kind] < kindOrder[items[j].Kind]
		}
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].ID < items[j].ID
	})
	return items
}

// describeItem returns a short human description ("backend api: server added").
func describeItem(it core.PendingItem, cur, live *model.Snapshot) string {
	noun := map[string]string{
		model.KindHost: "host", model.KindRedirect: "redirect", model.KindStream: "stream", model.KindAccessList: "access list",
		model.KindCertificate: "certificate", model.KindBackend: "backend", model.KindFrontend: "frontend",
	}[it.Kind]
	if it.Kind == "settings" {
		return it.Name + " changed"
	}
	switch it.Action {
	case core.ActionCreated:
		return "New " + noun + " " + it.Name
	case core.ActionDeleted:
		return noun + " " + it.Name + " removed"
	}
	if live == nil {
		live = defaultSnapshot()
	}
	switch it.Kind {
	case model.KindHost:
		return it.Name + " edited"
	case model.KindBackend:
		var c, l *model.Backend
		for i := range cur.Backends {
			if cur.Backends[i].ID == it.ID {
				c = &cur.Backends[i]
			}
		}
		for i := range live.Backends {
			if live.Backends[i].ID == it.ID {
				l = &live.Backends[i]
			}
		}
		if c != nil && l != nil {
			if d := countDelta(len(c.Servers), len(l.Servers), "server"); d != "" {
				return "backend " + it.Name + ": " + d
			}
		}
		return "backend " + it.Name + " edited"
	case model.KindAccessList:
		var c, l *model.AccessList
		for i := range cur.AccessLists {
			if cur.AccessLists[i].ID == it.ID {
				c = &cur.AccessLists[i]
			}
		}
		for i := range live.AccessLists {
			if live.AccessLists[i].ID == it.ID {
				l = &live.AccessLists[i]
			}
		}
		if c != nil && l != nil {
			if d := countDelta(len(c.Rules), len(l.Rules), "rule"); d != "" {
				return it.Name + ": " + d
			}
			if d := countDelta(len(c.BasicAuth.Users), len(l.BasicAuth.Users), "user"); d != "" {
				return it.Name + ": " + d
			}
		}
		return it.Name + " edited"
	case model.KindCertificate:
		for _, c := range cur.Certificates {
			if c.ID == it.ID && c.NotAfter != nil && !c.NotAfter.IsZero() {
				return "certificate " + it.Name + " issued"
			}
		}
		return "certificate " + it.Name + " updated"
	}
	return noun + " " + it.Name + " edited"
}

func countDelta(cur, live int, noun string) string {
	switch d := cur - live; {
	case d == 1:
		return noun + " added"
	case d > 1:
		return fmt.Sprintf("%d %ss added", d, noun)
	case d == -1:
		return noun + " removed"
	case d < -1:
		return fmt.Sprintf("%d %ss removed", -d, noun)
	}
	return ""
}

// summarize joins item descriptions for the pending bar and version summaries.
func summarize(items []core.PendingItem, cur, live *model.Snapshot) string {
	parts := []string{}
	for i, it := range items {
		if i == 3 && len(items) > 4 {
			parts = append(parts, fmt.Sprintf("+%d more", len(items)-3))
			break
		}
		parts = append(parts, describeItem(it, cur, live))
	}
	return strings.Join(parts, " · ")
}
