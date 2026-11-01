package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Config holds OIDC settings read from environment variables.
type Config struct {
	Enabled      bool   `env:"OIDC_ENABLED"       envDefault:"false"`
	IssuerURL    string `env:"OIDC_ISSUER_URL"`
	ClientID     string `env:"OIDC_CLIENT_ID"`
	ClientSecret string `env:"OIDC_CLIENT_SECRET"`
	RedirectURL  string `env:"OIDC_REDIRECT_URL"`
	// 64 hex chars (32 bytes). If empty, a random key is generated at startup
	// (sessions are lost on restart — acceptable for single-instance deploys).
	SessionKey string `env:"OIDC_SESSION_KEY"`
	// AllowedEmails, if non-empty, restricts login to these addresses (authz on
	// top of authn). Empty = any account from the OIDC provider may log in.
	AllowedEmails []string `env:"OIDC_ALLOWED_EMAILS" envSeparator:","`
	// OperatorEmails, if non-empty, restricts mutating actions (suspend/resume/
	// trigger) to these addresses; everyone else is read-only. Empty = every
	// logged-in user may operate (backwards-compatible default).
	OperatorEmails []string `env:"OIDC_OPERATOR_EMAILS" envSeparator:","`
}

const (
	sessionCookie = "kubecron_session"
	stateCookie   = "kubecron_state"
	sessionTTL    = 24 * time.Hour
)

type ctxKey int

const ctxKeyEmail ctxKey = 0

// EmailFromContext returns the authenticated user's email from the request context.
// Returns "" when OIDC is disabled or the user is not logged in.
func EmailFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyEmail).(string)
	return v
}

// Authenticator handles OIDC login, callback, logout, and session validation.
type Authenticator struct {
	provider   *oidc.Provider
	verifier   *oidc.IDTokenVerifier
	oauth2Cfg  oauth2.Config
	sessionKey []byte
	secure     bool // true when RedirectURL uses https — sets Secure flag on cookies

	// allowedEmails gates login; operatorEmails gates mutating actions. A nil
	// map means "no restriction" for that dimension.
	allowedEmails  map[string]bool
	operatorEmails map[string]bool
}

// emailSet builds a lookup set from a slice, returning nil (meaning "allow all")
// when the slice is empty. Entries are lower-cased for case-insensitive matching.
func emailSet(emails []string) map[string]bool {
	if len(emails) == 0 {
		return nil
	}
	m := make(map[string]bool, len(emails))
	for _, e := range emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" {
			m[e] = true
		}
	}
	return m
}

// session is stored in the signed cookie.
type session struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
	Expiry  int64  `json:"exp"`
}

// NewAuthenticator performs OIDC discovery and returns a ready Authenticator.
func NewAuthenticator(ctx context.Context, cfg Config) (*Authenticator, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("auth: OIDC discovery for %s: %w", cfg.IssuerURL, err)
	}

	var sessKey []byte
	if cfg.SessionKey != "" {
		if len(cfg.SessionKey) < 32 {
			return nil, fmt.Errorf("auth: OIDC_SESSION_KEY must be at least 32 characters")
		}
		derived := sha256.Sum256([]byte(cfg.SessionKey))
		sessKey = derived[:]
	} else {
		slog.Warn("OIDC_SESSION_KEY is not set: a random key is generated; all sessions are invalidated on restart")
		sessKey = make([]byte, 32)
		if _, err := rand.Read(sessKey); err != nil {
			return nil, fmt.Errorf("auth: generate session key: %w", err)
		}
	}

	return &Authenticator{
		provider: provider,
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth2Cfg: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
		sessionKey:     sessKey,
		secure:         strings.HasPrefix(cfg.RedirectURL, "https://"),
		allowedEmails:  emailSet(cfg.AllowedEmails),
		operatorEmails: emailSet(cfg.OperatorEmails),
	}, nil
}

