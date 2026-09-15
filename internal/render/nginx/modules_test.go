package nginx

import (
	"strings"
	"testing"
)

// Distro nginx builds (stream as .so) get load_module lines; the official
// image (everything static) gets none.
func TestDynamicModulesLoaded(t *testing.T) {
	env := testEnv()
	env.ModulePaths = map[string]string{"stream": "/usr/lib/nginx/modules/ngx_stream_module.so", "geoip2": "/usr/lib/nginx/modules/ngx_http_geoip2_module.so"}
	files, err := Render(richSnapshot(), env)
	if err != nil {
		t.Fatal(err)
	}
	f := files["nginx.conf"]
	mustContain(t, "nginx.conf", f, "load_module /usr/lib/nginx/modules/ngx_stream_module.so;")
	if i, j := strings.Index(f, "load_module"), strings.Index(f, "events {"); i < 0 || i > j {
		t.Fatal("load_module must precede the events block")
	}
	// Geo-blocking uses a geo include, so the third-party geoip2 module is never loaded.
	if strings.Contains(f, "geoip2_module") {
		t.Fatal("geoip2 must not be loaded")
	}
}
