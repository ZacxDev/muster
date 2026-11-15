package ui

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/ZacxDev/muster/internal/textplain"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE PAGE CARD — a shareable page an agent published, drawn in the chat
// (docs/pages.md#in-the-chat).
//
// When an agent calls page_create, page_update or page_share_request, the
// chat draws a card under its tool row: the page's title, what happened (a new
// page, a new version, shared, waiting for you), an Open button and a Share
// button, and a ⋯ menu with Revoke link, New link and the page's own screen.
//
// 🔴 ONE RENDERER FOR EVERY PATH. A dispatched agent's stored transcript
// (chatLogInner), its live stream (agentChatScript fetches PageCardPath for
// the result it just streamed) and a mirrored session's view (MirrorTranscript)
// all draw PageCard.
//
// 🔴 THE CARD NEVER CARRIES A LINK'S ADDRESS. It is drawn from the tool's
// result, which outlives the link (a revoke, a New link): an address in it
// would be copied after it stopped working. Share asks the server each time
// (POST PageSharePath), which answers with the page's CURRENT reusable link,
// making one when it has none.
//
// Open goes to the PREVIEW (/pages/{id}/preview), not the page's settings
// screen: "bring it up" is the page itself, as a viewer sees it, one tap
// away; the settings screen (links, versions, responses) is in the ⋯ menu.

// The page tools a card is drawn for.
const (
	PageCreateTool       = "page_create"
	PageUpdateTool       = "page_update"
	PageShareRequestTool = "page_share_request"
)

// The card's kinds (data-page-card-kind).
const (
	PageCardPublished = "published"
	PageCardUpdated   = "updated"
	PageCardShared    = "shared"
	PageCardAsked     = "asked"
)

// The owner's page paths (internal/api pages.go serves them).
func pageBase(id int64) string { return "/pages/" + strconv.FormatInt(id, 10) }

// PagePreviewPath is page id's preview (what Open opens).
func PagePreviewPath(id int64) string { return pageBase(id) + "/preview" }

// PageDetailPath is page id's own screen.
func PageDetailPath(id int64) string { return pageBase(id) }

// PageSharePath answers the page's reusable link (POST).
func PageSharePath(id int64) string { return pageBase(id) + "/share" }

// PageShareRotatePath revokes the page's link and makes a new one (POST).
func PageShareRotatePath(id int64) string { return pageBase(id) + "/share/rotate" }

// PageShareRevokePath revokes the page's link (POST).
func PageShareRevokePath(id int64) string { return pageBase(id) + "/share/revoke" }

// PageCardPath serves one card (GET ?tool=<name>): the streamed chat's.
func PageCardPath(id int64) string { return pageBase(id) + "/card" }

// PagesPath is the owner's Pages screen.
const PagesPath = "/pages"

// pagesLink is the sidebar's Pages entry, drawn only when pages are served: a
// full navigation (a standalone document, as attentionLink's).
func pagesLink(feat Features, active, current bool) g.Node {
	if !feat.Pages {
		return nil
	}
	return A(ID("sidebar-pages"), Href(PagesPath), hx("hx-boost", "false"), g.If(current, g.Attr("aria-current", "page")),
		Class("press flex min-h-[44px] w-full items-center gap-3 rounded-lg px-3 text-left text-sm font-medium transition "+navEntryTone(active)),
		Span(Class("text-base"), g.Text("📄")), Span(g.Text("Pages")))
}

// PageCardView is one page card.
type PageCardView struct {
	ID      int64
	Title   string
	Version int
	// Kind is PageCardPublished, …Updated, …Shared or …Asked.
	Kind string
	// SharedBy names who made the live link (PageCardShared).
	SharedBy string
}

// pageToolBase is a tool name's page tool, bare (a dispatched agent's) or as
// an MCP client names it (mcp__<server>__page_create): "" for any other tool.
func pageToolBase(name string) string {
	for _, b := range []string{PageCreateTool, PageUpdateTool, PageShareRequestTool} {
		if name == b || (strings.HasPrefix(name, "mcp__") && strings.HasSuffix(name, "__"+b)) {
			return b
		}
	}
	return ""
}

// IsPageTool reports whether name is a tool a page card is drawn for.
func IsPageTool(name string) bool { return pageToolBase(name) != "" }

// PageCardKind is the card kind of a page tool's result: tool is the tool's
// base name, status the share request's ("live" or "pending").
func PageCardKind(tool, status string) string {
	switch tool {
	case PageUpdateTool:
		return PageCardUpdated
	case PageShareRequestTool:
		if status == "live" {
			return PageCardShared
		}
		return PageCardAsked
	}
	return PageCardPublished
}

