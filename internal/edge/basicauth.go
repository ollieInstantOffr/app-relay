package edge

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// BasicUser is one htpasswd-style credential.
type BasicUser struct {
	Username     string `json:"username"`
	PasswordHash string `json:"passwordHash"`
}

// basicAuth checks HTTP basic credentials. Unlike nginx, which re-hashes the
// password on every request, successful checks are cached for a short time so
// bcrypt-protected hosts stay fast under load.
type basicAuth struct {
	realm string
	users map[string]string

	mu    sync.Mutex
	cache map[[32]byte]time.Time
}

const (
	authCacheTTL  = time.Minute
	authCacheSize = 4096
)

func newBasicAuth(realm string, users []BasicUser) *basicAuth {
	b := &basicAuth{realm: realm, users: make(map[string]string, len(users)), cache: map[[32]byte]time.Time{}}
	if b.realm == "" {
		b.realm = "Restricted"
	}
	for _, u := range users {
		b.users[u.Username] = u.PasswordHash
	}
	return b
}

// check returns the authenticated username.
func (b *basicAuth) check(r *http.Request) (string, bool) {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return "", false
	}
	hash, known := b.users[user]
	if !known {
		// Spend comparable time so usernames can't be probed by timing.
		bcrypt.CompareHashAndPassword([]byte("$2a$10$7EqJtq98hPqEX7fNZaFWoOhi5BWX4Z3ZhsLr3M4Rr3BjB0R2p6r2C"), []byte(pass))
		return "", false
	}
	key := sha256.Sum256([]byte(user + "\x00" + pass + "\x00" + hash))
	now := time.Now()
	b.mu.Lock()
	if exp, hit := b.cache[key]; hit && now.Before(exp) {
		b.mu.Unlock()
		return user, true
	}
	b.mu.Unlock()
	if !verifyPassword(hash, pass) {
		return "", false
	}
	b.mu.Lock()
	if len(b.cache) >= authCacheSize {
		for k, exp := range b.cache {
			if now.After(exp) || len(b.cache) >= authCacheSize {
				delete(b.cache, k)
			}
		}
	}
	b.cache[key] = now.Add(authCacheTTL)
	b.mu.Unlock()
	return user, true
}

func (b *basicAuth) challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="`+strings.ReplaceAll(b.realm, `"`, `'`)+`", charset="UTF-8"`)
	http.Error(w, "401 Authorization Required", http.StatusUnauthorized)
}

// verifyPassword supports the htpasswd formats nginx accepts: bcrypt
// ($2a$/$2b$/$2y$), Apache MD5 ($apr1$), MD5-crypt ($1$), {SHA} and {PLAIN}.
func verifyPassword(hash, password string) bool {
	switch {
	case strings.HasPrefix(hash, "$2a$"), strings.HasPrefix(hash, "$2b$"), strings.HasPrefix(hash, "$2y$"):
		h := hash
		if strings.HasPrefix(h, "$2y$") {
			h = "$2a$" + h[4:]
		}
		return bcrypt.CompareHashAndPassword([]byte(h), []byte(password)) == nil
	case strings.HasPrefix(hash, "$apr1$"):
		return constEq(md5Crypt(password, hash, "$apr1$"), hash)
	case strings.HasPrefix(hash, "$1$"):
		return constEq(md5Crypt(password, hash, "$1$"), hash)
	case strings.HasPrefix(hash, "{SHA}"):
		sum := sha1.Sum([]byte(password))
		return constEq("{SHA}"+base64.StdEncoding.EncodeToString(sum[:]), hash)
	case strings.HasPrefix(hash, "{PLAIN}"):
		return constEq("{PLAIN}"+password, hash)
	}
	return false
}

func constEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

const itoa64 = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// md5Crypt implements the MD5-based crypt used by $1$ and Apache's $apr1$.
func md5Crypt(password, setting, magic string) string {
	salt := strings.TrimPrefix(setting, magic)
	if i := strings.IndexByte(salt, '$'); i >= 0 {
		salt = salt[:i]
	}
	if len(salt) > 8 {
		salt = salt[:8]
	}
	pw := []byte(password)

	alt := md5.New()
	alt.Write(pw)
	alt.Write([]byte(salt))
	alt.Write(pw)
	altSum := alt.Sum(nil)

	ctx := md5.New()
	ctx.Write(pw)
	ctx.Write([]byte(magic))
	ctx.Write([]byte(salt))
	for n := len(pw); n > 0; n -= 16 {
		if n > 16 {
			ctx.Write(altSum)
		} else {
			ctx.Write(altSum[:n])
		}
	}
	for n := len(pw); n > 0; n >>= 1 {
		if n&1 != 0 {
			ctx.Write([]byte{0})
		} else {
			ctx.Write(pw[:1])
		}
	}
	final := ctx.Sum(nil)

	for i := 0; i < 1000; i++ {
		c := md5.New()
		if i&1 != 0 {
			c.Write(pw)
		} else {
			c.Write(final)
		}
		if i%3 != 0 {
			c.Write([]byte(salt))
		}
		if i%7 != 0 {
			c.Write(pw)
		}
		if i&1 != 0 {
			c.Write(final)
		} else {
			c.Write(pw)
		}
		final = c.Sum(nil)
	}

	var out strings.Builder
	out.WriteString(magic)
	out.WriteString(salt)
	out.WriteByte('$')
	enc := func(v uint32, n int) {
		for ; n > 0; n-- {
			out.WriteByte(itoa64[v&0x3f])
			v >>= 6
		}
	}
	enc(uint32(final[0])<<16|uint32(final[6])<<8|uint32(final[12]), 4)
	enc(uint32(final[1])<<16|uint32(final[7])<<8|uint32(final[13]), 4)
	enc(uint32(final[2])<<16|uint32(final[8])<<8|uint32(final[14]), 4)
	enc(uint32(final[3])<<16|uint32(final[9])<<8|uint32(final[15]), 4)
	enc(uint32(final[4])<<16|uint32(final[10])<<8|uint32(final[5]), 4)
	enc(uint32(final[11]), 2)
	return out.String()
}
