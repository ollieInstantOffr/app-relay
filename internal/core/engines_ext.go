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
	// ComposeProject is the compose project Relay searches for engine containers.
	ComposeProject string `json:"composeProject,omitempty"`
	// Relay is the application's own update state.
	Relay *RelayUpdateInfo `json:"relay,omitempty"`
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

// RelayCommit is one commit on the update branch that isn't installed yet.
type RelayCommit struct {
	SHA     string     `json:"sha"`
	Short   string     `json:"short"`
	Author  string     `json:"author"`
	Date    *time.Time `json:"date,omitempty"`
	Subject string     `json:"subject"`
	URL     string     `json:"url,omitempty"`
}

// RelayUpdateInfo describes Relay's own source checkout and whether a newer
// version is available on the update branch (GET /api/engines/updates → relay).
type RelayUpdateInfo struct {
	Version      string `json:"version"`      // binary version
	Commit       string `json:"commit"`       // commit the running binary was built from ("" = unknown)
	Branch       string `json:"branch"`       // update branch (main)
	Container    string `json:"container"`    // relay container name
	WorkingDir   string `json:"workingDir"`   // compose project folder on the Docker host
	Remote       string `json:"remote"`       // origin URL
	CheckoutHead string `json:"checkoutHead"` // HEAD of the checkout
	CheckoutRef  string `json:"checkoutRef"`  // branch the checkout is on
	RemoteHead   string `json:"remoteHead"`   // origin/<branch>
	// Versions derived from git history (scripts/version.sh); "" when unknown.
	CheckoutVersion string        `json:"checkoutVersion"`
	RemoteVersion   string        `json:"remoteVersion"`
	Behind          int           `json:"behind"` // commits on origin not in the checkout
	Ahead           int           `json:"ahead"`  // local commits not on origin
	Dirty           int           `json:"dirty"`  // modified tracked files
	Commits         []RelayCommit `json:"commits"`
	// RebuildNeeded: the checkout is newer than the running build.
	RebuildNeeded   bool       `json:"rebuildNeeded"`
	UpdateAvailable bool       `json:"updateAvailable"`
	CanUpdate       bool       `json:"canUpdate"`
	Blocker         string     `json:"blocker,omitempty"`
	CheckedAt       *time.Time `json:"checkedAt,omitempty"`
	CheckError      string     `json:"checkError,omitempty"`
}

// RelayUpdateJob is a self-update run: git pull, rebuild, restart.
type RelayUpdateJob struct {
	ID             string        `json:"id"`
	From           string        `json:"from"` // commit before
	To             string        `json:"to"`   // commit after the pull
	FromVersion    string        `json:"fromVersion"`
	ToVersion      string        `json:"toVersion"`
	Actor          string        `json:"actor"`
	RestartEngines bool          `json:"restartEngines"`
	Status         string        `json:"status"` // running | succeeded | failed
	Message        string        `json:"message"`
	Error          string        `json:"error,omitempty"`
	Output         string        `json:"output,omitempty"` // last lines of the helper output
	Progress       int           `json:"progress"`
	Steps          []UpgradeStep `json:"steps"`
	HelperID       string        `json:"helperId,omitempty"`
	StartedAt      time.Time     `json:"startedAt"`
	FinishedAt     *time.Time    `json:"finishedAt,omitempty"`
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
	// UpdateRelay pulls the update branch into Relay's checkout, rebuilds and
	// restarts the stack in a helper container.
	UpdateRelay(ctx context.Context, restartEngines bool) (*RelayUpdateJob, error)
	RelayUpdateStatus() *RelayUpdateJob
}
