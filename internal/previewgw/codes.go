package previewgw

import (
	"net/http"

	"github.com/ZacxDev/muster/internal/preview/wire"
)

// The codes a page of the gateway's own on a p- host can name. 🔴 EACH IS
// ^[a-z_]{1,32}$ AND ITS PAGE TEXT IS FIXED HERE: nothing an error, a host or
// an app wrote reaches the viewer through the gateway's pages.
const (
	codeNotAnswering    = "not_answering"
	codeUpstreamTimeout = "upstream_timeout"
	codePreviewClosed   = "preview_closed"
	codeSessionExpired  = "session_expired"
	codeTooManyCookies  = "too_many_cookies"
	codeBadUpgrade      = "bad_upgrade"
	codeEntryUsed       = "entry_used"
	codeFrameBlocked    = "frame_blocked"
	codeTooManyConns    = "too_many_connections"
	codeBodyTooLarge    = "body_too_large"
)

// ErrorCodeHeader names the code on every page the gateway answers for a p-
// request itself.
const ErrorCodeHeader = "X-Muster-Preview-Error"

var codePages = map[string][2]string{
	codeHostOffline:                    {"This host is offline", "Its previews come back when it reconnects."},
	codeNotAnswering:                   {"The app is not answering", "Nothing answered on the preview's port. The preview stays open: start the app and reload."},
	codeUpstreamTimeout:                {"The app did not answer in time", "Reload to try again."},
	string(wire.ReasonMaxConns):        {"Too many connections to this host", "Wait a moment, or close other tabs of its previews."},
	string(wire.ReasonPortNotAllowed):  {"This port is not allowed", "The host's preview allowlist no longer lists it."},
	string(wire.ReasonForeignListener): {"Another user's server", "The port is held by another user on the host."},
	codePreviewClosed:                  {"This preview is closed", "Open it again from muster."},
	codeSessionExpired:                 {"Session expired", "Open the preview again from muster."},
	codeCrossSite:                      {"Open this preview from muster", "This request came from another site or another preview, and was refused."},
	codeTooManyCookies:                 {"Too many cookies", "This browser sent more cookies than a preview accepts. They have been cleared: reload."},
	codeBadUpgrade:                     {"Not supported", "A preview relays WebSocket upgrades only."},
	codeEntryUsed:                      {"This link has been used or has expired", "Open the preview again from its session page in muster."},
	codeFrameBlocked:                   {"This browser blocks embedded previews", "Open it in a new tab from the session page in muster."},
	codeTooManyConns:                   {"Too many connections to this preview", "Wait for its other requests to finish, or close other tabs of it."},
	codeBodyTooLarge:                   {"This upload is too large", "A preview accepts request bodies up to the gateway's limit."},
}

// renderCode writes the gateway's own page for code: fixed text, the code
// itself, and ErrorCodeHeader. An unknown code is not_answering's page. Each
// is counted in muster_preview_refusals_total{reason=code}.
func (g *Gateway) renderCode(w http.ResponseWriter, status int, code string) {
	txt, ok := codePages[code]
	if !ok {
		code, txt = codeNotAnswering, codePages[codeNotAnswering]
	}
	g.m.refused(code)
	w.Header().Set(ErrorCodeHeader, code)
	g.renderNotice(w, status, txt[0], txt[1]+" ("+code+")")
}
