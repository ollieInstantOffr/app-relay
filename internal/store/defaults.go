package store

import (
	"context"

	"github.com/instantoffr/relay/internal/model"
)

func DefaultGeneral() model.GeneralSettings {
	return model.GeneralSettings{
		InstanceName: "Relay",
		Timezone:     "UTC",
		HTTPPort:     80,
		HTTPSPort:    443,
		AdminPort:    8181,
		HTTP3:        false,
		ProxyEngine:  "nginx",
		LANCIDR:      "192.168.0.0/16",
		Defaults: model.HostDefaults{
			ForceHTTPS:    true,
			BlockExploits: true,
			Websockets:    true,
			HTTP2:         true,
		},
	}
}

func DefaultSecurity() model.SecuritySettings {
	return model.SecuritySettings{
		Require2FAForAdmins: false,
		SessionTTLHours:     24 * 7,
		LoginMaxAttempts:    3,
		LoginLockoutMinutes: 15,
	}
}

func DefaultDocker() model.DockerSettings {
	return model.DockerSettings{
		Enabled:       true,
		Endpoint:      "unix:///var/run/docker.sock",
		DomainPattern: "{name}.home.lan",
		KeepInSync:    true,
	}
}

func DefaultTLS() model.TLSSettings {
	return model.TLSSettings{
		ACMEProvider:       model.CertLetsEncrypt,
		PreferredChallenge: model.ChallengeHTTP01,
		RenewDaysBefore:    30,
		CipherProfile:      "intermediate",
		HSTS:               model.HSTSSettings{Enabled: false, MaxAgeSeconds: 15768000, IncludeSubdomains: true},
		OCSPStapling:       true,
	}
}

func DefaultDefaultHost() model.DefaultHostSettings {
	return model.DefaultHostSettings{Action: "close"}
}

func DefaultHAProxy() model.HAProxySettings {
	return model.HAProxySettings{
		TimeoutConnect:  "5s",
		TimeoutClient:   "50s",
		TimeoutServer:   "50s",
		MaxConn:         20000,
		CheckInterval:   "2s",
		Rise:            2,
		Fall:            3,
		SeamlessReload:  true,
		StatsEnabled:    true,
		StatsBind:       "127.0.0.1:8404",
		Prometheus:      true,
		ExposePortStart: 10080,
	}
}

// MCPTools lists every MCP tool with its default permission.
var MCPTools = map[string]string{
	"list_hosts":          model.ToolRead,
	"get_host":            model.ToolRead,
	"create_host":         model.ToolConfirm,
	"update_host":         model.ToolConfirm,
	"delete_host":         model.ToolDisabled,
	"query_logs":          model.ToolRead,
	"list_backends":       model.ToolRead,
	"get_backend_status":  model.ToolRead,
	"drain_server":        model.ToolConfirm,
	"list_certificates":   model.ToolRead,
	"request_certificate": model.ToolConfirm,
	"list_access_lists":   model.ToolRead,
	"list_streams":        model.ToolRead,
	"get_pending_changes": model.ToolRead,
	"apply_changes":       model.ToolConfirm,
	"manage_users":        model.ToolDisabled,
}

func DefaultMCP() model.MCPSettings {
	tools := map[string]string{}
	for k, v := range MCPTools {
		tools[k] = v
	}
	return model.MCPSettings{
		Enabled:                false,
		Transports:             []string{"http"},
		Tools:                  tools,
		ApprovalTimeoutMinutes: 10,
	}
}

func DefaultNotifications() model.NotificationSettings {
	return model.NotificationSettings{
		Channels:   []model.NotificationChannel{},
		Routes:     map[string][]string{},
		QuietHours: model.QuietHours{Enabled: false, Start: "23:00", End: "07:00"},
	}
}

func DefaultEngines() model.EnginesSettings {
	return model.EnginesSettings{NginxChannel: "stable", HAProxyChannel: "lts", CheckIntervalHours: 12, AutoCheck: true}
}

func DefaultBackup() model.BackupSettings {
	return model.BackupSettings{Enabled: true, Time: "03:00", Keep: 14, IncludePrivateKeys: true}
}

// SettingsDefaults returns a pointer to a defaulted value for a settings key,
// or nil for unknown keys.
func SettingsDefaults(key string) any {
	switch key {
	case model.SettingsGeneral:
		v := DefaultGeneral()
		return &v
	case model.SettingsSecurity:
		v := DefaultSecurity()
		return &v
	case model.SettingsDocker:
		v := DefaultDocker()
		return &v
	case model.SettingsTLS:
		v := DefaultTLS()
		return &v
	case model.SettingsDefaultHost:
		v := DefaultDefaultHost()
		return &v
	case model.SettingsHAProxy:
		v := DefaultHAProxy()
		return &v
	case model.SettingsMCP:
		v := DefaultMCP()
		return &v
	case model.SettingsNotifications:
		v := DefaultNotifications()
		return &v
	case model.SettingsBackup:
		v := DefaultBackup()
		return &v
	case model.SettingsBlocklist:
		return &model.BlocklistSettings{Entries: []model.BlockEntry{}}
	case model.SettingsEngines:
		v := DefaultEngines()
		return &v
	}
	return nil
}

// LoadSettings returns the stored settings for key merged over defaults.
func LoadSettings[T any](ctx context.Context, s *Store, key string) (T, error) {
	var zero T
	d, ok := SettingsDefaults(key).(*T)
	if !ok {
		return zero, ErrNotFound
	}
	if err := s.GetSettings(ctx, key, d); err != nil {
		return zero, err
	}
	return *d, nil
}
