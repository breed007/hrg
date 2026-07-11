package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Authentication is optional and off until a password is set. When enabled,
// a single shared password gates the whole UI — this is a single-user
// homelab tool, not a multi-tenant app. The password's bcrypt hash lives in
// the settings table; sessions are in-memory (a restart logs everyone out,
// which is fine).
//
// CSRF protection is always on, independent of auth: because HRG binds
// localhost with no auth by default, a malicious page in another tab could
// otherwise drive every state-changing endpoint. We verify request origin
// rather than plumb tokens through every form — robust for a same-origin
// server-rendered app and invisible to legitimate use.

const (
	setAuthHash    = "auth_password_hash"
	sessionCookie  = "hrg_session"
	sessionTTL     = 7 * 24 * time.Hour
	minPasswordLen = 8
)

// sessionStore is a tiny in-memory session table with sliding expiry.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time // token -> expiry
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: map[string]time.Time{}}
}

func (ss *sessionStore) create() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	ss.mu.Lock()
	ss.sessions[token] = time.Now().Add(sessionTTL)
	ss.mu.Unlock()
	return token, nil
}

func (ss *sessionStore) valid(token string) bool {
	if token == "" {
		return false
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	exp, ok := ss.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(ss.sessions, token)
		return false
	}
	ss.sessions[token] = time.Now().Add(sessionTTL) // sliding
	return true
}

func (ss *sessionStore) destroy(token string) {
	ss.mu.Lock()
	delete(ss.sessions, token)
	ss.mu.Unlock()
}

// destroyAll invalidates every session — used when the password changes.
func (ss *sessionStore) destroyAll() {
	ss.mu.Lock()
	ss.sessions = map[string]time.Time{}
	ss.mu.Unlock()
}

// authEnabled reports whether a password has been set.
func (s *Server) authEnabled(ctx context.Context) (bool, string) {
	settings, err := s.store.Settings(ctx)
	if err != nil {
		// Fail closed only if we already know auth was configured; a
		// settings read error shouldn't lock a fresh open install out.
		s.log.Error("auth: read settings", "err", err)
		return false, ""
	}
	hash := settings[setAuthHash]
	return hash != "", hash
}

// guard is the middleware applied to every request: CSRF first, then auth.
// Returns true if the request may proceed; if it returns false it has
// already written the response.
func (s *Server) guard(w http.ResponseWriter, r *http.Request) bool {
	// Health check and static assets are always reachable (no secrets, and
	// the container's healthcheck must work before login).
	if r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/static/") {
		return true
	}

	// CSRF: reject cross-origin state-changing requests. GET/HEAD are safe.
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
		if !sameOrigin(r) {
			http.Error(w, "cross-origin request blocked (CSRF protection)", http.StatusForbidden)
			return false
		}
	}

	enabled, _ := s.authEnabled(r.Context())
	if !enabled {
		return true
	}

	// The login page is reachable without a session.
	if r.URL.Path == "/login" {
		return true
	}

	c, _ := r.Cookie(sessionCookie)
	if c != nil && s.sessions.valid(c.Value) {
		return true
	}

	// Not authenticated. Redirect browsers to login, 401 API-ish callers.
	if r.Method == http.MethodGet && accepts(r, "text/html") {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
	} else {
		http.Error(w, "authentication required", http.StatusUnauthorized)
	}
	return false
}

// sameOrigin verifies a state-changing request originated from this app.
// Uses the Origin header when present (browsers always send it on
// cross-origin POSTs) and Sec-Fetch-Site as a backstop. A request with
// neither (curl, the CLI) is allowed — those aren't the CSRF threat.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
		// same-site (a sibling subdomain) is still not same-origin; reject.
		if origin := r.Header.Get("Origin"); origin != "" {
			return originHostMatches(origin, r.Host)
		}
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		return originHostMatches(origin, r.Host)
	}
	return true // no browser origin signal — not a forged cross-site form
}

func originHostMatches(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == host
}

func accepts(r *http.Request, mime string) bool {
	return strings.Contains(r.Header.Get("Accept"), mime) || r.Header.Get("Accept") == ""
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	enabled, hash := s.authEnabled(r.Context())
	if !enabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	next := r.FormValue("next")
	if next == "" || !strings.HasPrefix(next, "/") {
		next = "/"
	}

	if r.Method == http.MethodPost {
		if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(r.FormValue("password"))); err != nil {
			s.render(w, "login", "layout", map[string]any{"Title": "Sign in", "Next": next, "Error": "Incorrect password."})
			return
		}
		token, err := s.sessions.create()
		if err != nil {
			s.fail(w, err)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookie,
			Value:    token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   int(sessionTTL.Seconds()),
		})
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	s.render(w, "login", "layout", map[string]any{"Title": "Sign in", "Next": next})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, _ := r.Cookie(sessionCookie); c != nil {
		s.sessions.destroy(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// handleSetPassword sets, changes, or clears the access password. Setting or
// changing it requires the current password once one exists; clearing
// requires it too (so an XSS/CSRF can't silently disable auth). When auth is
// not yet configured, it can be set freely (first-run / setup wizard).
func (s *Server) handleSetPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	enabled, hash := s.authEnabled(ctx)
	current := r.FormValue("current_password")
	next := r.FormValue("new_password")
	back := r.FormValue("back")
	if back == "" || !strings.HasPrefix(back, "/") {
		back = "/settings"
	}

	if enabled {
		if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(current)); err != nil {
			http.Redirect(w, r, back+"?err="+url.QueryEscape("Current password is incorrect."), http.StatusSeeOther)
			return
		}
	}

	if next == "" {
		// Clear the password → disable auth.
		if err := s.store.SetSetting(ctx, setAuthHash, ""); err != nil {
			s.fail(w, err)
			return
		}
		s.sessions.destroyAll()
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	if len(next) < minPasswordLen {
		http.Redirect(w, r, back+"?err="+url.QueryEscape("Password must be at least 8 characters."), http.StatusSeeOther)
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(next), bcrypt.DefaultCost)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.SetSetting(ctx, setAuthHash, string(newHash)); err != nil {
		s.fail(w, err)
		return
	}
	s.sessions.destroyAll() // force re-login everywhere with the new password
	http.Redirect(w, r, back, http.StatusSeeOther)
}
