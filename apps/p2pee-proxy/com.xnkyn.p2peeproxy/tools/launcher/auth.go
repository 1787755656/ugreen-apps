package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const sessionCookieName = "p2pee_sid"
const sessionTTL = 30 * 24 * time.Hour

// ---- session store (in-memory; single local instance) ----

type SessionStore struct {
	mu     sync.Mutex
	tokens map[string]time.Time
}

func NewSessionStore() *SessionStore {
	return &SessionStore{tokens: map[string]time.Time{}}
}

func (s *SessionStore) New() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	s.tokens[tok] = time.Now().Add(sessionTTL)
	s.mu.Unlock()
	return tok
}

func (s *SessionStore) Valid(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.tokens[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.tokens, tok)
		return false
	}
	return true
}

func (s *SessionStore) Revoke(tok string) {
	if tok == "" {
		return
	}
	s.mu.Lock()
	delete(s.tokens, tok)
	s.mu.Unlock()
}

func (s *SessionStore) setCookie(w http.ResponseWriter, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func (s *SessionStore) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func sessionToken(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// ---- password hashing (PBKDF2-HMAC-SHA256, stdlib only) ----

const pbkdf2Iter = 200000
const saltLen = 16
const keyLen = 32

func pbkdf2Key(pw, salt []byte, iter, dkLen int) []byte {
	if iter <= 0 || dkLen <= 0 {
		return nil
	}
	prf := hmac.New(sha256.New, pw)
	hashLen := sha256.Size
	blocks := (dkLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	u := make([]byte, hashLen)
	t := make([]byte, hashLen)
	ib := []byte{0, 0, 0, 0}
	for i := 1; i <= blocks; i++ {
		ib[0] = byte(i >> 24)
		ib[1] = byte(i >> 16)
		ib[2] = byte(i >> 8)
		ib[3] = byte(i)
		prf.Reset()
		prf.Write(salt)
		prf.Write(ib)
		copy(u, prf.Sum(nil))
		copy(t, u)
		for j := 2; j <= iter; j++ {
			prf.Reset()
			prf.Write(u)
			copy(u, prf.Sum(nil))
			for k := 0; k < hashLen; k++ {
				t[k] ^= u[k]
			}
		}
		out = append(out, t...)
	}
	return out[:dkLen]
}

func hashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := pbkdf2Key([]byte(pw), salt, pbkdf2Iter, keyLen)
	return "pbkdf2$" + strconv.Itoa(pbkdf2Iter) + "$" + hex.EncodeToString(salt) + "$" + hex.EncodeToString(key), nil
}

func verifyPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil {
		return false
	}
	if len(want) == 0 {
		return false
	}
	key := pbkdf2Key([]byte(pw), salt, iter, len(want))
	return subtle.ConstantTimeCompare(key, want) == 1
}
