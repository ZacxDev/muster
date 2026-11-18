package plugins

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/fence"
)

// THE ROUTED BLOCK'S VOCABULARY: what muster pastes into a mirrored session
// when it routes something there (internal/api's renderRoutedBlock and its
// callers), and the one reading of it back (ParseRoutedBlock), for DRAWING the
// paste to a person — as a "From <who>" card, never as their own message.
//
// The words live here, not in internal/api, so the writer and the reader share
// one spelling. They are the agent's text: changing one changes every routed
// paste.
const (
	// RoutedMarker is the routed block's fence marker.
	RoutedMarker = "routed-message"
	// RoutedPrefacePrefix opens every routed block's preface (a message's, a
	// task's and a request's).
	RoutedPrefacePrefix = "Routed to you by "
	// Who a routed block says routed it: muster's words.
	RoutedByChief   = "your owner's chief"
	RoutedByOwner   = "your owner, from one of their mirrored sessions"
	RoutedByThread  = "muster, because it is a reply on a mail thread this session started"
	RoutedByRule    = "muster, because it matches a routing rule your owner approved for this session"
	RoutedByRedraft = "muster, because the plugin refused the reply you wrote to it"
	RoutedByPR      = "muster, because this session created or pushed the pull request the message is about"
)

// RoutedVia is who a routed block names as its router, in a person's terms.
type RoutedVia string

const (
	RoutedViaChief   RoutedVia = "chief"
	RoutedViaOwner   RoutedVia = "owner"
	RoutedViaThread  RoutedVia = "thread"
	RoutedViaRule    RoutedVia = "rule"
	RoutedViaRedraft RoutedVia = "redraft"
	RoutedViaPR      RoutedVia = "pr"
)

// The kinds of item a routed block carries.
const (
	RoutedKindMessage = "message"
	RoutedKindTask    = "task"
	RoutedKindRequest = "request"
)

// RoutedBlock is a routed paste read back (ParseRoutedBlock): who routed it,
// what it carries and its text, unfolded (fence.Unfold) for showing.
type RoutedBlock struct {
	Via     RoutedVia
	RouteID string
	Kind    string // RoutedKindMessage, RoutedKindTask or RoutedKindRequest
	// Source is the plugin a message came through (its label), "" otherwise.
	Source string
	TaskID int64
	// Subject is a message's subject or a task's title; "" for none.
	Subject string
	// Body is the item's text: a message's or task's body, or the chief's
	// relay of a request.
	Body string
	// OwnerTyped is, for a request, what the owner typed in the chief's chat
	// as the block quotes it (cut to the block's bound); "" otherwise.
	OwnerTyped string
}

var (
	routedHeadRE   = regexp.MustCompile(`route_id (rt_[0-9a-f]+)`)
	routedSourceRE = regexp.MustCompile(`^, plugin message_id [0-9]+ from (.*?)\. `)
	routedTaskRE   = regexp.MustCompile(`^, task #([0-9]+)\. `)
	routedRequest  = RoutedPrefacePrefix + RoutedByChief + ", relaying a request your owner typed in its chat: route_id "
)

// ParseRoutedBlock reads text back as a routed block muster pasted: ok only
// when it is exactly one fenced block (fence.Parse) with the routed marker, a
// preface opening with RoutedPrefacePrefix and one of the RoutedBy words, a
// route id, and a body line. The fence is what makes this structural: what an
// outside sender wrote is behind its content prefix and cannot open a block or
// its preface.
func ParseRoutedBlock(text string) (RoutedBlock, bool) {
	b, _, ok := fence.Parse(text)
	if !ok || b.Marker != RoutedMarker || !strings.HasPrefix(b.Preface, RoutedPrefacePrefix) {
		return RoutedBlock{}, false
	}
	var out RoutedBlock
	rest := strings.TrimPrefix(b.Preface, RoutedPrefacePrefix)
	switch {
	case strings.HasPrefix(b.Preface, routedRequest):
		out.Via, out.Kind = RoutedViaChief, RoutedKindRequest
	default:
		for _, v := range []struct {
			by  string
			via RoutedVia
		}{{RoutedByChief, RoutedViaChief}, {RoutedByOwner, RoutedViaOwner}, {RoutedByThread, RoutedViaThread},
			{RoutedByRule, RoutedViaRule}, {RoutedByRedraft, RoutedViaRedraft}, {RoutedByPR, RoutedViaPR}} {
			if strings.HasPrefix(rest, v.by+": route_id ") {
				out.Via = v.via
				rest = strings.TrimPrefix(rest, v.by+": ")
				break
			}
		}
		if out.Via == "" {
			return RoutedBlock{}, false
		}
	}
	m := routedHeadRE.FindStringSubmatch(b.Preface)
	if m == nil {
		return RoutedBlock{}, false
	}
	out.RouteID = m[1]
	if out.Kind == "" {
		after := strings.TrimPrefix(rest, "route_id "+out.RouteID)
		switch {
		case routedTaskRE.MatchString(after):
			out.Kind = RoutedKindTask
			out.TaskID, _ = strconv.ParseInt(routedTaskRE.FindStringSubmatch(after)[1], 10, 64)
		case routedSourceRE.MatchString(after):
			out.Kind = RoutedKindMessage
			out.Source = routedSourceRE.FindStringSubmatch(after)[1]
		default:
			return RoutedBlock{}, false
		}
	}
	body := false
	for _, l := range b.Lines {
		switch {
		case strings.HasPrefix(l, "  subject: ") && out.Kind == RoutedKindMessage:
			out.Subject = fence.Unfold(strings.TrimPrefix(l, "  subject: "))
		case strings.HasPrefix(l, "  title: ") && out.Kind == RoutedKindTask:
			out.Subject = fence.Unfold(strings.TrimPrefix(l, "  title: "))
		case strings.HasPrefix(l, "  your owner typed: ") && out.Kind == RoutedKindRequest:
			out.OwnerTyped = fence.Unfold(strings.TrimPrefix(l, "  your owner typed: "))
		case strings.HasPrefix(l, "  body: "):
			out.Body, body = fence.Unfold(strings.TrimPrefix(l, "  body: ")), true
		}
	}
	if !body {
		return RoutedBlock{}, false
	}
	return out, true
}

// RoutedPasteSHA256 is the hash a route records of the block it pasted
// (Route.PasteSHA256) and a view compares a prompt against: SHA-256 of text
// with CRLF folded and trailing white space trimmed (a transcript may keep or
// drop the paste's final newline), lower-case hex.
func RoutedPasteSHA256(text string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), " \t\n")))
	return hex.EncodeToString(sum[:])
}
