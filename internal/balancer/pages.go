package balancer

import "net/http"

// errorPages are HAProxy's built-in error responses (errors.c).
var errorPages = map[int][]byte{
	400: []byte("<html><body><h1>400 Bad request</h1>\nYour browser sent an invalid request.\n</body></html>\n"),
	403: []byte("<html><body><h1>403 Forbidden</h1>\nRequest forbidden by administrative rules.\n</body></html>\n"),
	408: []byte("<html><body><h1>408 Request Time-out</h1>\nYour browser didn't send a complete request in time.\n</body></html>\n"),
	500: []byte("<html><body><h1>500 Internal Server Error</h1>\nAn internal server error occurred.\n</body></html>\n"),
	502: []byte("<html><body><h1>502 Bad Gateway</h1>\nThe server returned an invalid or incomplete response.\n</body></html>\n"),
	503: []byte("<html><body><h1>503 Service Unavailable</h1>\nNo server is available to handle this request.\n</body></html>\n"),
	504: []byte("<html><body><h1>504 Gateway Time-out</h1>\nThe server didn't respond in time.\n</body></html>\n"),
}

func errorPage(code int) []byte {
	if b, ok := errorPages[code]; ok {
		return b
	}
	return []byte("<html><body><h1>" + http.StatusText(code) + "</h1>\n</body></html>\n")
}
