// Package noredirect is THE rule for an outbound HTTP client that carries a
// credential: it never follows a redirect.
//
// Go's net/http re-sends a request's Authorization header on a redirect to
// the same hostname (any port, any scheme) and to its subdomains, and every
// other header (a WebSocket subprotocol carrying a ticket, say) to ANY host.
// So a server that answers with a 3xx — a misconfigured proxy, a login page,
// a compromised upstream — would be handed the credential at the Location it
// names. A client built here refuses the redirect before that second request
// is made, and says where it was being sent.
//
// One rule, one place: every credential-bearing client the mirror binary
// makes is built with Client (a structural test in cmd/muster-mirror fails a
// new http.Client literal that is not).
package noredirect

import (
	"fmt"
	"net/http"
)

// Error is a redirect a client built by Client refused to follow.
type Error struct {
	// Host is the refused Location's host (and port, when it names one).
	Host string
}

func (e *Error) Error() string {
	return fmt.Sprintf("refused to follow a redirect to %q: a redirect would re-send this request's credential", e.Host)
}

// Check is an http.Client CheckRedirect that refuses every redirect with an
// *Error naming the Location's host. net/http calls it BEFORE the redirected
// request is sent, so the Location never receives a request.
func Check(req *http.Request, _ []*http.Request) error {
	return &Error{Host: req.URL.Host}
}

// Client returns a copy of c that refuses every redirect (Check); a nil c is
// a new zero client. The copy shares c's Transport, Jar and Timeout; c itself
// is not modified.
func Client(c *http.Client) *http.Client {
	var out http.Client
	if c != nil {
		out = *c
	}
	out.CheckRedirect = Check
	return &out
}
