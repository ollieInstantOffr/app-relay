package logs

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/agent"
	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/store"
)

// fakeLogsAgent serves /v1/logs with fixed lines on a unix socket.
func fakeLogsAgent(t *testing.T, sock string, lines []agent.LogLine) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(agent.LogsResponse{Lines: lines})
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

func TestLBLogSourceFollowsEngine(t *testing.T) {
	app := testApp(t)
	ctx := context.Background()
	dir, err := os.MkdirTemp("/tmp", "rllogs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	at := time.Now().Add(time.Minute).UTC()
	fakeLogsAgent(t, filepath.Join(dir, "balancer.sock"), []agent.LogLine{{At: at, Stream: "stderr", Text: "[ALERT] reload failed: backend api: bad server address"}})
	fakeLogsAgent(t, filepath.Join(dir, "haproxy.sock"), []agent.LogLine{{At: at, Stream: "stderr", Text: "[WARNING] (7) : Server api/api-1 is DOWN"}})
	app.Balancer = agent.NewClient(agent.EngineBalancer, filepath.Join(dir, "balancer.sock"))
	app.HAProxy = agent.NewClient(agent.EngineHAProxy, filepath.Join(dir, "haproxy.sock"))

	gen := store.DefaultGeneral()
	gen.LBEngine = "balancer"
	if err := app.Store.PutSettings(ctx, model.SettingsGeneral, gen); err != nil {
		t.Fatal(err)
	}
	s := &Service{app: app, names: newNameCache()}
	s.ing = newIngester(app, s.names)
	cursors := map[string]time.Time{}
	drain := func() {
		select {
		case c := <-s.ing.in:
			s.ing.add(c)
		case <-time.After(2 * time.Second):
			t.Fatal("no chunk")
		}
		s.ing.flush(ctx)
	}

	s.pollLBOnce(ctx, cursors)
	drain()
	app.SetLBEngine("haproxy")
	s.pollLBOnce(ctx, cursors)
	drain()

	rows, err := app.Store.QueryErrors(ctx, store.ErrorFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Source] = r.Level + " " + r.Message
	}
	if got["balancer"] != "alert reload failed: backend api: bad server address" || got["haproxy"] != "warn Server api/api-1 is DOWN" {
		t.Fatalf("rows = %+v", rows)
	}
	// Each engine keeps its own cursor.
	for _, e := range []string{"balancer", "haproxy"} {
		if b, err := app.Store.GetKV(ctx, kvLBCursor(e)); err != nil || string(b) != at.Format(time.RFC3339Nano) {
			t.Fatalf("cursor %s = %q %v", e, b, err)
		}
	}
	// Nothing new: no chunk.
	s.pollLBOnce(ctx, cursors)
	select {
	case <-s.ing.in:
		t.Fatal("duplicate chunk")
	default:
	}
}
