package agentgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/cctranscript"
)

// ErrTranscriptUnsupported is an agent runtime without GET /v1/transcript: a
// ccd image older than the route, or a runtime that is not ccd.
var ErrTranscriptUnsupported = errors.New("agentgateway: the agent's runtime does not serve its transcript (GET /v1/transcript); its image predates it")

// transcriptTimeout bounds one transcript read.
const transcriptTimeout = 10 * time.Second

// maxTranscriptBody bounds one transcript page's body: ccd reads at most
// 1 MiB of transcript per page, and an event's fields are capped
// (cctranscript), so this is a backstop.
const maxTranscriptBody = 16 << 20

// Transcript reads agent a's session transcript from its runtime (ccd's GET
// /v1/transcript): the complete lines after offset after in session (after -1:
// the tail, its last backfill bytes). It is the officer transcript
// forwarder's source (internal/officers/transcript). The bearer is the turn
// bearer, the same credential a turn carries; it never appears in an error.
func (g *Gateway) Transcript(ctx context.Context, a agents.Agent, session string, after int64, backfill int) (cctranscript.Page, error) {
	t, err := g.Resolve(ctx, a)
	if err != nil {
		return cctranscript.Page{}, err
	}
	q := url.Values{}
	q.Set("after", strconv.FormatInt(after, 10))
	if backfill > 0 {
		q.Set("backfill", strconv.Itoa(backfill))
	}
	if session != "" {
		q.Set("session", session)
	}
	ctx, cancel := context.WithTimeout(ctx, transcriptTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(t.ep.URL(), "/")+"/v1/transcript?"+q.Encode(), nil)
	if err != nil {
		return cctranscript.Page{}, fmt.Errorf("agentgateway: transcript request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.bearer)
	resp, err := g.client.Do(req)
	if err != nil {
		return cctranscript.Page{}, fmt.Errorf("agentgateway: read agent %q's transcript: %w", a.Name, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return cctranscript.Page{}, ErrTranscriptUnsupported
	case resp.StatusCode != http.StatusOK:
		return cctranscript.Page{}, fmt.Errorf("agentgateway: agent %q's transcript: its runtime answered %d", a.Name, resp.StatusCode)
	}
	var p cctranscript.Page
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTranscriptBody)).Decode(&p); err != nil {
		return cctranscript.Page{}, fmt.Errorf("agentgateway: agent %q's transcript is not a page: %v", a.Name, err)
	}
	return p, nil
}
