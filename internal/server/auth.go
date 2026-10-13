package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"filmeditor/internal/store"
)

// Accounts are local to this installation: email + password, sessions in an
// HttpOnly cookie, passwords bcrypt-hashed, session tokens stored only as
// SHA-256 hashes. State-changing requests must carry X-Splice: 1, which a
// cross-site page cannot add without a CORS preflight the server refuses.

const (
	sessionCookie = "splice_session"
	sessionTTL    = 30 * 24 * time.Hour
	bcryptCost    = 12
	maxPassword   = 72 // bcrypt truncates beyond this; refuse instead of silently weakening
	minPassword   = 8
	authWindow    = 10 * time.Minute
	authAttempts  = 10
)

type ctxKey int

const userKey ctxKey = 1

// user returns the signed-in account for this request, if any.
func userFrom(r *http.Request) (store.User, bool) {
	u, ok := r.Context().Value(userKey).(store.User)
	return u, ok
}

// actor identifies who owns data created by this request.
func (s *Server) actor(r *http.Request) string {
	if u, ok := userFrom(r); ok {
		return "u" + strconv.FormatInt(u.ID, 10)
	}
	return visitorOf(r)
}

func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) setSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	tok := newToken()
	exp := time.Now().Add(sessionTTL)
	if err := s.store.CreateSession(hashToken(tok), userID, exp); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", Expires: exp, MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureRequest(r),
	})
	return nil
}

func clearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureRequest(r),
	})
}

// publicAPI are the endpoints reachable without signing in.
func publicAPI(path string) bool {
	switch path {
	case "/api/health", "/api/auth/signup", "/api/auth/login", "/api/auth/me", "/api/auth/logout":
		return true
	}
	return false
}

// withAuth resolves the session cookie into a user, enforces CSRF protection
// for cookie-authenticated writes, and (when required) refuses anonymous use.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		var hasCookie bool
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" && s.store != nil {
			hasCookie = true
			if u, err := s.store.SessionUser(hashToken(c.Value), time.Now().Add(sessionTTL)); err == nil {
				r = r.WithContext(context.WithValue(r.Context(), userKey, u))
			}
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if hasCookie && r.Header.Get("X-Splice") != "1" {
				fail(w, http.StatusForbidden, "missing X-Splice header")
				return
			}
		}
		if s.cfg.RequireAuth && !publicAPI(r.URL.Path) {
			if _, ok := userFrom(r); !ok {
				fail(w, http.StatusUnauthorized, "sign in required")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// authAllow limits sign-in/sign-up attempts per key (IP or email).
func (s *Server) authAllow(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-authWindow)
	kept := s.authHits[key][:0]
	for _, t := range s.authHits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= authAttempts {
		s.authHits[key] = kept
		return false
	}
	s.authHits[key] = append(kept, time.Now())
	return true
}

type userView struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

func viewOf(u store.User) userView { return userView{ID: u.ID, Email: u.Email, Name: u.Name} }

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(v)
}

func validEmail(e string) bool {
	if len(e) > 254 || !strings.Contains(e, "@") {
		return false
	}
	a, err := mail.ParseAddress(e)
	return err == nil && a.Address == e
}

func validPassword(p string) error {
	switch {
	case len(p) < minPassword:
		return fmt.Errorf("use at least %d characters", minPassword)
	case len(p) > maxPassword:
		return fmt.Errorf("use at most %d characters", maxPassword)
	}
	return nil
}

