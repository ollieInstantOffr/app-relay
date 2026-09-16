package pair

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"time"
)

const (
	tokenPrefix = "rlypair1_"
	tokenRawLen = 32 + 8
)

var tokenEncoding = base64.RawURLEncoding.Strict()

// Token is a one-time pairing token: random secret + expiry, encoded as text.
// Whoever holds it can pair, so callers must treat it as a secret and discard
// it after one successful pairing.
type Token struct {
	Secret  [32]byte
	Expires time.Time
}

// NewToken returns a random token expiring ttl from now (truncated to the second).
func NewToken(ttl time.Duration) (Token, error) {
	if ttl < time.Second {
		return Token{}, errors.New("pair: token ttl must be at least 1s")
	}
	var t Token
	if _, err := rand.Read(t.Secret[:]); err != nil {
		return Token{}, err
	}
	t.Expires = time.Unix(time.Now().Add(ttl).Unix(), 0)
	return t, nil
}

// String encodes the token as "rlypair1_" + base64url(secret || expiry), where
// expiry is Unix seconds as 8 bytes big-endian (63 characters in total).
func (t Token) String() string {
	var raw [tokenRawLen]byte
	copy(raw[:32], t.Secret[:])
	binary.BigEndian.PutUint64(raw[32:], t.expiry())
	return tokenPrefix + tokenEncoding.EncodeToString(raw[:])
}

// ParseToken decodes String's output. It rejects any other prefix, length,
// alphabet, non-canonical encoding or a non-positive expiry; callers trim input.
func ParseToken(s string) (Token, error) {
	rest, ok := strings.CutPrefix(s, tokenPrefix)
	if !ok || len(rest) != tokenEncoding.EncodedLen(tokenRawLen) {
		return Token{}, errors.New("pair: malformed pairing token")
	}
	raw, err := tokenEncoding.DecodeString(rest)
	if err != nil || len(raw) != tokenRawLen {
		return Token{}, errors.New("pair: malformed pairing token")
	}
	exp := binary.BigEndian.Uint64(raw[32:])
	if exp == 0 || exp > math.MaxInt64 {
		return Token{}, errors.New("pair: malformed pairing token")
	}
	var t Token
	copy(t.Secret[:], raw[:32])
	t.Expires = time.Unix(int64(exp), 0)
	return t, nil
}

// Expired reports whether the token is no longer valid at now.
func (t Token) Expired(now time.Time) bool { return !now.Before(t.Expires) }

func (t Token) expiry() uint64 {
	u := t.Expires.Unix()
	if u < 0 {
		return 0
	}
	return uint64(u)
}
