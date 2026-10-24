// Package auth implements NewGit's device-bound accounts.
//
// A browser generates a non-extractable ECDSA P-256 key with WebCrypto and
// keeps it in IndexedDB. The account id is the key's RFC 7638 JWK thumbprint.
// Registration and sign-in are proofs of possession: the device signs a
// single-use server challenge. There are no passwords and no email.
//
// Git over HTTPS and MCP use personal access tokens issued by the device.
package auth

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var b64 = base64.RawURLEncoding

// Auth holds session secrets and outstanding challenges.
type Auth struct {
	secret     []byte
	secure     bool
	mu         sync.Mutex
	challenges map[string]time.Time
}

// New loads or creates the session secret.
func New(secretPath string, secureCookies bool) (*Auth, error) {
	secret, err := os.ReadFile(secretPath)
	if err != nil || len(secret) < 32 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
		if err := os.WriteFile(secretPath, secret, 0o600); err != nil {
			return nil, err
		}
	}
	return &Auth{secret: secret, secure: secureCookies, challenges: map[string]time.Time{}}, nil
}

const challengeTTL = 2 * time.Minute

// NewChallenge issues a single-use nonce.
func (a *Auth) NewChallenge() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	n := b64.EncodeToString(b)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, exp := range a.challenges {
		if now.After(exp) {
			delete(a.challenges, k)
		}
	}
	if len(a.challenges) > 10000 {
		// crude flood protection: drop everything
		a.challenges = map[string]time.Time{}
	}
	a.challenges[n] = now.Add(challengeTTL)
	return n
}

// consumeChallenge checks and removes a nonce.
func (a *Auth) consumeChallenge(n string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.challenges[n]
	if !ok {
		return false
	}
	delete(a.challenges, n)
	return time.Now().Before(exp)
}

// SignedMessage is the exact byte string the device signs.
func SignedMessage(purpose, nonce string) []byte {
	return []byte("newgit-device-auth:v1|" + purpose + "|" + nonce)
}

// Thumbprint computes the RFC 7638 JWK thumbprint of a P-256 key.
func Thumbprint(x, y string) string {
	canon := `{"crv":"P-256","kty":"EC","x":"` + x + `","y":"` + y + `"}`
	h := sha256.Sum256([]byte(canon))
	return b64.EncodeToString(h[:])
}

// ParseKey validates JWK coordinates and returns the public key.
func ParseKey(x, y string) (*ecdsa.PublicKey, error) {
	xb, err := b64.DecodeString(x)
	if err != nil || len(xb) != 32 {
		return nil, errors.New("bad key x")
	}
	yb, err := b64.DecodeString(y)
	if err != nil || len(yb) != 32 {
		return nil, errors.New("bad key y")
	}
	point := append([]byte{4}, append(xb, yb...)...)
	if _, err := ecdh.P256().NewPublicKey(point); err != nil {
		return nil, errors.New("key is not on P-256")
	}
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(xb), Y: new(big.Int).SetBytes(yb)}, nil
}

// VerifyProof checks a WebCrypto ECDSA (P1363 r||s) signature over the
// challenge message and consumes the challenge.
func (a *Auth) VerifyProof(pub *ecdsa.PublicKey, purpose, nonce, sig string) error {
	raw, err := b64.DecodeString(sig)
	if err != nil || len(raw) != 64 {
		return errors.New("bad signature encoding")
	}
	if !a.consumeChallenge(nonce) {
		return errors.New("challenge expired or already used")
	}
	h := sha256.Sum256(SignedMessage(purpose, nonce))
	r := new(big.Int).SetBytes(raw[:32])
	s := new(big.Int).SetBytes(raw[32:])
	if !ecdsa.Verify(pub, h[:], r, s) {
		return errors.New("signature does not match this device key")
	}
	return nil
}

// ---------- sessions ----------

// CookieName is the session cookie.
const CookieName = "ng_session"

const sessionTTL = 7 * 24 * time.Hour

func (a *Auth) mac(parts ...string) []byte {
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte(strings.Join(parts, "|")))
	return m.Sum(nil)
}

// SetSession issues a session cookie for the user.
func (a *Auth) SetSession(w http.ResponseWriter, userID string) {
	exp := time.Now().Add(sessionTTL).Unix()
	payload := userID + "|" + strconv.FormatInt(exp, 10)
	val := b64.EncodeToString([]byte(payload)) + "." + b64.EncodeToString(a.mac("session", payload))
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: val, Path: "/", HttpOnly: true, Secure: a.secure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds()),
	})
}

// ClearSession removes the session cookie.
func (a *Auth) ClearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

// SessionUser returns the user id in a valid session cookie.
func (a *Auth) SessionUser(r *http.Request) (string, string, bool) {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return "", "", false
	}
	p, m, ok := strings.Cut(c.Value, ".")
	if !ok {
		return "", "", false
	}
	payloadB, err := b64.DecodeString(p)
	if err != nil {
		return "", "", false
	}
	macB, err := b64.DecodeString(m)
	if err != nil || !hmac.Equal(macB, a.mac("session", string(payloadB))) {
		return "", "", false
	}
	uid, expS, ok := strings.Cut(string(payloadB), "|")
	if !ok {
		return "", "", false
	}
	exp, err := strconv.ParseInt(expS, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", "", false
	}
	return uid, c.Value, true
}

// CSRFToken derives the CSRF token for a session cookie value.
func (a *Auth) CSRFToken(sessionValue string) string {
	return hex.EncodeToString(a.mac("csrf", sessionValue))[:32]
}

// CheckCSRF validates a submitted CSRF token.
func (a *Auth) CheckCSRF(sessionValue, token string) bool {
	want := a.CSRFToken(sessionValue)
	return subtle.ConstantTimeCompare([]byte(want), []byte(token)) == 1
}

// BasicToken extracts a token from HTTP Basic auth (password) or Bearer.
func BasicToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")), true
	}
	if _, pass, ok := r.BasicAuth(); ok && pass != "" {
		return pass, true
	}
	return "", false
}

// ErrUnauthorized is a generic auth error.
var ErrUnauthorized = fmt.Errorf("unauthorized")
