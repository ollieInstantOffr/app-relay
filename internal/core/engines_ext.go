package core

// Engine image updates (slice: engine). Implemented by internal/engines.

import (
	"context"
	"time"
)

// EngineRelease is one published image version.
type EngineRelease struct {
	Version string     `json:"version"` // 1.30.4
	Tag     string     `json:"tag"`     // 1.30.4-alpine
	Image   string     `json:"image"`   // nginx:1.30.4-alpine
	Date    *time.Time `json:"date,omitempty"`
}

// EngineDrift means the running container isn't the image Relay installed
// (e.g. `docker compose up` recreated it from docker-compose.yml).
type EngineDrift struct {
	RunningImage   string `json:"runningImage"`
	RunningVersion string `json:"runningVersion"`
	DesiredImage   string `json:"desiredImage"`
	DesiredVersion string `json:"desiredVersion"`
}

type EngineUpdateInfo struct {
	Engine          string                    `json:"engine"`  // nginx | haproxy
	Channel         string                    `json:"channel"` // stable | mainline | lts | latest
	Version         string                    `json:"version"` // reported by the running agent
	Reachable       bool                      `json:"reachable"`
	Running         bool                      `json:"running"`
	Image           string                    `json:"image"` // running container image reference
	ImageVersion    string                    `json:"imageVersion"`
	Container       string                    `json:"container"`
	Official        bool                      `json:"official"` // docker.io/library image
	DesiredImage    string                    `json:"desiredImage"`
	Latest          *EngineRelease            `json:"latest,omitempty"` // newest in the selected channel
	Channels        map[string]*EngineRelease `json:"channels"`         // newest per channel
	UpdateAvailable bool                      `json:"updateAvailable"`
	Drift           *EngineDrift              `json:"drift,omitempty"`
	CanUpgrade      bool                      `json:"canUpgrade"`
	UpgradeBlocker  string                    `json:"upgradeBlocker,omitempty"`
	ChangesURL      string                    `json:"changesUrl"`
	Modules         []string                  `json:"modules"`
	MissingModules  []string                  `json:"missingModules"` // features the renderer skips (geoip2 …)
}

type EngineUpdates struct {
	Nginx       EngineUpdateInfo `json:"nginx"`
	HAProxy     EngineUpdateInfo `json:"haproxy"`
	AutoCheck   bool             `json:"autoCheck"`
	CheckedAt   *time.Time       `json:"checkedAt,omitempty"`
	NextCheckAt *time.Time       `json:"nextCheckAt,omitempty"`
	CheckError  string           `json:"checkError,omitempty"`
	DockerError string           `json:"dockerError,omitempty"`
}

const (
	UpgradeRunning    = "running"
	UpgradeSucceeded  = "succeeded"
	UpgradeRolledBack = "rolled_back"
	UpgradeFailed     = "failed"
)

type UpgradeStep struct {
	ID     string `json:"id"` // preflight | pull | validate | backup | swap | health
	Label  string `json:"label"`
	Status string `json:"status"` // pending | running | done | failed | skipped
	Detail string `json:"detail,omitempty"`
}

type UpgradeJob struct {
	ID         string        `json:"id"`
	Engine     string        `json:"engine"`
	From       string        `json:"from"`
	To         string        `json:"to"`
	FromImage  string        `json:"fromImage"`
	ToImage    string        `json:"toImage"`
	Actor      string        `json:"actor"`
	Status     string        `json:"status"`
	Message    string        `json:"message"`
	Error      string        `json:"error,omitempty"`
	Output     string        `json:"output,omitempty"`
	Progress   int           `json:"progress"`
	Steps      []UpgradeStep `json:"steps"`
	StartedAt  time.Time     `json:"startedAt"`
	FinishedAt *time.Time    `json:"finishedAt,omitempty"`
}

type Engines interface {
	Service
	// Updates returns cached version information (no network).
	Updates(ctx context.Context) (*EngineUpdates, error)
	// CheckUpdates queries Docker Hub now.
	CheckUpdates(ctx context.Context) (*EngineUpdates, error)
	// Upgrade starts a background image upgrade job for engine to version.
	Upgrade(ctx context.Context, engine, version string) (*UpgradeJob, error)
	UpgradeStatus() *UpgradeJob
	// KeepRunningImage accepts the running image as desired (resolves drift).
	KeepRunningImage(ctx context.Context, engine string) error
}