func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		fail(w, http.StatusNotImplemented, "accounts are not enabled on this server")
		return
	}
	if !s.authAllow("ip:" + clientIP(r)) {
		fail(w, http.StatusTooManyRequests, "too many attempts; wait a few minutes and try again")
		return
	}
	var c credentials
	if err := readJSON(r, &c); err != nil {
		fail(w, http.StatusBadRequest, "send email and password as JSON")
		return
	}
	c.Email = strings.ToLower(strings.TrimSpace(c.Email))
	c.Name = strings.TrimSpace(c.Name)
	if !validEmail(c.Email) {
		fail(w, http.StatusBadRequest, "enter a valid email address")
		return
	}
	if err := validPassword(c.Password); err != nil {
		fail(w, http.StatusBadRequest, "password: "+err.Error())
		return
	}
	if len(c.Name) > 80 {
		c.Name = c.Name[:80]
	}
	if s.cfg.ClosedSignup {
		if n, _ := s.store.UserCount(); n > 0 {
			fail(w, http.StatusForbidden, "sign-ups are closed on this installation")
			return
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(c.Password), bcryptCost)
	if err != nil {
		fail(w, 500, "could not create account")
		return
	}
	u, err := s.store.CreateUser(c.Email, c.Name, string(hash))
	if errors.Is(err, store.ErrEmailTaken) {
		fail(w, http.StatusConflict, "that email already has an account; sign in instead")
		return
	}
	if err != nil {
		fail(w, 500, "could not create account")
		return
	}
	if err := s.setSession(w, r, u.ID); err != nil {
		fail(w, 500, "could not start a session")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": viewOf(u)})
}

// A stand-in hash so unknown emails cost the same time as wrong passwords.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not-a-real-password"), bcryptCost)

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		fail(w, http.StatusNotImplemented, "accounts are not enabled on this server")
		return
	}
	var c credentials
	if err := readJSON(r, &c); err != nil {
		fail(w, http.StatusBadRequest, "send email and password as JSON")
		return
	}
	email := strings.ToLower(strings.TrimSpace(c.Email))
	if !s.authAllow("ip:"+clientIP(r)) || !s.authAllow("email:"+email) {
		fail(w, http.StatusTooManyRequests, "too many attempts; wait a few minutes and try again")
		return
	}
	u, err := s.store.UserByEmail(email)
	hash := dummyHash
	if err == nil {
		hash = []byte(u.PassHash)
	}
	okPass := bcrypt.CompareHashAndPassword(hash, []byte(c.Password)) == nil
	if err != nil || !okPass {
		fail(w, http.StatusUnauthorized, "that email and password don't match")
		return
	}
	if err := s.setSession(w, r, u.ID); err != nil {
		fail(w, 500, "could not start a session")
		return
	}
	writeJSON(w, 200, map[string]any{"user": viewOf(u)})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && s.store != nil {
		_ = s.store.DeleteSession(hashToken(c.Value))
	}
	clearSession(w, r)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// authMe tells the page who is signed in (or that nobody is), and whether
// accounts are required, so it can show the right first screen.
func (s *Server) authMe(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"required": s.cfg.RequireAuth, "enabled": s.store != nil, "user": nil}
	if u, ok := userFrom(r); ok {
		resp["user"] = viewOf(u)
	}
	if s.store != nil && s.cfg.ClosedSignup {
		n, _ := s.store.UserCount()
		resp["signup_open"] = n == 0
	} else {
		resp["signup_open"] = s.store != nil
	}
	writeJSON(w, 200, resp)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "sign in required")
		return
	}
	var body struct {
		Current string `json:"current"`
		Next    string `json:"next"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "send current and next password as JSON")
		return
	}
	if !s.authAllow("pw:" + strconv.FormatInt(u.ID, 10)) {
		fail(w, http.StatusTooManyRequests, "too many attempts; wait a few minutes")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PassHash), []byte(body.Current)) != nil {
		fail(w, http.StatusUnauthorized, "your current password is wrong")
		return
	}
	if err := validPassword(body.Next); err != nil {
		fail(w, http.StatusBadRequest, "password: "+err.Error())
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.Next), bcryptCost)
	if err != nil {
		fail(w, 500, "could not change password")
		return
	}
	if err := s.store.SetPassword(u.ID, string(hash)); err != nil {
		fail(w, 500, "could not change password")
		return
	}
	// Sign out everywhere, then keep this browser signed in on a fresh session.
	_ = s.store.DeleteUserSessions(u.ID)
	if err := s.setSession(w, r, u.ID); err != nil {
		fail(w, 500, "password changed; please sign in again")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
