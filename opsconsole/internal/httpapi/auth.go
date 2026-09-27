package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"opsconsole/internal/session"
)

// ParseOperators parses OC_OPERATORS, formatted
// "username:bcrypt_hash:display_name,...". A real password hash is the
// right tool here (unlike C6's customers.GenerateAPIKey, which
// deliberately avoided one for a machine-held 256-bit key) -- humans
// type a password into a login form, they don't paste a service token.
func ParseOperators(raw string) ([]Operator, error) {
	var out []Operator
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ":", 3)
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return nil, &operatorParseError{entry}
		}
		out = append(out, Operator{Username: parts[0], BcryptHash: parts[1], DisplayName: parts[2]})
	}
	if len(out) == 0 {
		return nil, &operatorParseError{raw}
	}
	return out, nil
}

type operatorParseError struct{ entry string }

func (e *operatorParseError) Error() string {
	return "httpapi: malformed OC_OPERATORS entry " + e.entry + ", want username:bcrypt_hash:display_name"
}

type sessionContextKey struct{}

func sessionFromContext(ctx context.Context) (session.Session, bool) {
	s, ok := ctx.Value(sessionContextKey{}).(session.Session)
	return s, ok
}

const sessionCookieName = "opsconsole_session"

// requireSession is the auth middleware every route but /login,
// /healthz, and /metrics sits behind.
func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		sess, err := s.Sessions.Verify(cookie.Value)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		ctx := context.WithValue(r.Context(), sessionContextKey{}, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

const loginContent = `
<h1>Ops Console</h1>
{{ if .Error }}<div class="flash flash-error">{{ .Error }}</div>{{ end }}
<form method="post" action="/login">
  <label>Username</label>
  <input type="text" name="username" required autofocus>
  <label>Password</label>
  <input type="password" name="password" required>
  <label>S1 approver token (optional -- only needed to approve/reject signing requests)</label>
  <input type="password" name="s1_approver_token">
  <p><input type="submit" value="Log in"></p>
</form>
`

type loginPageData struct {
	basePageData
}

func (s *Server) loginPage(errMsg string) loginPageData {
	d := loginPageData{basePageData: basePageData{Title: "Log in", EnvLabel: s.EnvLabel, EnvLive: strings.EqualFold(s.EnvLabel, "live")}}
	d.Err = errMsg
	return d
}

func (s *Server) getLogin(w http.ResponseWriter, r *http.Request) {
	s.Templates.Render(w, "login", s.loginPage(""))
}

func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.Templates.Render(w, "login", s.loginPage("malformed form submission"))
		return
	}
	username := r.FormValue("username")
	password := r.FormValue("password")
	approverToken := r.FormValue("s1_approver_token")

	op, ok := s.findOperator(username)
	if !ok || bcrypt.CompareHashAndPassword([]byte(op.BcryptHash), []byte(password)) != nil {
		s.Templates.Render(w, "login", s.loginPage("invalid username or password"))
		return
	}

	now := time.Now()
	sess := session.Session{
		Username: op.Username, DisplayName: op.DisplayName, S1ApproverToken: approverToken,
		IssuedAt: now, ExpiresAt: now.Add(12 * time.Hour),
	}
	cookieValue, err := s.Sessions.Sign(sess)
	if err != nil {
		http.Error(w, "opsconsole: signing session: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: cookieValue, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Expires: sess.ExpiresAt,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) postLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusFound)
}
