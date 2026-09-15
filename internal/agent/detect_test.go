package agent

import (
	"strings"
	"testing"
)

const officialNginxV = `nginx version: nginx/1.30.4
built by gcc 15.2.0 (Alpine 15.2.0)
configure arguments: --prefix=/etc/nginx --sbin-path=/usr/sbin/nginx --modules-path=/usr/lib/nginx/modules --with-http_auth_request_module --with-http_stub_status_module --with-http_v2_module --with-http_v3_module --with-mail --with-stream --with-stream_realip_module --with-stream_ssl_module --with-stream_ssl_preread_module`

const alpinePkgNginxV = `nginx version: nginx/1.28.0
configure arguments: --prefix=/var/lib/nginx --modules-path=/usr/lib/nginx/modules --with-http_auth_request_module --with-http_stub_status_module --with-http_v2_module --with-http_v3_module --with-stream=dynamic --with-stream_ssl_module`

func TestParseNginxV(t *testing.T) {
	none := func(string) string { return "" }
	v, mods, dyn := parseNginxV(officialNginxV, none, false)
	if v != "1.30.4" || strings.Join(mods, ",") != "http_v3,http_v2,auth_request,stub_status,stream" || dyn != nil {
		t.Fatalf("official: %s %v %v", v, mods, dyn)
	}

	so := func(name string) string {
		if name == "ngx_stream_module.so" || name == "ngx_http_geoip2_module.so" {
			return "/usr/lib/nginx/modules/" + name
		}
		return ""
	}
	v, mods, dyn = parseNginxV(alpinePkgNginxV, so, true)
	if v != "1.28.0" || strings.Join(mods, ",") != "http_v3,http_v2,auth_request,stub_status,stream,geoip2,ipv6" {
		t.Fatalf("alpine: %s %v", v, mods)
	}
	if dyn["stream"] != "/usr/lib/nginx/modules/ngx_stream_module.so" || dyn["geoip2"] == "" {
		t.Fatalf("dynamic = %v", dyn)
	}
	// --with-stream=dynamic without the .so: stream unavailable.
	_, mods, _ = parseNginxV(alpinePkgNginxV, none, false)
	if strings.Contains(strings.Join(mods, ","), "stream") {
		t.Fatalf("stream without .so: %v", mods)
	}
}

func TestBootstrapStatusPort(t *testing.T) {
	t.Setenv(StatusPortEnv, "19999")
	n := &nginxEngine{a: &Agent{o: Options{DataDir: "/data"}}}
	conf := n.bootstrap()["nginx.conf"]
	if !strings.Contains(conf, "listen 127.0.0.1:19999;") || strings.Contains(conf, "/etc/nginx/modules") {
		t.Fatalf("bootstrap:\n%s", conf)
	}
}
