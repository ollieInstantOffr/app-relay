// Package notify routes Relay events to ntfy, SMTP and webhook channels
// (slice: ops).
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/instantoffr/relay/internal/core"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

const (
	dedupeWindow = 5 * time.Minute
	kvHeld       = "ops.notify.held"
	logKeep      = 5000
)

// retryDelays are the waits before the 2nd and 3rd delivery attempt.
var retryDelays = []time.Duration{5 * time.Second, 30 * time.Second}

type delivery struct {
	channel model.NotificationChannel
	msg     Message
}

type heldItem struct {
	Channels []string  `json:"channels"`
	Event    string    `json:"event"`
	Level    string    `json:"level"`
	Title    string    `json:"title"`
	Message  string    `json:"message"`
	URL      string    `json:"url,omitempty"`
	At       time.Time `json:"at"`
}

type Service struct {
	app *core.App

	intake     chan core.Notification
	deliveries chan delivery

	mu     sync.Mutex
	recent map[string]time.Time
	held   []heldItem

	// send is replaceable in tests.
	send func(ctx context.Context, ch model.NotificationChannel, msg Message) error
	now  func() time.Time
}

func New(app *core.App) *Service {
	return &Service{
		app:        app,
		intake:     make(chan core.Notification, 512),
		deliveries: make(chan delivery, 512),
		recent:     map[string]time.Time{},
		send:       Send,
		now:        time.Now,
	}
}

func (s *Service) Start(ctx context.Context) error {
	if b, err := s.app.Store.GetKV(ctx, kvHeld); err == nil {
		_ = json.Unmarshal(b, &s.held)
	}
	go s.router(ctx)
	for i := 0; i < 2; i++ {
		go s.worker(ctx)
	}
	go s.ticker(ctx)
	return nil
}

// Notify routes an event to its channels without blocking the caller.
func (s *Service) Notify(ctx context.Context, n core.Notification) {
	if n.Level == "" {
		n.Level = "info"
	}
	select {
	case s.intake <- n:
	default:
		s.app.Log.Warn("notification queue full, dropping", "event", n.Event, "title", n.Title)
	}
}

func (s *Service) settings(ctx context.Context) model.NotificationSettings {
	v, err := store.LoadSettings[model.NotificationSettings](ctx, s.app.Store, model.SettingsNotifications)
	if err != nil {
		return store.DefaultNotifications()
	}
	return v
}

func (s *Service) router(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case n := <-s.intake:
			s.route(ctx, n)
		}
	}
}

// route resolves channels, applies dedupe and quiet hours, and queues deliveries.
func (s *Service) route(ctx context.Context, n core.Notification) {
	set := s.settings(ctx)
	chans := routedChannels(set, n.Event)
	if len(chans) == 0 {
		return
	}
	now := s.now()
	key := n.Event + "\x00" + n.Title
	s.mu.Lock()
	for k, t := range s.recent {
		if now.Sub(t) > dedupeWindow {
			delete(s.recent, k)
		}
	}
	if t, ok := s.recent[key]; ok && now.Sub(t) < dedupeWindow {
		s.mu.Unlock()
		return
	}
	s.recent[key] = now
	s.mu.Unlock()

	if !IsCritical(n.Event) && InQuietHours(set.QuietHours, now.In(location(ctx, s.app.Store))) {
		ids := make([]string, len(chans))
		for i, ch := range chans {
			ids[i] = ch.ID
			s.log(ctx, n.Event, ch.ID, "queued", n.Title, "quiet hours")
		}
		s.mu.Lock()
		s.held = append(s.held, heldItem{Channels: ids, Event: n.Event, Level: n.Level, Title: n.Title, Message: n.Message, URL: n.URL, At: now})
		s.persistHeldLocked(ctx)
		s.mu.Unlock()
		return
	}
	msg := fromNotification(n, now)
	for _, ch := range chans {
		s.enqueue(ch, msg)
	}
}

func routedChannels(set model.NotificationSettings, event string) []model.NotificationChannel {
	ids := set.Routes[event]
	out := []model.NotificationChannel{}
	for _, id := range ids {
		for _, ch := range set.Channels {
			if ch.ID == id && ch.Enabled {
				out = append(out, ch)
			}
		}
	}
	return out
}

func (s *Service) enqueue(ch model.NotificationChannel, msg Message) {
	select {
	case s.deliveries <- delivery{channel: ch, msg: msg}:
	default:
		s.app.Log.Warn("notification delivery queue full", "channel", ch.Name, "title", msg.Title)
	}
}

func (s *Service) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-s.deliveries:
			s.deliver(ctx, d)
		}
	}
}

