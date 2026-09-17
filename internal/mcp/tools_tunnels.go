package mcp

// Tools for tunnels (Tunnels page): gateways on public servers that publish
// hosts and TCP streams without port forwarding. Gateway changes take effect
// immediately; publishing a host or stream (tunnelGatewayId) is a pending
// change like any other.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/instantoffr/relay/internal/core"
)

type gatewayCreateArgs struct {
	Name      string `json:"name" jsonschema:"Short name, e.g. vps-fra"`
	Address   string `json:"address" jsonschema:"Public host name or IP of the server, optionally with :port (default 7443)"`
	Transport string `json:"transport,omitempty" jsonschema:"auto (default: QUIC, falling back to TCP), quic or tcp"`
	Reason    string `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

type gatewayUpdateArgs struct {
	Gateway   string `json:"gateway" jsonschema:"Gateway id or name"`
	Name      string `json:"name,omitempty" jsonschema:"New name (empty keeps it)"`
	Address   string `json:"address,omitempty" jsonschema:"New address (empty keeps it)"`
	Transport string `json:"transport,omitempty" jsonschema:"auto, quic or tcp (empty keeps it)"`
	Enabled   *bool  `json:"enabled,omitempty" jsonschema:"Enable or disable the gateway"`
	Reason    string `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

type gatewayRefArgs struct {
	Gateway string `json:"gateway" jsonschema:"Gateway id or name"`
	Reason  string `json:"reason,omitempty" jsonschema:"Why; shown to the person approving it"`
}

func (s *Service) registerTunnelTools() {
	addRead(s, toolInfo{Name: "list_tunnels", Title: "List tunnel gateways",
		Description: "Tunnel gateways with their pairing state, live connection status (connected, transport, round trip, traffic, port errors) and the hosts and streams published through each. Publish a host by setting tunnelGatewayId with update_host_config, a TCP stream with update_stream."},
		nil, func(ctx context.Context, c *call, _ noArgs) (*readResult, error) {
			if err := requireUnrestricted(c, "reading tunnels"); err != nil {
				return nil, err
			}
			return s.apiRead(ctx, c, "/tunnels", nil, "tunnels")
		})

	addWrite(s, toolInfo{Name: "create_gateway", Title: "Connect a tunnel gateway",
		Description: "Create a tunnel gateway and a one-time pairing token. The result contains the command to run on the public server (it includes the token: treat it as a secret). Then call pair_gateway once the gateway runs. Takes effect immediately (not a pending change). May wait for human approval."},
		map[string][]any{"transport": {"auto", "quic", "tcp"}}, false,
		func(ctx context.Context, c *call, in gatewayCreateArgs) (*plan, error) {
			if err := requireUnrestricted(c, "changing tunnels"); err != nil {
				return nil, err
			}
			if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.Address) == "" {
				return nil, errors.New("name and address are required")
			}
			body := map[string]any{"name": in.Name, "address": in.Address, "transport": in.Transport, "enabled": true}
			return &plan{
				Summary: fmt.Sprintf("Connect tunnel gateway %s at %s", bold(in.Name), in.Address), Target: in.Name, Preview: prettyJSON(body), Detail: "gateway create " + in.Address,
				Exec: func(ctx context.Context) (*outcome, error) {
					var g map[string]any
					if err := s.apiCall(ctx, http.MethodPost, "/gateways", body, &g); err != nil {
						return nil, err
					}
					var p map[string]any
					if err := s.apiCall(ctx, http.MethodPost, "/gateways/"+url.PathEscape(mStr(g["id"]))+"/pairing", nil, &p); err != nil {
						return nil, err
					}
					text := fmt.Sprintf("Created gateway %s (id %s). On the server (as root, over SSH), run the one-line installer. It installs Docker if needed, builds and starts the gateway and opens ports %s in ufw/firewalld:\n\n%s\n\nAlso allow those ports in the hosting provider's firewall, then call pair_gateway.", in.Name, mStr(g["id"]), strings.Join(mStrs(p["ports"]), ", "), mStr(p["install"]))
					return &outcome{Text: text, Structured: map[string]any{"gateway": g, "pairing": p}}, nil
				},
			}, nil
		})

	addWrite(s, toolInfo{Name: "pair_gateway", Title: "Pair a tunnel gateway",
		Description: "Connect to a pending gateway and pair with its token (after its command runs on the server). Fails with a clear message while the gateway isn't reachable yet. May wait for human approval."},
		nil, false,
		func(ctx context.Context, c *call, in gatewayRefArgs) (*plan, error) {
			if err := requireUnrestricted(c, "changing tunnels"); err != nil {
				return nil, err
			}
			g, err := s.findGateway(core.WithActor(ctx, c.actor), in.Gateway)
			if err != nil {
				return nil, err
			}
			return &plan{
				Summary: "Pair tunnel gateway " + bold(mStr(g["name"])), Target: mStr(g["name"]), Detail: "gateway pair " + mStr(g["address"]),
				Exec: func(ctx context.Context) (*outcome, error) {
					var out map[string]any
					if err := s.apiCall(ctx, http.MethodPost, "/gateways/"+url.PathEscape(mStr(g["id"]))+"/pair", nil, &out); err != nil {
						return nil, err
					}
					return &outcome{Text: fmt.Sprintf("%s is paired. Publish hosts through it with update_host_config (tunnelGatewayId) and apply.", mStr(g["name"])), Structured: out}, nil
				},
			}, nil
		})

	addWrite(s, toolInfo{Name: "update_gateway", Title: "Change a tunnel gateway",
		Description: "Change a gateway's name, address, transport or enabled state. Takes effect immediately (not a pending change); disabling disconnects everything published through it. May wait for human approval."},
		map[string][]any{"transport": {"auto", "quic", "tcp"}}, false,
		func(ctx context.Context, c *call, in gatewayUpdateArgs) (*plan, error) {
			if err := requireUnrestricted(c, "changing tunnels"); err != nil {
				return nil, err
			}
			g, err := s.findGateway(core.WithActor(ctx, c.actor), in.Gateway)
			if err != nil {
				return nil, err
			}
			next := mClone(g)
			var changes []string
			set := func(key, v string) {
				if v != "" && v != mStr(g[key]) {
					next[key] = v
					changes = append(changes, key+" → "+v)
				}
			}
			set("name", in.Name)
			set("address", in.Address)
			set("transport", in.Transport)
			if in.Enabled != nil && *in.Enabled != (g["enabled"] == true) {
				next["enabled"] = *in.Enabled
				changes = append(changes, fmt.Sprintf("enabled → %v", *in.Enabled))
			}
			if len(changes) == 0 {
				return nil, errors.New("nothing to change")
			}
			return &plan{
				Summary: fmt.Sprintf("Update gateway %s: %s", bold(mStr(g["name"])), strings.Join(changes, ", ")), Target: mStr(g["name"]),
				Preview: prettyJSON(next), Detail: "gateway update " + strings.Join(changes, ", "),
				Exec: func(ctx context.Context) (*outcome, error) {
					var out map[string]any
					if err := s.apiCall(ctx, http.MethodPut, "/gateways/"+url.PathEscape(mStr(g["id"])), next, &out); err != nil {
						return nil, err
					}
					return &outcome{Text: "Updated gateway " + mStr(out["name"]) + ": " + strings.Join(changes, ", ") + ".", Structured: out}, nil
				},
			}, nil
		})

	addWrite(s, toolInfo{Name: "delete_gateway", Title: "Delete a tunnel gateway",
		Description: "Delete a tunnel gateway and this Relay's key for it. Refused while hosts or streams are published through it. Takes effect immediately. May wait for human approval."},
		nil, true,
		func(ctx context.Context, c *call, in gatewayRefArgs) (*plan, error) {
			if err := requireUnrestricted(c, "changing tunnels"); err != nil {
				return nil, err
			}
			g, err := s.findGateway(core.WithActor(ctx, c.actor), in.Gateway)
			if err != nil {
				return nil, err
			}
			return &plan{
				Summary: "Delete tunnel gateway " + bold(mStr(g["name"])), Target: mStr(g["name"]), Detail: "gateway delete",
				Exec: func(ctx context.Context) (*outcome, error) {
					if err := s.apiCall(ctx, http.MethodDelete, "/gateways/"+url.PathEscape(mStr(g["id"])), nil, nil); err != nil {
						return nil, err
					}
					return &outcome{Text: "Deleted gateway " + mStr(g["name"]) + "."}, nil
				},
			}, nil
		})
}

// findGateway looks a gateway up by id or name.
func (s *Service) findGateway(ctx context.Context, ref string) (map[string]any, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("gateway is required (id or name)")
	}
	var items []map[string]any
	if err := s.apiCall(ctx, http.MethodGet, "/gateways", nil, &items); err != nil {
		return nil, err
	}
	for _, it := range items {
		if mStr(it["id"]) == ref {
			return it, nil
		}
	}
	for _, it := range items {
		if strings.EqualFold(mStr(it["name"]), ref) {
			return it, nil
		}
	}
	return nil, fmt.Errorf("no tunnel gateway matches %q (see list_tunnels)", ref)
}
