package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE TASK BOARD IS IN THE TASKS TAB'S FIRST PAINT.
//
// `/` (and /tasks) used to draw #tasks-list as a skeleton and fill it from its
// own `load` request (GET /ui/tasks), so opening the board cost two round trips
// before a single task was on screen — and then the SSE stream's first open
// fired muster:resync, which fetched /ui/tasks a THIRD time to morph an
// identical list over the one just drawn. The document now carries the board
// (ShellData.WithTasks, built by internal/api's tasksView, the SAME build the
// partial serves), its list issues no load, and the first open re-fetches only
// when an event may have been missed (the mark, below).
//
// 🔴 ONE RENDERER. The document's list body and the /ui/tasks partial are both
// NotesCards of the same TasksView: TestTheBoardDocumentDrawsThePartial
// (internal/api) compares the two byte for byte.
//
// 🔴 THE MARK IS WHAT LETS THE FIRST CONNECT SKIP ITS REFETCH, AND IT IS NOT AN
// OPTIMISATION OF CORRECTNESS. The board is a snapshot taken when the document
// was built; the stream subscribes later (after the document's scripts run). An
// event in between reaches neither, and the first-open resync is what used to
// cover that gap. The document carries sse.Broadcaster.Mark as read BEFORE its
// list read (data-sse-mark); the stream sends its own mark, read AFTER it
// subscribed, as its first event (muster.hello). Equal marks mean no event for
// this user was published in between, so the drawn board is current;
// resyncScript then skips that ONE resync and consumes the mark. Any other
// open — a reconnect, a document with no mark, a stream whose hello differs or
// never comes — resyncs as before.
//
// ⚠ A STORED TAG FILTER STILL COSTS A FETCH. The filter lives in this browser
// (localStorage, tagScript), the server draws the unfiltered board, and
// tagScript's boot re-requests the filtered one. tasksRefilterGuard hides the
// drawn board until that answer lands, so a filtered reader never sees the
// unfiltered list flash first.

// tasksTagFilterKey is the localStorage key the board's tag filter is
// persisted under — tagScript's KEY and tasksRefilterGuard's read, one string.
const tasksTagFilterKey = "muster.tasks.tagfilter"

// ShellTab is the tab a shell document for the path segment tab draws active —
// the same clamp the shell applies (`/` is the default tab), so the server
// builds first-paint state for the tab the reader will actually see.
func ShellTab(tab string, feat Features) string { return normalizeVisibleTab(tab, feat) }

// shellTasks is the board a document draws on its first paint, and the event
// stream mark it was drawn at ("" when the server has no bus).
type shellTasks struct {
	view TasksView
	mark string
}

// WithTasks draws the board (v) into #tasks-list on the first paint. mark is
// the SSE mark read BEFORE v was built (sse.Broadcaster.Mark), or "".
func (d ShellData) WithTasks(v TasksView, mark string) ShellData {
	d.tasks = &shellTasks{view: v, mark: mark}
	return d
}

// tasksListLiveTrigger is #tasks-list's hx-trigger without the first load: the
// document drew the board. tasksListTrigger is the one for a document that did
// not.
const tasksListLiveTrigger = "tasks:changed from:body, sse:task.changed from:body, sse:agent.changed from:body, muster:resync from:body"

const tasksListTrigger = "load, " + tasksListLiveTrigger

// tasksListBody is what #tasks-list first holds: the drawn board, or the
// skeleton its own load replaces.
func tasksListBody(t *shellTasks) g.Node {
	if t == nil {
		return tasksListSkeleton()
	}
	return NotesCards(t.view)
}

// tasksTrigger is #tasks-list's hx-trigger for what the document drew.
func tasksTrigger(t *shellTasks) string {
	if t != nil {
		return tasksListLiveTrigger
	}
	return tasksListTrigger
}

// tasksDrawnAttrs marks a drawn board (data-tasks-drawn, which the refilter
// guard hides) and the mark it was drawn at (data-sse-mark, which
// resyncScript consumes on the first open).
func tasksDrawnAttrs(t *shellTasks) g.Node {
	if t == nil {
		return nil
	}
	return g.Group{
		g.Attr("data-tasks-drawn", ""),
		g.If(t.mark != "", g.Attr("data-sse-mark", t.mark)),
	}
}

// tasksRefilterGuard hides a DRAWN board while a stored tag filter is about to
// replace it.
//
// It is rendered just BEFORE #tasks-list, inside the panel, and that position is
// the mechanism: on a full load the parser runs it before the list exists, and
// on a boosted body swap htmx runs it in the same task as the swap — either way
// before the browser can paint the unfiltered board. It sets
// html[data-tasks-refilter]; the style hides #tasks-list[data-tasks-drawn]'s
// content (visibility, so the box keeps its height); the first request
// #tasks-list completes — tagScript boot's filtered refresh, success or failure
// alike, so an error box is never hidden — clears it.
//
// A document whose board was NOT drawn has no data-tasks-drawn, so its skeleton
// is never hidden.
func tasksRefilterGuard() g.Node {
	return g.Group{
		StyleEl(g.Raw(`html[data-tasks-refilter] #tasks-list[data-tasks-drawn] > *{visibility:hidden}`)),
		Script(g.Raw(`
(function () {
  try {
    var a = JSON.parse(localStorage.getItem('` + tasksTagFilterKey + `') || '[]');
    document.documentElement.toggleAttribute('data-tasks-refilter', Array.isArray(a) && a.length > 0);
  } catch (e) {}
  if (window.__musterTasksRefilter) return;
  window.__musterTasksRefilter = true;
  function done(e) {
    if (e.target && e.target.id === 'tasks-list') document.documentElement.removeAttribute('data-tasks-refilter');
  }
  document.addEventListener('htmx:afterRequest', done);
  document.addEventListener('htmx:afterSwap', done);
})();
`)),
	}
}