func (s *Service) deliver(ctx context.Context, d delivery) {
	var err error
	for attempt := 0; attempt <= len(retryDelays); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelays[attempt-1]):
			}
		}
		actx, cancel := context.WithTimeout(ctx, 45*time.Second)
		err = s.send(actx, d.channel, d.msg)
		cancel()
		if err == nil {
			s.log(ctx, d.msg.Event, d.channel.ID, "sent", d.msg.Title, "")
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
	s.app.Log.Warn("notification failed", "channel", d.channel.Name, "event", d.msg.Event, "err", err)
	s.log(ctx, d.msg.Event, d.channel.ID, "failed", d.msg.Title, fmt.Sprintf("after %d attempts: %v", len(retryDelays)+1, err))
}

func (s *Service) log(ctx context.Context, event, channelID, status, title, errText string) {
	if _, err := s.app.Store.InsertNotificationLog(context.WithoutCancel(ctx), store.NotificationLogRow{
		At: s.now(), Event: event, ChannelID: channelID, Status: status, Title: title, Error: errText,
	}); err != nil {
		s.app.Log.Warn("notification log", "err", err)
	}
}

// Test sends "Relay test notification" synchronously over ch.
func (s *Service) Test(ctx context.Context, ch model.NotificationChannel) error {
	if errs := ValidateChannel(ch); len(errs) > 0 {
		return errs.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	instance := "Relay"
	if g, err := store.LoadSettings[model.GeneralSettings](ctx, s.app.Store, model.SettingsGeneral); err == nil && g.InstanceName != "" {
		instance = g.InstanceName
	}
	msg := Message{Event: "test", Level: "info", Title: "Relay test notification",
		Message: fmt.Sprintf("If you can read this, the %s channel on %s works.", ch.Name, instance), At: s.now()}
	err := s.send(ctx, ch, msg)
	if errors.Is(err, context.DeadlineExceeded) {
		err = errors.New("timed out after 30 s")
	}
	id := ch.ID
	if id == "" {
		id = "unsaved"
	}
	if err != nil {
		s.log(ctx, "test", id, "failed", msg.Title, err.Error())
		return err
	}
	s.log(ctx, "test", id, "sent", msg.Title, "")
	return nil
}

// ---------------------------------------------------------------- quiet hours & schedule

func (s *Service) persistHeldLocked(ctx context.Context) {
	b, _ := json.Marshal(s.held)
	if err := s.app.Store.PutKV(context.WithoutCancel(ctx), kvHeld, b); err != nil {
		s.app.Log.Warn("persist held notifications", "err", err)
	}
}

func (s *Service) ticker(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	prune := time.NewTicker(6 * time.Hour)
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.flushHeld(ctx)
			s.maybeWeeklySummary(ctx)
		case <-prune.C:
			_ = s.app.Store.PruneNotificationLog(ctx, logKeep)
		}
	}
}

// flushHeld sends one digest per channel once quiet hours are over.
func (s *Service) flushHeld(ctx context.Context) {
	s.mu.Lock()
	if len(s.held) == 0 {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	set := s.settings(ctx)
	now := s.now()
	if InQuietHours(set.QuietHours, now.In(location(ctx, s.app.Store))) {
		return
	}
	s.mu.Lock()
	held := s.held
	s.held = nil
	s.persistHeldLocked(ctx)
	s.mu.Unlock()

	for _, d := range BuildDigests(held, set.Channels, now) {
		s.enqueue(d.channel, d.msg)
	}
}

// BuildDigests groups held items per enabled channel.
func BuildDigests(held []heldItem, channels []model.NotificationChannel, now time.Time) []delivery {
	byChannel := map[string][]heldItem{}
	order := []string{}
	for _, h := range held {
		for _, id := range h.Channels {
			if _, ok := byChannel[id]; !ok {
				order = append(order, id)
			}
			byChannel[id] = append(byChannel[id], h)
		}
	}
	out := []delivery{}
	for _, id := range order {
		var ch *model.NotificationChannel
		for i := range channels {
			if channels[i].ID == id && channels[i].Enabled {
				ch = &channels[i]
			}
		}
		if ch == nil {
			continue
		}
		items := byChannel[id]
		if len(items) == 1 {
			h := items[0]
			out = append(out, delivery{channel: *ch, msg: Message{Event: h.Event, Level: h.Level, Title: h.Title, Message: h.Message, URL: h.URL, At: now}})
			continue
		}
		level := "info"
		lines := make([]string, 0, len(items))
		for _, h := range items {
			if h.Level == "warn" && level == "info" {
				level = "warn"
			}
			if h.Level == "error" {
				level = "error"
			}
			line := "• " + h.At.Format("15:04") + " " + h.Title
			if h.Message != "" {
				line += " — " + h.Message
			}
			lines = append(lines, line)
		}
		out = append(out, delivery{channel: *ch, msg: Message{
			Event: "digest", Level: level, At: now,
			Title:   fmt.Sprintf("Relay · %d alerts held during quiet hours", len(items)),
			Message: strings.Join(lines, "\n"),
		}})
	}
	return out
}
