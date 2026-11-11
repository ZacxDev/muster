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

// ErrUsageUnsupported is an agent runtime without GET /v1/usage: a ccd image
// older than the route, or a runtime that is not ccd.
var ErrUsageUnsupported = errors.New("agentgateway: the agent's runtime does not serve its usage (GET /v1/usage); its image predates it")

// usageTimeout bounds one usage read: ccd reads every transcript touched in
// the window, which can be tens of megabytes.
const usageTimeout = 30 * time.Second

// maxUsageBody bounds one usage answer: at most 49 hours of buckets per
// session, a few hundred bytes each.
const maxUsageBody = 8 << 20

// Usage reads agent a's Claude usage per session and UTC hour since since
// from its runtime (ccd's GET /v1/usage), which truncates since down to the
// hour (cctranscript.UsagePage). It is the officer usage collector's source
// (internal/officers/usage). The bearer is the turn bearer, as Transcript's;
// it never appears in an error.
func (g *Gateway) Usage(ctx context.Context, a agents.Agent, since time.Time) (cctranscript.UsagePage, error) {
	t, err := g.Resolve(ctx, a)
	if err != nil {
		return cctranscript.UsagePage{}, err
	}
	q := url.Values{}
	q.Set("since", strconv.FormatInt(since.Unix(), 10))
	ctx, cancel := context.WithTimeout(ctx, usageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(t.ep.URL(), "/")+"/v1/usage?"+q.Encode(), nil)
	if err != nil {
		return cctranscript.UsagePage{}, fmt.Errorf("agentgateway: usage request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.bearer)
	resp, err := g.client.Do(req)
	if err != nil {
		return cctranscript.UsagePage{}, fmt.Errorf("agentgateway: read agent %q's usage: %w", a.Name, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return cctranscript.UsagePage{}, ErrUsageUnsupported
	case resp.StatusCode != http.StatusOK:
		return cctranscript.UsagePage{}, fmt.Errorf("agentgateway: agent %q's usage: its runtime answered %d", a.Name, resp.StatusCode)
	}
	var p cctranscript.UsagePage
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUsageBody)).Decode(&p); err != nil {
		return cctranscript.UsagePage{}, fmt.Errorf("agentgateway: agent %q's usage is not a usage page: %v", a.Name, err)
	}
	return p, nil
}
