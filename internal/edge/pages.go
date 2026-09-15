package edge

import (
	"net/http"
	"strconv"
)

// notFoundPage is the default server's 404 page, identical to the nginx
// renderer's (internal/render/nginx notFoundPage).
const notFoundPage = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>404 Not Found</title></head><body style="margin:0;height:100vh;display:flex;align-items:center;justify-content:center;font:14px system-ui,sans-serif;color:#666;background:#fafaf9">404 · Not found</body></html>`

var statusTitles = map[int]string{
	301: "301 Moved Permanently",
	302: "302 Found",
	307: "307 Temporary Redirect",
	308: "308 Permanent Redirect",
	400: "400 Bad Request",
	401: "401 Authorization Required",
	403: "403 Forbidden",
	404: "404 Not Found",
	405: "405 Not Allowed",
	413: "413 Request Entity Too Large",
	429: "429 Too Many Requests",
	500: "500 Internal Server Error",
	502: "502 Bad Gateway",
	503: "503 Service Temporarily Unavailable",
	504: "504 Gateway Time-out",
}

// pages holds pre-rendered neutral pages (no server branding).
var pages = func() map[int][]byte {
	m := make(map[int][]byte, len(statusTitles))
	for code, title := range statusTitles {
		m[code] = []byte(`<!doctype html><html><head><meta charset="utf-8"><title>` + title + `</title></head><body style="font:14px system-ui,sans-serif;color:#444;text-align:center;padding-top:15vh"><h1 style="font-weight:500">` + title + "</h1></body></html>\n")
	}
	m[404] = []byte(notFoundPage)
	return m
}()

func pageFor(code int) []byte {
	if p, ok := pages[code]; ok {
		return p
	}
	title := strconv.Itoa(code) + " " + http.StatusText(code)
	return []byte("<!doctype html><html><head><title>" + title + "</title></head><body><h1>" + title + "</h1></body></html>\n")
}

// writePage answers with a small HTML page for code.
func writePage(w http.ResponseWriter, code int) {
	writeBody(w, code, "text/html", pageFor(code))
}

func writeBody(w http.ResponseWriter, code int, contentType string, body []byte) {
	h := w.Header()
	h["Content-Type"] = []string{contentType}
	h["Content-Length"] = []string{strconv.Itoa(len(body))}
	w.WriteHeader(code)
	w.Write(body)
}

// writeRedirect answers with a redirect and nginx's small HTML body.
func writeRedirect(w http.ResponseWriter, code int, location string) {
	w.Header()["Location"] = []string{location}
	writePage(w, code)
}