// Secure reports whether the app is served over HTTPS (derived from the OIDC
// redirect URL). Used to set the Secure flag on cookies issued outside this
// package (e.g. the CSRF cookie).
func (a *Authenticator) Secure() bool { return a.secure }

// IsAllowed reports whether email may log in. True when no allow-list is set.
func (a *Authenticator) IsAllowed(email string) bool {
	if a.allowedEmails == nil {
		return true
	}
	return a.allowedEmails[strings.ToLower(email)]
}

// CanOperate reports whether email may perform mutating actions
// (suspend/resume/trigger). True when no operator list is set.
func (a *Authenticator) CanOperate(email string) bool {
	if a.operatorEmails == nil {
		return true
	}
	return a.operatorEmails[strings.ToLower(email)]
}

// RequireOperator wraps a handler so that only users permitted by CanOperate
// may reach it; others receive 403. Used on mutating POST endpoints.
func (a *Authenticator) RequireOperator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.CanOperate(EmailFromContext(r.Context())) {
			http.Error(w, "forbidden: read-only access", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Middleware returns an http.Handler that checks session cookies.
// Exempt paths: /healthz, /readyz, /metrics, /auth/*, /static/*.
// On success, injects the authenticated user's email into the request context.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		// /static/ is the embedded stylesheet and favicon: the logged-out page
		// links them, and behind the guard they answered with a login redirect.
		if p == "/healthz" || p == "/readyz" || p == "/metrics" || strings.HasPrefix(p, "/auth/") || strings.HasPrefix(p, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			redirectToLogin(w, r)
			return
		}
		sess, ok := a.parse(c.Value)
		if !ok {
			redirectToLogin(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyEmail, sess.Email)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// HandleLogin starts the OIDC authorization code flow.
func (a *Authenticator) HandleLogin(w http.ResponseWriter, r *http.Request) {
	state := randStr(16)
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, Path: "/auth/callback",
		MaxAge: 600, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.secure,
	})
	// Validate redirect to prevent open redirect attacks.
	redirect := safeRedirect(r.URL.Query().Get("redirect"), "/")
	// Embed redirect target inside state so it survives the round-trip.
	stateVal := state + ":" + base64.URLEncoding.EncodeToString([]byte(redirect))
	http.Redirect(w, r, a.oauth2Cfg.AuthCodeURL(stateVal), http.StatusFound)
}

// HandleCallback exchanges the authorization code and sets a session cookie.
func (a *Authenticator) HandleCallback(w http.ResponseWriter, r *http.Request) {
	sc, err := r.Cookie(stateCookie)
	if err != nil {
		http.Error(w, "missing state cookie", http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(r.URL.Query().Get("state"), ":", 2)
	if len(parts) != 2 || parts[0] != sc.Value {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	redirect := "/"
	if dec, err := base64.URLEncoding.DecodeString(parts[1]); err == nil {
		redirect = safeRedirect(string(dec), "/")
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", MaxAge: -1, Path: "/auth/callback"})

	token, err := a.oauth2Cfg.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "token exchange failed", http.StatusInternalServerError)
		return
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "no id_token in response", http.StatusInternalServerError)
		return
	}
	idToken, err := a.verifier.Verify(r.Context(), rawID)
	if err != nil {
		http.Error(w, "token verification failed", http.StatusUnauthorized)
		return
	}
	var claims struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "claims extraction failed", http.StatusInternalServerError)
		return
	}

	// Authorization gate: reject accounts not on the allow-list (if configured).
	if !a.IsAllowed(claims.Email) {
		slog.Warn("login denied: email not allowed", "email", claims.Email)
		http.Error(w, "forbidden: this account is not authorized", http.StatusForbidden)
		return
	}

	signed, err := a.sign(session{Subject: claims.Sub, Email: claims.Email, Expiry: time.Now().Add(sessionTTL).Unix()})
	if err != nil {
		http.Error(w, "session signing failed", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: signed, Path: "/",
		MaxAge: int(sessionTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.secure,
	})
	http.Redirect(w, r, redirect, http.StatusFound)
}

// HandleLogout clears the session cookie and shows the logged-out page.
// POST-only (CSRF-protected); the nav button sends it via HTMX, so an
// HX-Redirect header is used to navigate the full page.
func (a *Authenticator) HandleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", MaxAge: -1, Path: "/", HttpOnly: true, Secure: a.secure})
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/auth/logged-out")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/auth/logged-out", http.StatusSeeOther)
}

