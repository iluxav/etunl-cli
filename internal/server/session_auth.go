package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/iluxav/etunl-cli/internal/tunnel"
)

const (
	loginPath        = "/___login___"
	logoutPath       = "/___logout___"
	sessionCookie    = "etunl_session"
	sessionTTL       = 24 * time.Hour
	sessionSeparator = "|"
)

// signSession returns a cookie value of the form "<b64(user|exp)>.<b64(hmac)>".
func signSession(user string, exp time.Time, secret string) string {
	payload := user + sessionSeparator + strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

// verifySession returns the user encoded in the cookie if the signature is
// valid and the cookie has not expired; otherwise returns "".
func verifySession(value, secret string) string {
	dot := strings.IndexByte(value, '.')
	if dot < 0 {
		return ""
	}
	payloadB64, sigB64 := value[:dot], value[dot+1:]
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return ""
	}
	gotSig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	wantSig := mac.Sum(nil)
	if subtle.ConstantTimeCompare(gotSig, wantSig) != 1 {
		return ""
	}
	sep := strings.IndexByte(string(payload), sessionSeparator[0])
	if sep < 0 {
		return ""
	}
	user := string(payload[:sep])
	expUnix, err := strconv.ParseInt(string(payload[sep+1:]), 10, 64)
	if err != nil {
		return ""
	}
	if time.Now().Unix() >= expUnix {
		return ""
	}
	return user
}

// safeRedirect returns a path that's safe to use in a Location header:
// it must start with "/" and not "//" (which would be protocol-relative).
func safeRedirect(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") {
		return "/"
	}
	return raw
}

// setSessionCookie writes a signed session cookie scoped to the current host.
func setSessionCookie(w http.ResponseWriter, r *http.Request, user, secret string) {
	exp := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    signSession(user, exp, secret),
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
}

// clearSessionCookie writes an expired cookie to log the user out.
func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
}

// stripSessionCookie removes our session cookie from the request's Cookie
// header so it isn't forwarded to the upstream service.
func stripSessionCookie(r *http.Request) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name == sessionCookie {
			continue
		}
		r.AddCookie(c)
	}
}

// renderLoginPage writes the embedded login form. errMsg is shown above
// the form if non-empty.
func renderLoginPage(w http.ResponseWriter, host, redirect, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	action := loginPath + "?redirect=" + safeRedirectURLValue(redirect)
	errBlock := ""
	if errMsg != "" {
		errBlock = `<div class="err">` + html.EscapeString(errMsg) + `</div>`
	}
	fmt.Fprintf(w, loginHTML, html.EscapeString(host), html.EscapeString(action), errBlock)
}

// safeRedirectURLValue is a tiny URL-query encoder for the single redirect
// param; we keep it inline to avoid pulling net/url just for this.
func safeRedirectURLValue(s string) string {
	r := safeRedirect(s)
	out := make([]byte, 0, len(r))
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c == '/' || c == '-' || c == '_' || c == '.' || c == '~' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'):
			out = append(out, c)
		default:
			out = append(out, '%', hexNibble(c>>4), hexNibble(c&0x0f))
		}
	}
	return string(out)
}

func hexNibble(b byte) byte {
	if b < 10 {
		return '0' + b
	}
	return 'a' + (b - 10)
}

// handleSessionAuth handles the login/logout paths and validates the
// session cookie. It returns true if the request has been handled (response
// already written) and the caller should stop. It returns false if the
// session is valid and the caller should proceed to forward to upstream.
func (s *Server) handleSessionAuth(w http.ResponseWriter, r *http.Request, route *tunnel.RouteInfo) bool {
	s.mu.RLock()
	secret := s.cfg.SessionSecret
	s.mu.RUnlock()

	switch r.URL.Path {
	case loginPath:
		s.handleLogin(w, r, route, secret)
		return true
	case logoutPath:
		clearSessionCookie(w, r)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return true
	}

	c, err := r.Cookie(sessionCookie)
	if err == nil && verifySession(c.Value, secret) != "" {
		stripSessionCookie(r)
		return false
	}

	// No or invalid session: send the browser to the login page.
	target := loginPath + "?redirect=" + safeRedirectURLValue(r.URL.RequestURI())
	http.Redirect(w, r, target, http.StatusSeeOther)
	return true
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request, route *tunnel.RouteInfo, secret string) {
	redirect := safeRedirect(r.URL.Query().Get("redirect"))

	if r.Method == http.MethodGet {
		renderLoginPage(w, r.Host, redirect, "")
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		renderLoginPage(w, r.Host, redirect, "Invalid form submission")
		return
	}
	user := r.PostFormValue("user")
	pass := r.PostFormValue("password")
	want := route.Auth.LookupUser(user)
	if want == "" || subtle.ConstantTimeCompare([]byte(pass), []byte(want)) != 1 {
		renderLoginPage(w, r.Host, redirect, "Invalid username or password")
		return
	}

	setSessionCookie(w, r, user, secret)
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

const loginHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Sign in &middot; %[1]s</title>
<style>
  *{box-sizing:border-box;margin:0;padding:0}
  body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0a0a0a;color:#e0e0e0;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:1rem}
  .card{background:#111;border:1px solid #222;border-radius:8px;padding:2rem;width:100%%;max-width:340px}
  h1{font-size:1.1rem;font-weight:600;margin-bottom:0.25rem;color:#fff}
  .host{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:0.8rem;color:#888;margin-bottom:1.5rem}
  label{display:block;font-size:0.75rem;color:#888;text-transform:uppercase;letter-spacing:0.05em;margin-bottom:0.3rem;margin-top:1rem}
  label:first-of-type{margin-top:0}
  input{width:100%%;background:#0a0a0a;border:1px solid #333;color:#e0e0e0;padding:0.6rem;border-radius:4px;font-size:0.9rem;font-family:inherit}
  input:focus{outline:none;border-color:#555}
  button{margin-top:1.5rem;width:100%%;background:#1e3a5f;border:none;color:#60a5fa;padding:0.65rem;border-radius:4px;cursor:pointer;font-size:0.9rem;font-weight:500}
  button:hover{background:#254b75}
  .err{margin-bottom:1rem;padding:0.5rem 0.75rem;background:#3b1f1f;border:1px solid #5a2a2a;border-radius:4px;color:#f87171;font-size:0.85rem}
</style>
</head>
<body>
  <form class="card" method="POST" action="%[2]s" autocomplete="on">
    <h1>Sign in</h1>
    <div class="host">%[1]s</div>
    %[3]s
    <label for="user">Username</label>
    <input id="user" name="user" autofocus autocomplete="username">
    <label for="password">Password</label>
    <input id="password" name="password" type="password" autocomplete="current-password">
    <button type="submit">Sign in</button>
  </form>
</body>
</html>`
