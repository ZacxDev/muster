package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// `nudge ls|next` — a HOST's chief-nudge notices, as tmux targets
// (docs/chief-nudges.md#jumping-to-a-notice).
//
// A live chief nudge is a NOTICE on the owner's Asks page: a pointer at a
// session that looks like it is waiting on them, with no answer to give. These
// two verbs are the host side of it — what a tmux key binding runs to jump to
// the oldest one waiting on THIS host:
//
//	tmux switch-client -t "$(muster nudge next --host "$MUSTER_MIRROR_HOST" | jq -r .target)"
//
// Both read GET /api/attention/summary — the same document a status bar polls,
// whose items are exactly the Asks page's — with the host's ordinary muster
// credential, a PERSONAL token (mst_…) or a bar-summary token: the summary
// is a user's own, so the shared hook token is refused (exit 3). No new route,
// no new credential: a notice is one more item of the summary, of kind
// "notice", carrying its host and its window's tmux target, resolved by the
// server NOW (a window renumbered since the nudge resolves to its new index).
//
// OUTPUT DISCIPLINE HOLDS: stdout is JSON and nothing else. `next` prints one
// notice object (its `target` is "<session>:<window index>"); when NONE is
// waiting it prints nothing on stdout and exits exitNoneWaiting (11), so a key
// binding can tell "nothing to jump to" from a failure. A notice whose window
// no mirrored window holds now has no target: `ls` lists it, `next` skips it.

const (
	attentionSummaryRoute = "/api/attention/summary"
	// noticeKind is a notice's item kind on the summary (internal/api
	// attentionKindNotice).
	noticeKind = "notice"
	// envMirrorHost is the host name a muster-mirror reports, the default for
	// --host (cmd/muster-mirror's MUSTER_MIRROR_HOST).
	envMirrorHost = "MUSTER_MIRROR_HOST"
)

// nudgeNotice is one notice as these verbs print it: the summary item's fields
// a jump needs.
type nudgeNotice struct {
	Key     string `json:"key"`
	Host    string `json:"host"`
	Target  string `json:"target"`
	Title   string `json:"title"`
	Why     string `json:"why"`
	Since   int64  `json:"since"`
	AgeSecs int64  `json:"age_secs"`
	URL     string `json:"url,omitempty"`
}

// summaryDoc is the part of the summary these verbs read.
type summaryDoc struct {
	Partial bool `json:"partial"`
	Items   []struct {
		nudgeNotice
		Kind string `json:"kind"`
	} `json:"items"`
}

func newNudgeCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nudge",
		Short: "This host's chief-nudge notices, as tmux targets (for a jump key)",
	}
	cmd.AddCommand(newNudgeLsCmd(a), newNudgeNextCmd(a))
	return cmd
}

// hostFlag adds --host, defaulting to MUSTER_MIRROR_HOST.
func hostFlag(cmd *cobra.Command, host *string) {
	cmd.Flags().StringVar(host, "host", "", "the mirror host's name, as its muster-mirror reports it (default: $"+envMirrorHost+")")
}

func (a *app) resolveHost(host string) (string, error) {
	h := strings.TrimSpace(host)
	if h == "" && a.getenv != nil {
		h = strings.TrimSpace(a.getenv(envMirrorHost))
	}
	if h == "" {
		return "", failf(exitUsage, "no host: pass --host or set %s", envMirrorHost)
	}
	return h, nil
}

// hostNotices is host's open notices, OLDEST FIRST (by since, then key), and
// whether the summary said it was partial.
func (a *app) hostNotices(cmd *cobra.Command, host string) ([]nudgeNotice, bool, error) {
	raw, err := a.call(cmd.Context(), request{method: http.MethodGet, path: attentionSummaryRoute, cred: credHook})
	if err != nil {
		return nil, false, err
	}
	var doc summaryDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false, wrapf(exitNonJSON, err, "the attention summary was not the JSON expected")
	}
	out := []nudgeNotice{}
	for _, it := range doc.Items {
		if it.Kind == noticeKind && it.Host == host {
			out = append(out, it.nudgeNotice)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Since != out[j].Since {
			return out[i].Since < out[j].Since
		}
		return out[i].Key < out[j].Key
	})
	return out, doc.Partial, nil
}

func newNudgeLsCmd(a *app) *cobra.Command {
	var host string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "GET /api/attention/summary — this host's open chief-nudge notices, oldest first",
		Long: `List the chief-nudge notices waiting on you whose session is on this host,
oldest first, as one JSON document: {"host", "partial", "notices": [...]}. Each
notice's "target" is "<tmux session>:<window index>", or "" when no mirrored
window holds its session now.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := a.resolveHost(host)
			if err != nil {
				return err
			}
			list, partial, err := a.hostNotices(cmd, h)
			if err != nil {
				return err
			}
			return a.emitValue(map[string]any{"host": h, "partial": partial, "notices": list})
		},
	}
	hostFlag(cmd, &host)
	return cmd
}

func newNudgeNextCmd(a *app) *cobra.Command {
	var host string
	cmd := &cobra.Command{
		Use:   "next",
		Short: "The oldest chief-nudge notice on this host that has a tmux target; exit 11 when none is waiting",
		Long: `Print the OLDEST chief-nudge notice waiting on you whose session is on this
host and has a tmux target now, as one JSON object; its "target" is
"<tmux session>:<window index>":

  tmux switch-client -t "$(muster nudge next --host <host> | jq -r .target)"

When none is waiting it prints nothing on stdout and exits 11. When the queue
could only be partly read and none was found, it exits 1 rather than claim
none is waiting.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := a.resolveHost(host)
			if err != nil {
				return err
			}
			list, partial, err := a.hostNotices(cmd, h)
			if err != nil {
				return err
			}
			for _, n := range list {
				if n.Target != "" {
					return a.emitValue(n)
				}
			}
			if partial {
				return failf(exitServerError, "no notice found on %s, but muster could only read part of the queue, so one may be missing", h)
			}
			return failf(exitNoneWaiting, "%s", noneWaitingMessage(h, len(list)))
		},
	}
	hostFlag(cmd, &host)
	return cmd
}

// noneWaitingMessage says why next found nothing to jump to.
func noneWaitingMessage(host string, untargeted int) string {
	if untargeted > 0 {
		return fmt.Sprintf("no chief-nudge notice to jump to on %s (%d waiting, but no mirrored window holds its session now)", host, untargeted)
	}
	return "no chief-nudge notice is waiting on " + host
}