// HandleLoggedOut renders a simple page with a login button.
// Exempt from auth middleware so users can reach it after logout.
func (a *Authenticator) HandleLoggedOut(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>KubeCron</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;600&family=Syne:wght@400;600;800&display=swap" rel="stylesheet">
<link rel="icon" type="image/svg+xml" href="/static/favicon.svg">
<link rel="stylesheet" href="/static/app.css">
</head>
<body>
<nav>
  <a class="logo" href="/auth/logged-out"><span style="font-family:var(--font-mono);">[KubeCron]</span></a>
</nav>
<div style="display:flex;align-items:center;justify-content:center;min-height:70vh;">
  <div class="card" style="text-align:center;padding:3rem 4rem;max-width:380px;width:100%;">
    <div style="font-family:var(--font-mono);color:var(--accent);font-size:1.4rem;margin-bottom:.5rem;">[KubeCron]</div>
    <div style="color:var(--muted);font-family:var(--font-mono);font-size:0.85rem;margin-bottom:2rem;">You have been logged out.</div>
    <a href="/auth/login" style="display:inline-block;font-family:var(--font-mono);font-size:0.9rem;color:var(--bg);background:var(--accent);border:none;padding:.6rem 2rem;border-radius:6px;text-decoration:none;cursor:pointer;">Login</a>
  </div>
</div>
</body>
</html>`))
}

// sign serialises a session and appends an HMAC-SHA256 signature.
func (a *Authenticator) sign(s session) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	payload := base64.URLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, a.sessionKey)
	mac.Write([]byte(payload))
	sig := base64.URLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + sig, nil
}

// parse verifies the HMAC signature, checks the expiry, and returns the decoded session.
func (a *Authenticator) parse(cookie string) (session, bool) {
	parts := strings.SplitN(cookie, ".", 2)
	if len(parts) != 2 {
		return session{}, false
	}
	mac := hmac.New(sha256.New, a.sessionKey)
	mac.Write([]byte(parts[0]))
	if !hmac.Equal([]byte(base64.URLEncoding.EncodeToString(mac.Sum(nil))), []byte(parts[1])) {
		return session{}, false
	}
	b, err := base64.URLEncoding.DecodeString(parts[0])
	if err != nil {
		return session{}, false
	}
	var s session
	if err := json.Unmarshal(b, &s); err != nil {
		return session{}, false
	}
	if time.Now().Unix() >= s.Expiry {
		return session{}, false
	}
	return s, true
}

// safeRedirect returns raw if it is a safe same-origin relative path,
// or fallback otherwise. Prevents open redirect attacks.
func safeRedirect(raw, fallback string) string {
	// Browsers read a backslash as a slash, so "/\evil.com" is the
	// protocol-relative "//evil.com"; control characters (a tab or newline is
	// stripped before parsing) can smuggle the same shape past the "//" check.
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") ||
		strings.ContainsFunc(raw, func(c rune) bool { return c == '\\' || c < 0x20 || c == 0x7f }) {
		return fallback
	}
	return raw
}

// redirectToLogin sends the user to /auth/login.
// For HTMX requests it uses HX-Redirect so the full page navigates instead of
// injecting the login page into the current HTMX target element.
func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	// Escaped, or a query string in the original URL ends the redirect
	// parameter at its first "&".
	target := "/auth/login?redirect=" + url.QueryEscape(r.URL.RequestURI())
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func randStr(n int) string {
	b := make([]byte, n)
	rand.Read(b) //nolint:errcheck
	return base64.URLEncoding.EncodeToString(b)[:n]
}
