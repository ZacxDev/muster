package ui

import (
	"crypto/sha256"
	"encoding/hex"

	g "maragu.dev/gomponents"
)

// agentsRulesJS defines window.__musterAgentsRules — the chips' table, the
// selector builder and the hide rules — once per window AND VERSION. It is
// emitted in BOTH the head script and agentsFilterScript, from this one
// constant: a boosted navigation into the Agents tab from a document without
// the shell's head (an agent's page) runs the body script with no head script
// ever having run.
//
// 🔴 VERSIONED (agentsRulesVersion). A window keeps its globals across boosted
// navigations, including one into a page a NEWER server rendered. A table
// defined "once per window" then stayed the OLD one: chips the new page drew
// (data-chip="waiting") looked up nothing in it and filtered nothing, and a
// stored selection of them was dropped as unknown. The table is rebuilt
// whenever its version is not this build's.
var agentsRulesJS = `
if (!window.__musterAgentsRules || window.__musterAgentsRules.v !== '` + agentsRulesVersion() + `') (function () {
    var CHIP_SEL = ` + chipSelectorsJSON() + `;
    var CHIP_IDS = ` + chipIDsJSON() + `;
    var CHIP_ALIASES = ` + chipAliasesJSON() + `;
    // sel is the attribute selector for ONE term {attr, value, exact}, its value
    // a CSS double-quoted string: [a="v"] for an exact term, [a*="v"] for every
    // other. The rule that hides and the query that counts are both built from it.
    function sel(t) { return '[' + t.attr + (t.exact ? '=' : '*=') + '"' + t.value.replace(/\\/g, '\\\\').replace(/"/g, '\\"') + '"]'; }
    // chipsOf is a stored chip list, cleaned to the known chips: a retired
    // id is read as the chip that replaced it (CHIP_ALIASES), once.
    function chipsOf(raw) {
      try {
        var v = JSON.parse(raw || '[]'), out = [];
        if (!Array.isArray(v)) return [];
        v.forEach(function (c) {
          if (typeof c === 'string' && Object.prototype.hasOwnProperty.call(CHIP_ALIASES, c)) c = CHIP_ALIASES[c];
          if (CHIP_IDS.indexOf(c) >= 0 && out.indexOf(c) < 0) out.push(c);
        });
        return out;
      } catch (e) { return []; }
    }
    // chipSel is the chips' compound selector for a selection ('' for none).
    function chipSel(cs) { return CHIP_SEL[CHIP_IDS.filter(function (c) { return cs.indexOf(c) >= 0; }).join(',')] || ''; }
    // rules is the hide rules for a filter: the shown host (exact; '' = All)
    // and the chips' selector cs. hostHas: the host rule applies only to a list
    // holding one of its cards (the first paint, where the script has not
    // looked yet).
    function rules(host, cs, hostHas) {
      var r = ['#agents-list > article[data-held]{display:none !important}'];
      if (host) {
        var h = sel({ attr: 'data-f-host', value: host, exact: true });
        r.push((hostHas ? '#agents-list:has(> article' + h + ')' : '#agents-list') + ' > article:not(' + h + '){display:none !important}');
      }
      if (cs) r.push('#agents-list > article:not(' + cs + '){display:none !important}');
      return r.join('\n');
    }
    window.__musterAgentsRules = { v: '` + agentsRulesVersion() + `', sel: sel, chipsOf: chipsOf, chipSel: chipSel, rules: rules, CHIP_IDS: CHIP_IDS };
})();
`

// agentsRulesVersion is a hash of everything agentsRulesJS takes from Go — the
// chips' selectors, ids and aliases — and of the table's shape, so a window
// holding a table built from other values, or an older build's table (one
// whose rules took the removed search box's terms), rebuilds it.
func agentsRulesVersion() string {
	sum := sha256.Sum256([]byte(agentsRulesShape + "\x00" + chipSelectorsJSON() + "\x00" + chipIDsJSON() + "\x00" + chipAliasesJSON()))
	return hex.EncodeToString(sum[:6])
}

// agentsRulesShape names the table's API: rules(host, cs, hostHas). Changed
// whenever that API changes, so a window still holding an older build's
// table — rules(ts, host, cs, hostHas) — rebuilds it rather than calling it
// with the wrong arguments.
const agentsRulesShape = "rules(host,cs,hostHas)"

// THE LIST'S FILTER STATE, APPLIED BEFORE THE FIRST PAINT.
//
// The Agents tab's document carries its cards (agents_list_ssr.go). What the
// reader had the list narrowed to — the host tab and the quick-filter chips
// (both localStorage) — lives in THIS browser, not on the server.
// agentsFilterScript applies it, but it is at the end of <body>: a browser may
// paint the list before it runs (a long document is parsed and painted in
// pieces), and the list would then show unfiltered and shrink to the filtered
// set — every card below the first hidden one jumping up.
//
// So this script, in <head>, writes the SAME hide rules into the SAME <style>
// (#agents-filter-style) before <body> exists. agentsFilterScript later
// rewrites that element with its own, equal, rules; the cards never move.
//
// 🔴 ONE TABLE. The chips' table and the hide rules are defined once, in
// agentsRulesJS (window.__musterAgentsRules), and both scripts use them — the
// head and the body cannot disagree about what a filter hides.
//
// 🔴 THE REMOVED SEARCH BOX'S TEXT IS NEVER READ — neither its sessionStorage
// copy nor a `?q=` — and the stored copy is deleted here, so a filter typed
// before the box was removed cannot hide a card now that there is no box to
// clear it from (agents_filter.go).
//
// ⚠ WHY NOT A COOKIE (so the server could render the filtered list itself).
// The state is per-browser by design (the tab and the chips per device), and
// the body script already owns it; a cookie would be a second copy of it on
// every request. Every card is still rendered; this only hides, as the script
// always has.
//
// ⚠ THE HOST TAB'S FALLBACK WITHOUT THE DOM. The script shows All while the
// remembered host has no card on the list (syncHosts); here, before the list
// exists, that is the rule's `:has()` — the host's rule applies only to a list
// that HAS one of that host's cards.
func agentsFilterHead() g.Node { return staticScript(agentsFilterHeadSrc) }

// agentsFilterHeadSrc is the path agentsFilterHead's code is served at, /static/js/agents-filter-head.js (static_script.go).
var agentsFilterHeadSrc = staticJS("agents-filter-head", agentsRulesJS+`
(function () {
  var R = window.__musterAgentsRules;

  // The first paint: the state as agentsFilterScript's boot reads it.
  try {
    try { sessionStorage.removeItem('`+retiredFilterStorageKey+`'); } catch (e) {}
    var host = '';
    try { host = localStorage.getItem('`+agentsHostTabsStorageKey+`') || ''; } catch (e) {}
    var cs = '';
    try { cs = R.chipSel(R.chipsOf(localStorage.getItem('`+agentsChipsStorageKey+`'))); } catch (e) {}
    // The Idle section (idle_section.go): open only if this viewer opened it.
    var open = false;
    try { open = localStorage.getItem('`+agentsIdleOpenKey+`') === '1'; } catch (e) {}
    document.documentElement.toggleAttribute('data-agents-idle-open', open);
    var css = R.rules(host, cs, true);
    var st = document.createElement('style');
    st.id = 'agents-filter-style';
    st.textContent = css;
    document.head.appendChild(st);
  } catch (e) {}
})();
`)