// ReadPageToolResult reads a page tool's result as its card: false for a tool
// that is not one, a refusal ({"error"}), or a result without a page.
func ReadPageToolResult(toolName, output string) (PageCardView, bool) {
	base := pageToolBase(toolName)
	if base == "" {
		return PageCardView{}, false
	}
	var r struct {
		PageID      int64  `json:"page_id"`
		Title       string `json:"title"`
		Version     int    `json:"version"`
		Status      string `json:"status"`
		RequestedBy string `json:"shared_by"`
		Error       string `json:"error"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(output)), &r) != nil || r.Error != "" || r.PageID <= 0 {
		return PageCardView{}, false
	}
	return PageCardView{ID: r.PageID, Title: r.Title, Version: r.Version, Kind: PageCardKind(base, r.Status), SharedBy: r.RequestedBy}, true
}

// PageCardID is the element id of the card drawn for tool call toolID.
func PageCardID(toolID string) string { return "page-card-" + toolRowID(toolID) }

const (
	pageButtonPrimary = "press inline-flex min-h-[44px] items-center justify-center gap-1.5 rounded-lg bg-accent px-3 text-sm font-semibold text-on-accent"
	pageButtonQuiet   = "press inline-flex min-h-[44px] items-center justify-center gap-1.5 rounded-lg bg-bg px-3 text-sm font-semibold text-fg ring-1 ring-inset ring-edge"
)

// pageCardTitle is a page title as muster draws it, never empty.
//
// 🔴 IT IS THE ONE PLACE A PAGE'S TITLE IS PLAINED ON ITS WAY TO THE CARD AND
// THE SHARE CONTROLS (a plaining helper in textplain's sink ledger), so every
// caller passes the RAW title: plaining it first would apply Title twice.
func pageCardTitle(raw string) string {
	if t := textplain.Title(raw); t != "" {
		return t
	}
	return "Untitled page"
}

// PageShareButton is THE Share button of page id: a form POST to
// PageSharePath, which the page script turns into the native share sheet or a
// copy (pageShareJS). shareURL, when not "", is the page's current address
// (the owner's own screens know it); "" asks the server on the tap.
func PageShareButton(id int64, title, shareURL string) g.Node {
	t := pageCardTitle(title)
	pid := strconv.FormatInt(id, 10)
	return Form(Method("POST"), Action(PageSharePath(id)), Class("contents"),
		Button(Type("submit"), g.Attr("data-page-share", ""), g.Attr("data-page-id", pid), g.Attr("data-share-title", t),
			g.If(shareURL != "", g.Attr("data-share-url", shareURL)),
			g.Attr("aria-label", "Share “"+t+"”"), Class(pageButtonPrimary),
			Span(g.Attr("aria-hidden", "true"), g.Text("↗")), Span(g.Text("Share"))))
}

// PageShareMenu is page id's ⋯ menu: Revoke link, New link (the old one stops
// working), and — off its own screen — the page's screen.
func PageShareMenu(id int64, title string, withDetail bool) g.Node {
	t := pageCardTitle(title)
	pid := strconv.FormatInt(id, 10)
	item := "press flex min-h-[44px] w-full items-center rounded-md px-3 text-left text-sm text-fg hover:bg-s2"
	action := func(act, path, label string) g.Node {
		return Form(Method("POST"), Action(path), g.Attr("data-page-share-action", act), g.Attr("data-page-id", pid),
			Button(Type("submit"), Class(item), g.Text(label)))
	}
	return Details(g.Attr("data-page-share-menu", ""), Class("relative"),
		Summary(g.Attr("aria-label", "More for “"+t+"”"), Class(pageButtonQuiet+" cursor-pointer list-none"), g.Text("⋯")),
		Div(Class("absolute right-0 z-20 mt-1 flex w-48 flex-col rounded-lg bg-s1 p-1 shadow-lg shadow-black/30 ring-1 ring-line"),
			action("revoke", PageShareRevokePath(id), "Revoke link"),
			action("rotate", PageShareRotatePath(id), "New link"),
			g.If(withDetail, A(Href(PageDetailPath(id)), hx("hx-boost", "false"), Class(item), g.Text("Page settings")))))
}

// PageCard draws one card; id is its element id ("" for none).
func PageCard(v PageCardView, id string) g.Node {
	t := pageCardTitle(v.Title)
	note := "New page — only you can see it until you share it."
	switch v.Kind {
	case PageCardUpdated:
		note = "Updated"
		if v.Version > 0 {
			note += " to version " + strconv.Itoa(v.Version)
		}
		note += " — its link shows this version."
	case PageCardShared:
		note = "Shared — anyone with its link can open it."
		if v.SharedBy != "" {
			note = "Shared by " + v.SharedBy + " — anyone with its link can open it. Revoke it from ⋯."
		}
	case PageCardAsked:
		note = "Asked to share it — waiting for you on Asks. Or share it yourself."
	}
	return Div(g.If(id != "", ID(id)),
		g.Attr("data-page-card", ""), g.Attr("data-page-id", strconv.FormatInt(v.ID, 10)), g.Attr("data-page-card-kind", v.Kind),
		Class("flex w-full max-w-[92%] min-w-0 flex-col gap-2 self-start rounded-xl bg-bg/50 px-3 py-2 ring-1 ring-inset ring-line"),
		Div(Class("flex min-w-0 items-center gap-2"),
			Span(Class("shrink-0"), g.Attr("aria-hidden", "true"), g.Text("📄")),
			Span(g.Attr("data-page-card-title", ""), Class("min-w-0 truncate text-sm font-semibold text-fg"), g.Text(t))),
		P(g.Attr("data-page-card-note", ""), Class("break-words text-xs text-muted"), g.Text(note)),
		Div(Class("flex flex-wrap items-center gap-2"),
			A(Href(PagePreviewPath(v.ID)), hx("hx-boost", "false"), g.Attr("data-page-open", ""), g.Attr("aria-label", "Open “"+t+"”"),
				Class(pageButtonQuiet), g.Text("Open")),
			PageShareButton(v.ID, v.Title, ""),
			PageShareMenu(v.ID, v.Title, true)),
	)
}

// pageShareJS is the Share button's and the ⋯ menu's behaviour, in appScript
// (every document with the nav: the chat, the mirrored view, the Pages
// screens). Delegated from document, so a card the stream inserts later works
// too; installed once per window however often appScript runs.
//
// Share: the address (the button's own, or POST PageSharePath's answer) goes
// to navigator.share when the browser has one — a cancelled sheet
// (AbortError) is silence — and otherwise, or when the sheet fails, to the
// clipboard with a "Link copied" toast. A copy the browser refuses shows the
// address in the toast instead, and the button keeps it, so the next tap
// shares at once.
//
// 🔴 THE SHARE SHEET NEEDS THE TAP'S OWN USER ACTIVATION (iOS Safari refuses
// navigator.share once a tap's handler has awaited a fetch), SO THE ADDRESS IS
// FETCHED BEFORE THE CLICK: on the button's pointerdown (and keyboard focus),
// ensure() asks the server for the page's link, and the click — a few hundred
// milliseconds later — finds it on the button and calls navigator.share
// SYNCHRONOUSLY, inside the click's own dispatch. The Pages screens draw the
// address on the button already. Only a click that beats its own pointerdown's
// fetch takes the slow path: the address is put on the clipboard in the same
// click (a ClipboardItem whose content is the pending fetch — what Safari
// allows), the sheet is tried when the fetch lands, and if the browser
// refuses it the toast says the link is copied; the next tap shares at once.
// A pointerdown that turns into a scroll has made (or found) the page's link
// and given it to nobody: it is still only the owner's, until it is sent.
const pageShareJS = `
  if (!window.__musterPageShare) {
    window.__musterPageShare = true;
    var pageToast = function (msg) {
      var host = document.getElementById('action-toasts');
      if (!host) return;
      var t = document.createElement('div');
      t.className = 'pointer-events-auto break-all rounded-lg bg-s2 px-3 py-2 text-sm font-medium text-fg shadow-lg shadow-black/30 ring-1 ring-line';
      t.setAttribute('role', 'status');
      t.setAttribute('data-page-toast', '');
      t.textContent = msg;
      host.appendChild(t);
      setTimeout(function () { t.remove(); }, 5000);
    };
    var setShareURL = function (id, url) {
      document.querySelectorAll('[data-page-share][data-page-id="' + id + '"]').forEach(function (b) {
        b._ensure = null;
        if (url) b.setAttribute('data-share-url', url); else b.removeAttribute('data-share-url');
      });
      document.querySelectorAll('[data-share-url-field][data-page-id="' + id + '"]').forEach(function (f) { f.value = url || ''; });
    };
    var postJSON = function (src) {
      return fetch(src, { method: 'POST', credentials: 'same-origin', headers: { Accept: 'application/json' } }).then(function (r) {
        return r.json().catch(function () { return {}; }).then(function (j) {
          if (!r.ok || j.error) throw new Error(j.error || 'That did not work. Try again.');
          return j;
        });
      });
    };
    // ensure is the button's address: its own, else ONE fetch per button
    // (kept while it is in flight, and its answer kept on the button).
    var ensure = function (btn) {
      var have = btn.getAttribute('data-share-url');
      if (have) return Promise.resolve(have);
      if (!btn._ensure) {
        btn._ensure = postJSON(btn.closest('form').action).then(function (j) {
          if (j.reusable) setShareURL(btn.getAttribute('data-page-id'), j.url);
          else btn.setAttribute('data-share-url', j.url);
          return j.url;
        }, function (e) { btn._ensure = null; throw e; });
      }
      return btn._ensure;
    };
    var copyShare = function (btn, url) {
      return window.cgCopyText(url).then(function () { pageToast('Link copied'); }, function () {
        btn.setAttribute('data-share-url', url);
        pageToast('Copy this link: ' + url);
      });
    };
    var canSheet = function (data) { return !!navigator.share && (!navigator.canShare || navigator.canShare(data)); };
    // sheet calls navigator.share NOW (the caller is still inside the tap,
    // or has nothing better), falling back to fallback() on any failure but a
    // cancel.
    var sheet = function (data, fallback) {
      var p;
      try { p = navigator.share(data); } catch (e) { p = Promise.reject(e); }
      return p.catch(function (e) {
        if (e && e.name === 'AbortError') return;
        return fallback();
      });
    };
    var sharePage = function (btn) {
      var title = btn.getAttribute('data-share-title') || document.title;
      var have = btn.getAttribute('data-share-url');
      if (have) {
        // The fast path: synchronous, inside the click's own dispatch.
        var data = { title: title, url: have };
        if (canSheet(data)) sheet(data, function () { return copyShare(btn, have); });
        else copyShare(btn, have);
        return;
      }
      if (btn.getAttribute('aria-busy') === 'true') return;
      btn.setAttribute('aria-busy', 'true');
      var got = ensure(btn);
      // The slow path: the address goes on the clipboard in THIS click, as a
      // ClipboardItem whose content is the pending fetch.
      var copied = null;
      if (window.ClipboardItem && navigator.clipboard && navigator.clipboard.write) {
        try {
          copied = navigator.clipboard.write([new ClipboardItem({ 'text/plain': got.then(function (u) { return new Blob([u], { type: 'text/plain' }); }) })]);
          copied.catch(function () {});
        } catch (e) { copied = null; }
      }
      var copiedOr = function (url) {
        if (!copied) return copyShare(btn, url);
        return copied.then(function () { pageToast('Link copied'); }, function () { return copyShare(btn, url); });
      };
      got.then(function (url) {
        var data = { title: title, url: url };
        if (canSheet(data)) return sheet(data, function () { return copiedOr(url); });
        return copiedOr(url);
      }).catch(function (e) { pageToast(e.message || 'The link could not be made.'); })
        .then(function () { btn.removeAttribute('aria-busy'); });
    };
    var early = function (e) {
      var btn = e.target.closest && e.target.closest('[data-page-share]');
      if (btn && !btn.getAttribute('data-share-url')) ensure(btn).catch(function () {});
    };
    document.addEventListener('pointerdown', early, true);
    document.addEventListener('focusin', early, true);
    document.addEventListener('click', function (e) {
      var btn = e.target.closest && e.target.closest('[data-page-share]');
      if (!btn) return;
      e.preventDefault();
      sharePage(btn);
    });
    document.addEventListener('submit', function (e) {
      var f = e.target.closest && e.target.closest('form[data-page-share-action]');
      if (!f) return;
      e.preventDefault();
      var act = f.getAttribute('data-page-share-action'), id = f.getAttribute('data-page-id');
      var menu = f.closest('details');
      postJSON(f.action).then(function (j) {
        if (menu) menu.open = false;
        if (act === 'rotate') {
          setShareURL(id, j.url);
          pageToast('New link made. The old one no longer works. Tap Share to send it.');
        } else {
          setShareURL(id, '');
          pageToast(j.revoked ? 'Link revoked. It no longer works.' : 'This page has no link to revoke.');
        }
        if (document.querySelector('[data-testid="page-detail"]')) setTimeout(function () { location.reload(); }, 600);
      }).catch(function (e) { pageToast(e.message); });
    });
  }
`
