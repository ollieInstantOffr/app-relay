package edge

import (
	"fmt"

	"github.com/instantoffr/relay/internal/model"
	"github.com/instantoffr/relay/internal/render"
	"github.com/instantoffr/relay/internal/render/nginx"
)

// RenderHost renders one proxy host (which may be a draft not yet saved in
// snap) as it appears in edge.json's hosts, pretty-printed, for previews.
// Same signature as nginx.RenderHost; use nginx.WithHost + Render to validate
// the full configuration with the draft.
func RenderHost(snap *model.Snapshot, host *model.ProxyHost, env render.Env) (string, error) {
	r := newRenderer(nginx.WithHost(snap, host), env)
	h := *host
	h.Enabled = true
	r.checkHost(&h)
	out, err := encodeJSON(r.host(&h, false))
	if err != nil {
		return "", fmt.Errorf("edge render: %w", err)
	}
	return out, r.err()
}

// RenderStream renders one stream as it appears in edge.json's streams for
// previews. The result is empty when the stream can't be rendered (see the error).
func RenderStream(snap *model.Snapshot, stream *model.Stream, env render.Env) (string, error) {
	r := newRenderer(snap, env)
	st := *stream
	st.Enabled = true
	resolved, ok := r.stream(&st)
	if !ok {
		return "", r.err()
	}
	out, err := encodeJSON(resolved)
	if err != nil {
		return "", fmt.Errorf("edge render: %w", err)
	}
	return out, r.err()
}
