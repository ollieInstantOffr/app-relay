// Package events is an in-process pub/sub bus. The API streams bus events to
// the browser over SSE so the UI can invalidate cached queries.
package events

import (
	"sync"
	"time"
)

// Topics. Keep in sync with web/src/lib/events.ts.
const (
	ConfigChanged    = "config.changed"  // Data: {kind, id, action}
	PendingChanged   = "pending.changed" // Data: {count}
	ApplyProgress    = "apply.progress"  // Data: {version, stage, message}
	ApplyFinished    = "apply.finished"  // Data: {version, status, error}
	HealthChanged    = "health.changed"  // Data: {target, status}
	CertChanged      = "cert.changed"    // Data: {id, status}
	LogLine          = "log.access"      // Data: model access log entry
	ErrorLogLine     = "log.error"
	AuditAppended    = "audit.appended"
	ApprovalChanged  = "approval.changed" // Data: {id, status}
	EngineChanged    = "engine.changed"   // Data: {engine: nginx|haproxy, running}
	DockerChanged    = "docker.changed"
	BackupChanged    = "backup.changed"
	ActivityAppended = "activity.appended"
	EngineUpgrade    = "engine.upgrade" // Data: core.UpgradeJob (progress of an nginx/HAProxy image upgrade)
	EngineUpdates    = "engine.updates" // Data: {nginx, haproxy} update availability changed
	RelayUpdate      = "relay.update"   // Data: core.RelayUpdateJob (progress of a Relay self-update)
)

type Event struct {
	Topic string    `json:"topic"`
	At    time.Time `json:"at"`
	Data  any       `json:"data,omitempty"`
}

type Bus struct {
	mu   sync.RWMutex
	subs map[int]chan Event
	next int
}

func New() *Bus { return &Bus{subs: map[int]chan Event{}} }

// Publish never blocks; slow subscribers drop events.
func (b *Bus) Publish(topic string, data any) {
	ev := Event{Topic: topic, At: time.Now(), Data: data}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Subscribe returns a channel of events and a cancel func.
func (b *Bus) Subscribe(buffer int) (<-chan Event, func()) {
	ch := make(chan Event, buffer)
	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = ch
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if _, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(ch)
		}
		b.mu.Unlock()
	}
}
