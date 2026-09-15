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

func DefaultErrorPages() model.ErrorPagesSettings { return model.DefaultErrorPages() }

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

	// Read tools added for full coverage of the UI.
	"list_redirects":     model.ToolRead,
	"get_access_list":    model.ToolRead,
	"list_frontends":     model.ToolRead,
	"get_default_host":   model.ToolRead,
	"list_versions":      model.ToolRead,
	"get_config_diff":    model.ToolRead,
	"query_error_log":    model.ToolRead,
	"query_audit_log":    model.ToolRead,
	"get_health":         model.ToolRead,
	"get_overview":       model.ToolRead,
	"get_engine_status":  model.ToolRead,
	"get_engine_logs":    model.ToolRead,
	"get_updates":        model.ToolRead,
	"list_containers":    model.ToolRead,
	"list_dns_providers": model.ToolRead,
	"get_settings":       model.ToolRead,
	"list_backups":       model.ToolRead,
	"get_activity":       model.ToolRead,
	"get_ports":          model.ToolRead,
	"list_dns_zones":     model.ToolRead,
	"list_dns_records":   model.ToolRead,
	"check_dns":          model.ToolRead,

	// Write tools: configuration and operations (deletes, unblocking and rollbacks start disabled).
	"update_host_config":       model.ToolConfirm,
	"create_redirect":          model.ToolConfirm,
	"update_redirect":          model.ToolConfirm,
	"create_access_list":       model.ToolConfirm,
	"update_access_list":       model.ToolConfirm,
	"create_stream":            model.ToolConfirm,
	"update_stream":            model.ToolConfirm,
	"create_backend":           model.ToolConfirm,
	"update_backend":           model.ToolConfirm,
	"create_frontend":          model.ToolConfirm,
	"update_frontend":          model.ToolConfirm,
	"expose_backend":           model.ToolConfirm,
	"set_default_host":         model.ToolConfirm,
	"update_settings":          model.ToolConfirm,
	"set_proxy_engine":         model.ToolConfirm,
	"discard_changes":          model.ToolConfirm,
	"engine_action":            model.ToolConfirm,
	"upgrade_engine":           model.ToolConfirm,
	"upgrade_relay":            model.ToolConfirm,
	"create_hosts_from_docker": model.ToolConfirm,
	"renew_certificate":        model.ToolConfirm,
	"create_backup":            model.ToolConfirm,
	"block_ip":                 model.ToolConfirm,
	"check_for_updates":        model.ToolAllow,
	"create_dns_record":        model.ToolConfirm,
	"update_dns_record":        model.ToolConfirm,
	"sync_dns":                 model.ToolConfirm,
	"delete_redirect":          model.ToolDisabled,
	"delete_access_list":       model.ToolDisabled,
	"delete_stream":            model.ToolDisabled,
	"delete_backend":           model.ToolDisabled,
	"delete_frontend":          model.ToolDisabled,
	"delete_dns_record":        model.ToolDisabled,
	"delete_certificate":       model.ToolDisabled,
	"unblock_ip":               model.ToolDisabled,
	"rollback_version":         model.ToolDisabled,
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
	case model.SettingsErrorPages:
		v := DefaultErrorPages()
		return &v
	case model.SettingsPublicDNS:
		v := model.DefaultPublicDNS()
		return &v
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
