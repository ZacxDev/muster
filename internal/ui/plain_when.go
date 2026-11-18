package ui

import (
	"html"
	"strconv"
	"time"
)

// A TIME IN WORDS (the operator rule: no timestamp in the top layer). A page
// screen says "Updated today at 9:14", never "2026-11-11 14:02 UTC"; the exact
// time, where it is needed at all, is behind an ⓘ.
//
// The server does not know the reader's time zone, so it writes only the
// DAY ("today", "on Nov 11", reckoned in UTC) — never a clock it would have
// to label "UTC" — and the app script (plainWhenJS) redraws every
// <time data-plain-when> in the reader's own zone, with the time, as the
// document (or an htmx swap) loads: "today at 9:14".

// PlainWhen is t's day in words, against now, in UTC: "today",
// "yesterday", "on Nov 11", "on Nov 11, 2025".
func PlainWhen(t, now time.Time) string {
	t, now = t.UTC(), now.UTC()
	ty, tm, td := t.Date()
	ny, nm, nd := now.Date()
	py, pm, pd := now.AddDate(0, 0, -1).Date()
	switch {
	case ty == ny && tm == nm && td == nd:
		return "today"
	case ty == py && tm == pm && td == pd:
		return "yesterday"
	case ty == ny:
		return "on " + t.Format("Jan 2")
	}
	return "on " + t.Format("Jan 2, 2006")
}

// PlainWhenHTML is t in words after prefix ("Updated "), as a <time> the app
// script redraws in the reader's own zone with its time: its datetime is the
// exact time (never drawn), its text PlainWhen's day.
func PlainWhenHTML(prefix string, t, now time.Time) string {
	return `<time datetime="` + t.UTC().Format(time.RFC3339) + `" data-plain-when="` + strconv.FormatInt(t.UnixMilli(), 10) +
		`" data-plain-when-prefix="` + html.EscapeString(prefix) + `">` + html.EscapeString(prefix+PlainWhen(t, now)) + `</time>`
}

// plainWhenJS redraws each <time data-plain-when> in the reader's zone, with
// PlainWhen's words; the server's text stays where it cannot.
const plainWhenJS = `
  (function () {
    var draw = function () {
      var ts = document.querySelectorAll('time[data-plain-when]');
      var now = new Date();
      var yday = new Date(now.getFullYear(), now.getMonth(), now.getDate() - 1);
      for (var i = 0; i < ts.length; i++) {
        try {
          var ms = parseInt(ts[i].getAttribute('data-plain-when'), 10);
          if (!ms) continue;
          var d = new Date(ms);
          var clock = d.toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' });
          var day;
          if (d.toDateString() === now.toDateString()) day = 'today';
          else if (d.toDateString() === yday.toDateString()) day = 'yesterday';
          else if (d.getFullYear() === now.getFullYear()) day = 'on ' + d.toLocaleDateString([], { month: 'short', day: 'numeric' });
          else day = 'on ' + d.toLocaleDateString([], { month: 'short', day: 'numeric', year: 'numeric' });
          ts[i].textContent = (ts[i].getAttribute('data-plain-when-prefix') || '') + day + ' at ' + clock;
        } catch (e) {}
      }
    };
    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', draw); else draw();
    // A fragment htmx swaps in is drawn too (once per window).
    if (!window.__musterPlainWhen) {
      window.__musterPlainWhen = true;
      document.addEventListener('htmx:load', draw);
    }
  })();
`
