package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/auth"
	"github.com/ZacxDev/muster/internal/github"
	"github.com/ZacxDev/muster/internal/ui"
)

const oauthStateCookie = "muster_gh_state"

// registerGitHubRoutes wires the Repos tab + GitHub OAuth flow when a GitHub
// connection store is set.
// 🔴 NO EARLY RETURN ON A nil DEPENDENCY — see the package doc in server.go.
// Every handler below answers its own "nothing to serve from" case, so the
// recorded route set is a function of the CODE and never of the fixture.
func (s *Server) registerGitHubRoutes(mux Mux) {
	mux.HandleFunc("GET /ui/repos", s.requireSession(s.handleReposContent))
	mux.HandleFunc("GET /github/connect", s.requireSession(s.handleGitHubConnect))
	mux.HandleFunc("GET /github/callback", s.requireSession(s.handleGitHubCallback))
	mux.HandleFunc("POST /github/disconnect", s.requireSession(s.handleGitHubDisconnect))
}

// handleReposContent serves the /ui/repos partial: not-configured / connect /
// connected-with-repo-list depending on state.
func (s *Server) handleReposContent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Check for a stored connection FIRST — it may come from the static-token
	// path (MUSTER_GITHUB_TOKEN auto-connect) even when no OAuth App is set.
	conn, ok, err := s.ext.GitHub.Get(r.Context())
	if err != nil {
		s.logger.Printf("github: get connection: %v", err)
		http.Error(w, "could not load GitHub connection", http.StatusInternalServerError)
		return
	}
	if !ok {
		// Not connected: offer the OAuth flow if it's configured, otherwise
		// explain how to connect.
		if s.ext.GitHubOAuth.Configured() {
			_ = ui.ReposConnect().Render(w)
		} else {
			_ = ui.ReposNotConfigured().Render(w)
		}
		return
	}
	repos, err := github.ListRepos(r.Context(), conn.Token)
	if err != nil {
		s.logger.Printf("github: list repos: %v", err)
		_ = ui.ReposConnected(conn.Login, nil).Render(w)
		return
	}
	views := make([]ui.RepoView, 0, len(repos))
	for _, rp := range repos {
		views = append(views, ui.RepoView{FullName: rp.FullName, Private: rp.Private, HTMLURL: rp.HTMLURL})
	}
	_ = ui.ReposConnected(conn.Login, views).Render(w)
}

// handleGitHubConnect starts the OAuth web flow: set a state cookie and redirect
// to GitHub's authorize page.
func (s *Server) handleGitHubConnect(w http.ResponseWriter, r *http.Request) {
	state, err := auth.RandomToken(16)
	if err != nil {
		http.Error(w, "could not start oauth", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookie,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.auth.SecureCookies,
		SameSite: http.SameSiteLaxMode,
		Expires:  s.now().Add(10 * time.Minute),
	})
	http.Redirect(w, r, github.AuthorizeURL(s.ext.GitHubOAuth.ClientID, s.callbackURL(r), state), http.StatusFound)
}

// handleGitHubCallback completes the flow: verify state, exchange the code,
// fetch the login, and persist the (encrypted) token.
func (s *Server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	wantState, err := r.Cookie(oauthStateCookie)
	if err != nil || wantState.Value == "" || r.URL.Query().Get("state") != wantState.Value {
		http.Error(w, "invalid oauth state", http.StatusBadRequest)
		return
	}
	// Clear the state cookie.
	http.SetCookie(w, &http.Cookie{Name: oauthStateCookie, Value: "", Path: "/", MaxAge: -1})

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	token, scopes, err := github.ExchangeCode(r.Context(), s.ext.GitHubOAuth.ClientID, s.ext.GitHubOAuth.ClientSecret, code, s.callbackURL(r))
	if err != nil {
		s.logger.Printf("github: exchange code: %v", err)
		http.Error(w, "github authorization failed", http.StatusBadGateway)
		return
	}
	login, err := github.FetchLogin(r.Context(), token)
	if err != nil {
		s.logger.Printf("github: fetch login: %v", err)
		http.Error(w, "could not read github account", http.StatusBadGateway)
		return
	}
	if err := s.ext.GitHub.Save(r.Context(), login, scopes, token); err != nil {
		s.logger.Printf("github: save connection: %v", err)
		http.Error(w, "could not save github connection", http.StatusInternalServerError)
		return
	}
	s.logger.Printf("github: connected account %q (scopes=%q)", login, scopes)
	// Back to the app, Repos tab.
	http.Redirect(w, r, "/repos", http.StatusFound)
}

// handleGitHubDisconnect clears the stored connection and re-renders the panel.
func (s *Server) handleGitHubDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.ext.GitHub.Clear(r.Context()); err != nil {
		s.logger.Printf("github: clear connection: %v", err)
		http.Error(w, "could not disconnect", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = ui.ReposConnect().Render(w)
}

// callbackURL builds the OAuth redirect URI. It prefers the configured public
// base URL; otherwise it derives the origin from the inbound request.
func (s *Server) callbackURL(r *http.Request) string {
	base := strings.TrimRight(s.ext.GitHubOAuth.BaseURL, "/")
	if base == "" {
		scheme := "https"
		if r.TLS == nil && !s.auth.SecureCookies {
			scheme = "http"
		}
		base = scheme + "://" + r.Host
	}
	return base + "/github/callback"
}
