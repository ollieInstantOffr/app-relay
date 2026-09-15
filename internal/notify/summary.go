package notify

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

const kvWeekly = "ops.notify.weekly_sent"

// weeklyDue reports whether the Monday 09:00 summary for t's week is due.
func weeklyDue(t time.Time) (string, bool) {
	year, week := t.ISOWeek()
	key := fmt.Sprintf("%d-W%02d", year, week)
	return key, t.Weekday() == time.Monday && t.Hour() >= 9
}

func (s *Service) maybeWeeklySummary(ctx context.Context) {
	set := s.settings(ctx)
	if len(routedChannels(set, model.EventWeeklySummary)) == 0 {
		return
	}
	now := s.now().In(location(ctx, s.app.Store))
	key, due := weeklyDue(now)
	if !due {
		return
	}
	if last, err := s.app.Store.GetKV(ctx, kvWeekly); err == nil && string(last) == key {
		return
	}
	if err := s.app.Store.PutKV(ctx, kvWeekly, []byte(key)); err != nil {
		return
	}
	n, err := s.WeeklySummary(ctx, now)
	if err != nil {
		s.app.Log.Warn("weekly summary", "err", err)
		return
	}
	s.Notify(ctx, n)
}

// WeeklySummary builds the summary of the 7 days before now.
func (s *Service) WeeklySummary(ctx context.Context, now time.Time) (core.Notification, error) {
	since := now.AddDate(0, 0, -7)
	stats, err := s.app.Store.WeeklyStats(ctx, since)
	if err != nil {
		return core.Notification{}, err
	}
	hosts, err := s.app.Store.Hosts().List(ctx)
	if err != nil {
		return core.Notification{}, err
	}
	certs, err := s.app.Store.Certificates().List(ctx)
	if err != nil {
		return core.Notification{}, err
	}
	enabled := 0
	for _, h := range hosts {
		if h.Enabled {
			enabled++
		}
	}
	expiring := []string{}
	for _, c := range certs {
		if c.NotAfter != nil && c.NotAfter.Sub(now) < 14*24*time.Hour {
			expiring = append(expiring, c.Name)
		}
	}
	return core.Notification{
		Event: model.EventWeeklySummary, Level: "info",
		Title:   "Relay weekly summary · " + since.Format("Jan 2") + " – " + now.Format("Jan 2"),
		Message: summaryText(stats, len(hosts), enabled, len(certs), expiring),
	}, nil
}

func summaryText(w store.WeeklyStats, hosts, enabled, certs int, expiring []string) string {
	lines := []string{}
	req := fmt.Sprintf("Requests: %s", compactNum(w.Requests))
	if w.Requests > 0 {
		req += fmt.Sprintf(" (%.2f%% 5xx)", float64(w.Errors5xx)*100/float64(w.Requests))
	}
	lines = append(lines, req)
	lines = append(lines, fmt.Sprintf("Hosts: %d (%d enabled)", hosts, enabled))
	c := fmt.Sprintf("Certificates: %d", certs)
	if len(expiring) > 0 {
		c += fmt.Sprintf(" · %d expire within 14 days: %s", len(expiring), strings.Join(expiring, ", "))
	}
	lines = append(lines, c)
	lines = append(lines, fmt.Sprintf("Config: %d applies · %d rollbacks · %d failed", w.Applies, w.Rollbacks, w.Failed))
	return strings.Join(lines, "\n")
}

func compactNum(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}
