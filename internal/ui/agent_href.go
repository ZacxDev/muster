package ui

import "strconv"

// AgentSessionHref is THE in-app link to an agent session's page:
// /agents/{name}?session={id}, the exact URL a bare GET /agents/{name} answers
// with its 303 (api.Server.handleAgentDetail builds its Location with this same
// function, so the two cannot disagree).
//
// 🔴 EVERY IN-APP LINK TO AN AGENT'S PAGE GOES THROUGH HERE, AND IT NAMES THE
// SESSION WHENEVER THE CALLER KNOWS IT. The bare URL costs one extra round trip
// on every open: the browser fetches it, gets the 303, and only then fetches the
// page. Measured with an emulated phone (350 ms RTT, 4x CPU), the first bubble
// landed at 828/1203 ms through the bare link and 476/488 ms through this one.
// api.TestEveryAgentPageLinkGoesThroughAgentSessionHref ledgers every caller and
// fails when a link to the page is built anywhere else.
//
// sessionID <= 0 (the caller has no session: the agent has none yet, or the
// read failed) gives the bare page, which still redirects — and creates the
// session — exactly as before. The bare URL itself keeps redirecting for
// bookmarks and links from outside the app.
func AgentSessionHref(name string, sessionID int64) string {
	if sessionID > 0 {
		return AgentPath(name) + "?session=" + strconv.FormatInt(sessionID, 10)
	}
	return AgentPath(name)
}
