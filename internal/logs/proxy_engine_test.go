package logs

import (
	"context"
	"testing"
	"time"

	"github.com/instantoffr/relay/internal/store"
)

func TestErrorLogSourceFollowsProxyEngine(t *testing.T) {
	app := testApp(t)
	ctx := context.Background()
	g := newIngester(app, newNameCache())
	ts := time.Now().UTC().Add(-time.Minute).Format("2006/01/02 15:04:05")

	g.add(chunk{src: srcNginxError, lines: [][]byte{[]byte(ts + " [error] 7#7: *1 connect() failed (111: Connection refused)")}})
	engine := "edge"
	g.proxyEngine = func() string { return engine }
	g.add(chunk{src: srcNginxError, lines: [][]byte{[]byte(ts + " [emerg] reload failed: host a.home.lan: bad upstream")}})
	g.flush(ctx)

	rows, err := app.Store.QueryErrors(ctx, store.ErrorFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Source] = r.Level + " " + r.Message
	}
	if got["nginx"] != "error connect() failed (111: Connection refused)" || got["edge"] != "emerg reload failed: host a.home.lan: bad upstream" {
		t.Fatalf("rows = %+v", rows)
	}
}
