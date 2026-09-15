package notify

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

var clockRe = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)$`)

// parseClock converts "23:00" to minutes after midnight.
func parseClock(s string) (int, bool) {
	m := clockRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	return (int(m[1][0]-'0')*10+int(m[1][1]-'0'))*60 + int(m[2][0]-'0')*10 + int(m[2][1]-'0'), true
}

// InQuietHours reports whether t (already in the local zone) falls inside the
// window [start, end). Windows wrap past midnight when end <= start. An empty
// window (start == end) is never quiet.
func InQuietHours(q model.QuietHours, t time.Time) bool {
	if !q.Enabled {
		return false
	}
	start, ok1 := parseClock(q.Start)
	end, ok2 := parseClock(q.End)
	if !ok1 || !ok2 || start == end {
		return false
	}
	cur := t.Hour()*60 + t.Minute()
	if start < end {
		return cur >= start && cur < end
	}
	return cur >= start || cur < end
}

// QuietEnd returns when the quiet window containing t ends (t if not quiet).
func QuietEnd(q model.QuietHours, t time.Time) time.Time {
	if !InQuietHours(q, t) {
		return t
	}
	end, _ := parseClock(q.End)
	e := time.Date(t.Year(), t.Month(), t.Day(), end/60, end%60, 0, 0, t.Location())
	if !e.After(t) {
		e = e.AddDate(0, 0, 1)
	}
	return e
}

// critical events always go through, even during quiet hours.
var critical = map[string]bool{
	model.EventUpstreamDown:    true,
	model.EventCertRenewFailed: true,
	model.EventReloadFailed:    true,
}

func IsCritical(event string) bool { return critical[event] }

// Events lists every routable event in display order.
var Events = []string{
	model.EventUpstreamDown, model.EventCertRenewFailed, model.EventCertExpiring, model.EventReloadFailed,
	model.EventUnknownSignIn, model.EventMCPWriteExecuted, model.EventWeeklySummary, model.EventEngineUpdateAvailable,
}

func location(ctx context.Context, st *store.Store) *time.Location {
	g, err := store.LoadSettings[model.GeneralSettings](ctx, st, model.SettingsGeneral)
	if err == nil && g.Timezone != "" {
		if loc, err := time.LoadLocation(g.Timezone); err == nil {
			return loc
		}
	}
	return time.Local
}
