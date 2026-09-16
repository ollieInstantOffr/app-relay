package tunnel

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
	tunnelcfg "github.com/instantoffr/relay/internal/tunnel"
)

func snapshot() *model.Snapshot {
	return &model.Snapshot{
		General: model.GeneralSettings{ProxyEngine: "edge"},
		Hosts: []model.ProxyHost{
			{Meta: model.Meta{ID: "h1"}, Enabled: true, Domains: []string{"App.example.com", "*.apps.example.com"}, TunnelGatewayID: "gw2"},
			{Meta: model.Meta{ID: "h2"}, Enabled: true, Domains: []string{"wiki.example.com"}, TunnelGatewayID: "gw1"},
			{Meta: model.Meta{ID: "h3"}, Enabled: false, Domains: []string{"off.example.com"}, TunnelGatewayID: "gw1"},
			{Meta: model.Meta{ID: "h4"}, Enabled: true, Domains: []string{"lan.example.com"}},
		},
		Streams: []model.Stream{
			{Meta: model.Meta{ID: "s1"}, Name: "mc", Protocol: "tcp", ListenPorts: "25565", Enabled: true, TunnelGatewayID: "gw1"},
			{Meta: model.Meta{ID: "s2"}, Name: "range", Protocol: "both", ListenPorts: "7000-7001", Enabled: true, TunnelGatewayID: "gw1"},
			{Meta: model.Meta{ID: "s3"}, Name: "udp", Protocol: "udp", ListenPorts: "5000", Enabled: true, TunnelGatewayID: "gw1"},
		},
	}
}

func TestRender(t *testing.T) {
	env := render.DefaultEnv("/data", "/run/relay", "/var/log/relay")
	if !Published(snapshot()) || Published(&model.Snapshot{}) {
		t.Fatal("Published")
	}
	files, err := Render(snapshot(), env)
	if err != nil {
		t.Fatal(err)
	}
	var cfg tunnelcfg.Config
	if err := json.Unmarshal([]byte(files[tunnelcfg.ConfigFile]), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.GatewaysFile != "/data/tunnel/gateways.json" || cfg.RuntimeSocket != "/run/relay/tunnel-runtime.sock" {
		t.Errorf("paths %+v", cfg)
	}
	if cfg.Targets.HTTPS != "/run/relay/tunnel/edge-https.sock" {
		t.Errorf("targets %+v", cfg.Targets)
	}
	if len(cfg.Routes) != 2 || cfg.Routes[0].GatewayID != "gw1" || cfg.Routes[1].GatewayID != "gw2" {
		t.Fatalf("routes %+v", cfg.Routes)
	}
	gw1, gw2 := cfg.Routes[0], cfg.Routes[1]
	if strings.Join(gw1.Names, ",") != "wiki.example.com" || strings.Join(gw2.Names, ",") != "*.apps.example.com,app.example.com" {
		t.Errorf("names %v %v", gw1.Names, gw2.Names)
	}
	if len(gw1.TCP) != 3 || gw1.TCP[0].Port != 7000 || gw1.TCP[2].Port != 25565 || gw1.TCP[2].Socket != "/run/relay/tunnel/edge-stream-25565.sock" {
		t.Errorf("tcp %+v", gw1.TCP)
	}

	s := snapshot()
	s.Streams[1].ListenPorts = "25565-25566"
	if _, err := Render(s, env); err == nil || !strings.Contains(err.Error(), "already published") {
		t.Errorf("duplicate port: %v", err)
	}
	// Deterministic.
	again, _ := Render(snapshot(), env)
	if again[tunnelcfg.ConfigFile] != files[tunnelcfg.ConfigFile] {
		t.Error("not deterministic")
	}
}
