package store

import (
	"context"
	"database/sql"

	"github.com/instantoffr/relay/internal/model"
)

// Snapshot reads the complete desired configuration.
func (s *Store) Snapshot(ctx context.Context) (*model.Snapshot, error) {
	snap := &model.Snapshot{}
	var err error
	if snap.Hosts, err = s.Hosts().List(ctx); err != nil {
		return nil, err
	}
	if snap.Redirects, err = s.Redirects().List(ctx); err != nil {
		return nil, err
	}
	if snap.Streams, err = s.Streams().List(ctx); err != nil {
		return nil, err
	}
	if snap.AccessLists, err = s.AccessLists().List(ctx); err != nil {
		return nil, err
	}
	if snap.Certificates, err = s.Certificates().List(ctx); err != nil {
		return nil, err
	}
	if snap.Backends, err = s.Backends().List(ctx); err != nil {
		return nil, err
	}
	if snap.Frontends, err = s.Frontends().List(ctx); err != nil {
		return nil, err
	}
	snap.General = DefaultGeneral()
	snap.TLS = DefaultTLS()
	snap.DefaultHost = DefaultDefaultHost()
	snap.HAProxy = DefaultHAProxy()
	snap.Blocklist = model.BlocklistSettings{Entries: []model.BlockEntry{}}
	for key, out := range map[string]any{
		model.SettingsGeneral:     &snap.General,
		model.SettingsTLS:         &snap.TLS,
		model.SettingsDefaultHost: &snap.DefaultHost,
		model.SettingsHAProxy:     &snap.HAProxy,
		model.SettingsBlocklist:   &snap.Blocklist,
	} {
		if err := s.GetSettings(ctx, key, out); err != nil {
			return nil, err
		}
	}
	return snap, nil
}

// RestoreSnapshot replaces all configuration entities and config-affecting
// settings with snap. Certificates are not restored (issuance state is not
// versioned); their metadata in snap is only used for rendering.
func (s *Store) RestoreSnapshot(ctx context.Context, snap *model.Snapshot) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if err := replaceAll[model.ProxyHost](ctx, tx, model.KindHost, snap.Hosts); err != nil {
			return err
		}
		if err := replaceAll[model.Redirect](ctx, tx, model.KindRedirect, snap.Redirects); err != nil {
			return err
		}
		if err := replaceAll[model.Stream](ctx, tx, model.KindStream, snap.Streams); err != nil {
			return err
		}
		if err := replaceAll[model.AccessList](ctx, tx, model.KindAccessList, snap.AccessLists); err != nil {
			return err
		}
		if err := replaceAll[model.Backend](ctx, tx, model.KindBackend, snap.Backends); err != nil {
			return err
		}
		if err := replaceAll[model.Frontend](ctx, tx, model.KindFrontend, snap.Frontends); err != nil {
			return err
		}
		for key, v := range map[string]any{
			model.SettingsGeneral:     snap.General,
			model.SettingsTLS:         snap.TLS,
			model.SettingsDefaultHost: snap.DefaultHost,
			model.SettingsHAProxy:     snap.HAProxy,
			model.SettingsBlocklist:   snap.Blocklist,
		} {
			if err := putSettings(ctx, tx, key, v); err != nil {
				return err
			}
		}
		return nil
	})
}
