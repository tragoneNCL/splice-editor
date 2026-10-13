package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"filmeditor/internal/synth"
)

type acct struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newAcct(t *testing.T, base string) *acct {
	jar, _ := cookiejar.New(nil)
	return &acct{t: t, base: base, c: &http.Client{Jar: jar}}
}

func (a *acct) do(method, path string, body any, hdr map[string]string) (*http.Response, []byte) {
	a.t.Helper()
	var rd io.Reader
	ct := ""
	switch b := body.(type) {
	case nil:
	case *bytes.Buffer:
		rd = b
	default:
		raw, _ := json.Marshal(b)
		rd, ct = bytes.NewReader(raw), "application/json"
	}
	req, _ := http.NewRequest(method, a.base+path, rd)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	req.Header.Set("X-Splice", "1")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := a.c.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func (a *acct) signup(email, pw string) int {
	resp, _ := a.do("POST", "/api/auth/signup", map[string]string{"email": email, "password": pw, "name": "T"}, nil)
	return resp.StatusCode
}

func authServer(t *testing.T, mod func(*Config)) *httptest.Server {
	ts, _ := newTestServer(t, func(c *Config) {
		c.RequireAuth = true
		c.RatePerHour = 1000
		if mod != nil {
			mod(c)
		}
	})
	return ts
}

func TestSignUpSignInAndOut(t *testing.T) {
	ts := authServer(t, nil)
	a := newAcct(t, ts.URL)

	if resp, _ := a.do("GET", "/api/me", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("anonymous /api/me = %d, want 401", resp.StatusCode)
	}
	if resp, _ := a.do("GET", "/api/health", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("health must stay public, got %d", resp.StatusCode)
	}
	var me map[string]any
	_, raw := a.do("GET", "/api/auth/me", nil, nil)
	json.Unmarshal(raw, &me)
	if me["user"] != nil || me["required"] != true || me["signup_open"] != true {
		t.Fatalf("anonymous auth/me = %v", me)
	}

	if code := a.signup("Ada@Example.com", "correct horse"); code != 201 {
		t.Fatalf("signup = %d", code)
	}
	if resp, _ := a.do("GET", "/api/me", nil, nil); resp.StatusCode != 200 {
		t.Errorf("signed-in /api/me = %d", resp.StatusCode)
	}
	_, raw = a.do("GET", "/api/auth/me", nil, nil)
	json.Unmarshal(raw, &me)
	if u, _ := me["user"].(map[string]any); u == nil || u["email"] != "ada@example.com" {
		t.Errorf("auth/me user = %v", me["user"])
	}

	a.do("POST", "/api/auth/logout", nil, nil)
	if resp, _ := a.do("GET", "/api/me", nil, nil); resp.StatusCode != 401 {
		t.Errorf("after logout /api/me = %d, want 401", resp.StatusCode)
	}
	// And sign back in with different letter case in the email.
	resp, _ := a.do("POST", "/api/auth/login", map[string]string{"email": "ADA@example.com", "password": "correct horse"}, nil)
	if resp.StatusCode != 200 {
		t.Errorf("login = %d", resp.StatusCode)
	}
}

func TestSessionCookieIsHardened(t *testing.T) {
	ts := authServer(t, nil)
	a := newAcct(t, ts.URL)
	resp, _ := a.do("POST", "/api/auth/signup", map[string]string{"email": "a@b.co", "password": "longenough1"},
		map[string]string{"X-Forwarded-Proto": "https"})
	var c *http.Cookie
	for _, k := range resp.Cookies() {
		if k.Name == sessionCookie {
			c = k
		}
	}
	if c == nil {
		t.Fatal("no session cookie")
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || !c.Secure {
		t.Errorf("cookie flags: httponly=%v samesite=%v secure=%v", c.HttpOnly, c.SameSite, c.Secure)
	}
	if len(c.Value) < 40 {
		t.Errorf("session token looks too short to be unguessable: %d chars", len(c.Value))
	}
}

func TestCredentialValidationAndNoUserEnumeration(t *testing.T) {
	ts := authServer(t, nil)
	a := newAcct(t, ts.URL)
	cases := map[string][2]string{
		"bad email":      {"not-an-email", "longenough1"},
		"short password": {"x@y.co", "short"},
		"huge password":  {"x@y.co", strings.Repeat("p", 80)},
	}
	for name, c := range cases {
		if code := a.signup(c[0], c[1]); code != 400 {
			t.Errorf("%s: status %d, want 400", name, code)
		}
	}
	if code := a.signup("dup@y.co", "longenough1"); code != 201 {
		t.Fatalf("first signup %d", code)
	}
	if code := newAcct(t, ts.URL).signup("DUP@y.co", "longenough1"); code != 409 {
		t.Errorf("duplicate email = %d, want 409", code)
	}

	b := newAcct(t, ts.URL)
	r1, body1 := b.do("POST", "/api/auth/login", map[string]string{"email": "dup@y.co", "password": "wrong-password"}, nil)
	r2, body2 := b.do("POST", "/api/auth/login", map[string]string{"email": "nobody@y.co", "password": "wrong-password"}, nil)
	if r1.StatusCode != 401 || r2.StatusCode != 401 || string(body1) != string(body2) {
		t.Errorf("wrong password and unknown email must be indistinguishable: %d %s / %d %s", r1.StatusCode, body1, r2.StatusCode, body2)
	}
}

func TestBruteForceIsLimited(t *testing.T) {
	ts := authServer(t, nil)
	a := newAcct(t, ts.URL)
	a.signup("victim@y.co", "longenough1")
	b := newAcct(t, ts.URL)
	var last int
	for i := 0; i < authAttempts+3; i++ {
		resp, _ := b.do("POST", "/api/auth/login", map[string]string{"email": "victim@y.co", "password": "guess-" + string(rune('a'+i))}, nil)
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("after many wrong guesses status = %d, want 429", last)
	}
	// Even the right password is refused while locked out.
	resp, _ := b.do("POST", "/api/auth/login", map[string]string{"email": "victim@y.co", "password": "longenough1"}, nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("correct password during lockout = %d, want 429", resp.StatusCode)
	}
}

func TestCSRFHeaderRequiredForCookieWrites(t *testing.T) {
	ts := authServer(t, nil)
	a := newAcct(t, ts.URL)
	a.signup("c@y.co", "longenough1")
	// Same cookie, but without the custom header a cross-site form could not add.
	req, _ := http.NewRequest("PUT", ts.URL+"/api/me/prefs", strings.NewReader(`{"preset":"1:1"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cookie write without X-Splice = %d, want 403", resp.StatusCode)
	}
	if r, _ := a.do("PUT", "/api/me/prefs", map[string]string{"preset": "1:1"}, nil); r.StatusCode != 200 {
		t.Errorf("with header = %d", r.StatusCode)
	}
}

func mp(t *testing.T, name string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("duration", "5")
	fw, _ := w.CreateFormFile("clips", name)
	fw.Write(clipBytes(t, synth.RedCircle, 2))
	w.Close()
	return &buf, w.FormDataContentType()
}

func TestAccountsCannotSeeEachOthersWork(t *testing.T) {
	ts := authServer(t, nil)
	alice, bob := newAcct(t, ts.URL), newAcct(t, ts.URL)
	alice.signup("alice@y.co", "longenough1")
	bob.signup("bob@y.co", "longenough1")

	body, ct := mp(t, "a.mp4")
	resp, raw := alice.do("POST", "/api/jobs", body, map[string]string{"Content-Type": ct})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("alice upload: %d %s", resp.StatusCode, raw)
	}
	var id struct{ ID string }
	json.Unmarshal(raw, &id)

	getAs := func(a *acct, path string) int {
		r, _ := a.do("GET", path, nil, nil)
		return r.StatusCode
	}
	for i := 0; i < 300; i++ { // poll as the owner (anonymous polls are rightly refused)
		_, b := alice.do("GET", "/api/jobs/"+id.ID, nil, nil)
		var v JobView
		json.Unmarshal(b, &v)
		if v.Status == "done" || v.Status == "error" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if c := getAs(alice, "/api/jobs/"+id.ID); c != 200 {
		t.Errorf("owner sees own job: %d", c)
	}
	for _, p := range []string{"/api/jobs/" + id.ID, "/api/jobs/" + id.ID + "/video"} {
		if c := getAs(bob, p); c != 404 {
			t.Errorf("another account got %d on %s, want 404", c, p)
		}
	}
	if r, _ := bob.do("POST", "/api/jobs/"+id.ID+"/undo", nil, nil); r.StatusCode != 404 {
		t.Errorf("another account's undo = %d, want 404", r.StatusCode)
	}
	if c := getAs(newAcct(t, ts.URL), "/api/jobs/"+id.ID); c != 401 {
		t.Errorf("signed-out access = %d, want 401", c)
	}

	// Preferences are per account too.
	alice.do("PUT", "/api/me/prefs", map[string]string{"preset": "1:1"}, nil)
	_, raw = bob.do("GET", "/api/me", nil, nil)
	if strings.Contains(string(raw), "1:1") {
		t.Errorf("preferences leaked between accounts: %s", raw)
	}
}

func TestChangingPasswordSignsOutOtherDevices(t *testing.T) {
	ts := authServer(t, nil)
	laptop, phone := newAcct(t, ts.URL), newAcct(t, ts.URL)
	laptop.signup("p@y.co", "old-password-1")
	phone.do("POST", "/api/auth/login", map[string]string{"email": "p@y.co", "password": "old-password-1"}, nil)
	if r, _ := phone.do("GET", "/api/me", nil, nil); r.StatusCode != 200 {
		t.Fatalf("phone should be signed in: %d", r.StatusCode)
	}

	if r, _ := laptop.do("POST", "/api/auth/password", map[string]string{"current": "WRONG", "next": "new-password-1"}, nil); r.StatusCode != 401 {
		t.Errorf("wrong current password = %d", r.StatusCode)
	}
	if r, _ := laptop.do("POST", "/api/auth/password", map[string]string{"current": "old-password-1", "next": "new-password-1"}, nil); r.StatusCode != 200 {
		t.Fatalf("change = %d", r.StatusCode)
	}
	if r, _ := phone.do("GET", "/api/me", nil, nil); r.StatusCode != 401 {
		t.Errorf("other device should be signed out, got %d", r.StatusCode)
	}
	if r, _ := laptop.do("GET", "/api/me", nil, nil); r.StatusCode != 200 {
		t.Errorf("this device should stay signed in, got %d", r.StatusCode)
	}
	if r, _ := newAcct(t, ts.URL).do("POST", "/api/auth/login", map[string]string{"email": "p@y.co", "password": "new-password-1"}, nil); r.StatusCode != 200 {
		t.Errorf("new password should work, got %d", r.StatusCode)
	}
}

func TestClosedSignupAllowsOnlyTheFirstAccount(t *testing.T) {
	ts := authServer(t, func(c *Config) { c.ClosedSignup = true })
	if code := newAcct(t, ts.URL).signup("first@y.co", "longenough1"); code != 201 {
		t.Fatalf("first = %d", code)
	}
	if code := newAcct(t, ts.URL).signup("second@y.co", "longenough1"); code != 403 {
		t.Errorf("second = %d, want 403", code)
	}
}
