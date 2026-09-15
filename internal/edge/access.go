package edge

import (
	"net/http"
	"net/url"
)

// authorize runs the access phase of a location: IP rules (403), basic auth
// (401) and forward auth, with nginx `satisfy all|any` semantics. It writes
// the response and returns false when the request is refused.
//
// Under satisfy any a check that passes ends the phase; a failing 403/401 is
// remembered (a 401 is never replaced, as in ngx_http_core_access_phase) and
// answers when nothing passes; a forward auth error (500) answers at once.
func (s *Server) authorize(rs *reqState, h *hostRT, loc *locationRT) bool {
	faActive := loc.forwardAuth && h.fa != nil
	rs.stripRemote = faActive
	if loc.denyAll {
		writePage(&rs.fw, http.StatusForbidden)
		return false
	}
	acc := loc.access
	if acc == nil && !faActive {
		return true
	}
	anyOf := acc != nil && acc.satisfyAny
	var fail struct {
		code  int
		basic *basicAuth
		auth  *authResult
	}
	refuse := func() bool {
		s.refuse(rs, h, faActive, fail.code, fail.basic, fail.auth)
		return false
	}
	if acc != nil && acc.rules != nil {
		if allow, matched := acc.rules.decide(rs.clientIP); matched {
			if allow {
				if anyOf {
					return true
				}
			} else {
				fail.code = http.StatusForbidden
				if !anyOf {
					return refuse()
				}
			}
		}
	}
	if acc != nil && acc.basic != nil {
		if _, ok := acc.basic.check(rs.r); ok {
			if anyOf {
				return true
			}
		} else {
			if fail.code != http.StatusUnauthorized {
				fail.code, fail.basic, fail.auth = http.StatusUnauthorized, acc.basic, nil
			}
			if !anyOf {
				return refuse()
			}
		}
	}
	if faActive {
		res := h.fa.check(rs)
		switch {
		case res.status >= 200 && res.status < 300:
			if anyOf {
				return true
			}
		case res.status == http.StatusUnauthorized || res.status == http.StatusForbidden:
			if fail.code != http.StatusUnauthorized {
				fail.code, fail.basic, fail.auth = res.status, nil, res
			}
			if !anyOf {
				return refuse()
			}
		default:
			writePage(&rs.fw, http.StatusInternalServerError)
			return false
		}
	}
	if anyOf && fail.code != 0 {
		return refuse()
	}
	return true
}

// refuse writes the response for a failed access check.
func (s *Server) refuse(rs *reqState, h *hostRT, faActive bool, code int, basic *basicAuth, auth *authResult) {
	w := &rs.fw
	// error_page 401 = @relay_signin on forward-auth locations.
	if code == http.StatusUnauthorized && faActive && h.fa.signIn != "" {
		original := rs.role.scheme + "://" + rs.r.Host + rs.requestURI()
		writeRedirect(w, http.StatusFound, h.fa.signIn+url.QueryEscape(original))
		return
	}
	switch {
	case basic != nil:
		w.Header()["Www-Authenticate"] = []string{basic.challenge}
		writePage(w, http.StatusUnauthorized)
	case auth != nil:
		hdr := w.Header()
		for k, vs := range auth.header {
			hdr[k] = vs
		}
		if len(auth.body) == 0 {
			writePage(w, auth.status)
			return
		}
		delete(hdr, "Content-Length")
		w.WriteHeader(auth.status)
		w.Write(auth.body)
	default:
		writePage(w, code)
	}
}
