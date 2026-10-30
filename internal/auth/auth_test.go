package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func newTestAuth() *Authenticator {
	return &Authenticator{
		sessionKey: []byte("test-key-that-is-exactly-32-bytes"),
	}
}

func TestSignAndParse_RoundTrip(t *testing.T) {
	a := newTestAuth()
	sess := session{
		Subject: "sub123",
		Email:   "user@example.com",
		Expiry:  time.Now().Add(time.Hour).Unix(),
	}
	token, err := a.sign(sess)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, ok := a.parse(token)
	if !ok {
		t.Fatal("parse: expected ok=true")
	}
	if got.Email != sess.Email {
		t.Errorf("email: got %q, want %q", got.Email, sess.Email)
	}
	if got.Subject != sess.Subject {
		t.Errorf("subject: got %q, want %q", got.Subject, sess.Subject)
	}
}

func TestParse_ExpiredSession(t *testing.T) {
	a := newTestAuth()
	sess := session{
		Subject: "sub123",
		Email:   "user@example.com",
		Expiry:  time.Now().Add(-time.Hour).Unix(), // already expired
	}
	token, err := a.sign(sess)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, ok := a.parse(token); ok {
		t.Error("parse: expected ok=false for expired session")
	}
}

func TestParse_TamperedPayload(t *testing.T) {
	a := newTestAuth()
	sess := session{Subject: "x", Email: "x@x.com", Expiry: time.Now().Add(time.Hour).Unix()}
	token, _ := a.sign(sess)
	// Prepend junk to the payload (before the dot).
	if _, ok := a.parse("tampered." + token); ok {
		t.Error("parse: expected ok=false for tampered payload")
	}
}

func TestParse_WrongKey(t *testing.T) {
	a1 := &Authenticator{sessionKey: []byte("key-a-32-bytes-long-padding-here!")}
	a2 := &Authenticator{sessionKey: []byte("key-b-32-bytes-long-padding-here!")}
	sess := session{Subject: "x", Email: "x@x.com", Expiry: time.Now().Add(time.Hour).Unix()}
	token, _ := a1.sign(sess)
	if _, ok := a2.parse(token); ok {
		t.Error("parse: expected ok=false when signed with a different key")
	}
}

func TestSafeRedirect(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"/dashboard", "/dashboard"},
		{"/clusters/foo/bar", "/clusters/foo/bar"},
		{"//evil.com", "/"},            // protocol-relative URL
		{"https://evil.com", "/"},      // absolute HTTPS
		{"http://evil.com/steal", "/"}, // absolute HTTP
		{"javascript:alert(1)", "/"},   // JS injection
		{"", "/"},                      // empty falls back
		{"/\\evil.com", "/"},           // backslash read as a slash by browsers
		{"/\t/evil.com", "/"},          // tab stripped by browsers before parsing
		{"/runs?a=1&b=2", "/runs?a=1&b=2"},
	}
	for _, tc := range tests {
		got := safeRedirect(tc.raw, "/")
		if got != tc.want {
			t.Errorf("safeRedirect(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestIsAllowedAndCanOperate(t *testing.T) {
	// No lists configured → everything permitted (backwards-compatible).
	open := &Authenticator{}
	if !open.IsAllowed("anyone@x.com") || !open.CanOperate("anyone@x.com") {
		t.Error("empty config should allow all")
	}

	a := &Authenticator{
		allowedEmails:  emailSet([]string{"Alice@X.com", "bob@x.com"}),
		operatorEmails: emailSet([]string{"alice@x.com"}),
	}
	cases := []struct {
		email             string
		allowed, operator bool
	}{
		{"alice@x.com", true, true},    // operator (case-insensitive on config)
		{"BOB@x.com", true, false},     // allowed but read-only (case-insensitive input)
		{"eve@evil.com", false, false}, // not allowed at all
	}
	for _, c := range cases {
		if got := a.IsAllowed(c.email); got != c.allowed {
			t.Errorf("IsAllowed(%q) = %v, want %v", c.email, got, c.allowed)
		}
		if got := a.CanOperate(c.email); got != c.operator {
			t.Errorf("CanOperate(%q) = %v, want %v", c.email, got, c.operator)
		}
	}
}

func TestRedirectToLogin_KeepsQueryString(t *testing.T) {
	w := httptest.NewRecorder()
	redirectToLogin(w, httptest.NewRequest("GET", "/runs?a=1&b=2", nil))

	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if got := loc.Query().Get("redirect"); got != "/runs?a=1&b=2" {
		t.Errorf("redirect = %q, want the full original URI", got)
	}
}

func TestMiddleware_StaticIsExempt(t *testing.T) {
	a := newTestAuth()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	w := httptest.NewRecorder()
	a.Middleware(next).ServeHTTP(w, httptest.NewRequest("GET", "/static/app.css", nil))
	if w.Code != http.StatusOK {
		t.Errorf("/static/app.css without a session: got %d, want 200", w.Code)
	}

	w = httptest.NewRecorder()
	a.Middleware(next).ServeHTTP(w, httptest.NewRequest("GET", "/clusters/x", nil))
	if w.Code != http.StatusFound {
		t.Errorf("/clusters/x without a session: got %d, want 302", w.Code)
	}
}
