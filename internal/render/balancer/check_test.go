package balancer_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	dataplane "github.com/instantoffr/relay/internal/balancer"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
	renderbalancer "github.com/instantoffr/relay/internal/render/balancer"
)

// TestDataPlaneAcceptsRenderedConfig runs the golden sample and the empty
// snapshot through the data plane's validator. It skips while
// internal/balancer is still a stub.
func TestDataPlaneAcceptsRenderedConfig(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "full.json"))
	if err != nil {
		t.Fatal(err)
	}
	empty, err := renderbalancer.Render(&model.Snapshot{}, render.DefaultEnv("/data", "/run/relay", "/var/log/relay"))
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"full": string(golden), "empty": empty[renderbalancer.ConfigFile]} {
		dir := t.TempDir()
		path := filepath.Join(dir, renderbalancer.ConfigFile)
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := dataplane.Check(path); err != nil {
			if strings.Contains(err.Error(), "not implemented yet") {
				t.Skip("internal/balancer.Check is still a stub; this test activates once the data plane lands")
			}
			t.Errorf("%s: balancer.Check rejected the rendered config: %v", name, err)
		}
	}
}
