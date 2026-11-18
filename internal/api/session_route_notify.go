package api

import (
	"context"
	"strconv"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
)

// THE OWNER IS TOLD WHEN THE CHIEF ROUTES ON ITS OWN. The chief hands a plugin
// message — feedback, a heads-up nobody can answer — or a task to the live
// session it judged is already on it, and nothing in that asks the owner. So
// the owner gets ONE short notification per route, naming the session and what
// it got: they can see what went where, and step in when the chief chose wrong.
//
// 🔴 DETERMINISTIC, IN THE SERVER, ON THE ROUTE'S CREATION. route_to_session
// calls it once the route is made and its paste queued (routeItem returned no
// refusal), never on a refusal, and never for a REQUEST route (the owner's own
// ask: they know where it went, and the chief tells them). It is not left to
// the chief's prose, which can forget.
//
// 🔴 INFORMATION, NOT AN ASK. Type "notify" (fire and forget: no action, no
// answer), on push.EventPlugin — muted with that toggle on /account, the one
// existing switch for plugin traffic; there is no other.
//
// ⚠ THE BODY CARRIES OUTSIDE TEXT, CUT TO ONE LINE: a message's subject (its
// sender wrote it) or a task's title. It is shown, never acted on; the title
// is muster's words and the session's place (mirror.Where).

// chiefRoutedPushType is the notification's payload type: a plain,
// informational notification (web/static/sw.js).
const chiefRoutedPushType = "notify"

// chiefRoutedHereTitle is a routed push's title after its window's
// codename@host, which says where (codenameLed).
const chiefRoutedHereTitle = "Chief routed it here"

// chiefRoutedBodyMax bounds the notification's body.
const chiefRoutedBodyMax = 160

// chiefRoutedTag is route r's notification tag: one per route.
func chiefRoutedTag(r plugins.Route) string { return "route-" + r.RouteID }

// notifyChiefRouted tells owner that the chief routed item it — a message or
// a task; route_to_session never calls it for a request route — to session t
// as route r: once, best-effort (goNotify neither blocks nor fails the route).
func (s *Server) notifyChiefRouted(ctx context.Context, owner int64, r plugins.Route, t routeTarget, it routedItem) {
	where := t.window.Where()
	if where == "" {
		where = t.label
	}
	title := "Chief routed it to " + where
	var body, url string
	switch {
	case it.isTask():
		body = "Task #" + strconv.FormatInt(it.task.ID, 10) + ": " + taskLabelPlain(it.task)
		url = canonicalTaskPath(it.task.ID)
	default:
		label := ""
		if s.ext.Plugins != nil {
			if p, err := s.ext.Plugins.Plugin(ctx, it.msg.PluginID); err == nil {
				label = mirror.OneLine(p.Label, plugins.MaxLabelRunes)
			}
		}
		subject := mirror.OneLine(it.msg.Subject, chiefRoutedBodyMax)
		if subject == "" {
			subject = "(no subject)"
		}
		body = subject
		if label != "" {
			body = label + ": " + subject
		}
		if r.OneWay {
			body += " (no reply possible: a heads-up)"
		}
	}
	if url == "" && t.window.Host != "" {
		url = mirrorRef(t.window).ViewPath()
	}
	n := RouterNotification{Type: chiefRoutedPushType, ID: r.RouteID, Title: title, Body: truncate(body, chiefRoutedBodyMax),
		Tag: chiefRoutedTag(r), TitleKeep: where}
	// "Start notification titles with the tmux name": the target window's
	// codename@host first (a window the route resolved over the hosts NOW).
	if t.window.Host != "" {
		n.CodenameTitle, n.CodenameKeep = codenameLed(chiefRoutedHereTitle, t.window.Place())
	}
	if url != "" {
		n.Data = map[string]string{"url": url}
	}
	s.logger.Printf("routes: %s: the owner is told the chief routed it to %s", r.RouteID, where)
	s.goNotify(owner, push.EventPlugin, n, nil)
}
