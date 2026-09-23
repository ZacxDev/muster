// The Tasks-UI filter-chip toggle rule.
//
// Extracted from the inline click handler so the rule is EXECUTABLE by a test
// (extension/tests/filter-toggle.test.js reads these exact bytes) instead of
// only assertable as a substring. It is go:embed-ed into
// internal/ui/components.go (tagScript).
//
// 🔑 Two selection modes, and the second one is the whole point:
//   - Ordinary tags are MULTI-SELECT AND. Two tags narrow the list.
//   - A chip that declares `data-tag-filter-exclusive="<prefix>"` is
//     SINGLE-SELECT within that prefix. `project:` is such a group: a task
//     carries at most one project, so ANDing two project chips is a guaranteed
//     empty result — and only the first would even render aria-pressed, because
//     the row picks the FIRST project tag it finds as "the" active one. Clicking
//     a second project must REPLACE the first, not add to it.
//
// Pure: takes and returns the tag array, touches no DOM.
function toggleFilter(cur, tag, group) {
  var out = Array.isArray(cur) ? cur.slice() : [];
  if (group) {
    out = out.filter(function (x) { return x === tag || x.indexOf(group) !== 0; });
  }
  var i = out.indexOf(tag);
  if (i >= 0) out.splice(i, 1); else out.push(tag);
  return out;
}
