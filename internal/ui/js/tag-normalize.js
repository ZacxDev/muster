// Tag normalization for the Tasks-UI chip editor.
//
// 🔑 ONE RULE, ONE PLACE. This used to be a third, hand-rolled spelling of
// notes.NormalizeTag living inline in components.go — and it had drifted: it did
// not strip a project slug's leading/trailing '-', so a chip DISPLAYED as
// `project:-foo-` SAVED as the different tag `project:foo`. An editor that
// silently saves something other than what it shows you is the exact failure the
// normalize/validate split exists to prevent.
//
// This file is the single copy. It is:
//   - go:embed-ed into internal/ui/components.go (tagScript) for the browser, and
//   - read verbatim by extension/tests/project-vectors.test.js, which drives it
//     from testdata/project-normalization-vectors.json — the SAME shared golden
//     table that drives internal/notes (Go) and extension/lib.js.
// Editing this rule without the others turns that cross-language test RED.
//
// It NORMALIZES only; it does not validate. An illegal tag still reaches the
// server, which rejects it LOUDLY by name. That split is deliberate: the editor
// must never silently discard what you typed.
//
// Kept as an ES5 function declaration with no imports/exports on purpose — it is
// spliced into an IIFE in a <script> tag, and eval'd by the test.
function normalize(raw) {
  // Go's `unicode.IsSpace` (the Unicode White_Space property) — what
  // strings.Fields splits on — NOT JavaScript's `\s`. They differ on exactly two
  // codepoints and both differences were live bugs: U+0085 NEL is White_Space
  // but `\s` does not match it, and U+FEFF is not White_Space but `\s` does.
  var WS = /[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+/;
  // Go's strings.ToLower is the SIMPLE (1:1) case mapping; JS toLowerCase is the
  // FULL (1:many) one. U+0130 (İ) is the only codepoint whose difference lands
  // inside the ASCII tag charset: Go gives "i", JS gives "i" + U+0307.
  var t = String(raw == null ? '' : raw)
    .replace(/\u0130/g, 'i')
    .toLowerCase()
    .split(WS)
    .filter(Boolean)
    .join('-');
  // `project:` slugs — and ONLY those — strip leading/trailing '-', matching
  // notes.NormalizeTag. The namespace literal is pinned to notes.NSProject by
  // TestChipEditorNormalizerUsesTheProjectNamespaceConstant.
  var i = t.indexOf(':');
  if (i > 0 && t.slice(0, i) === 'project') {
    t = 'project:' + t.slice(i + 1).replace(/^-+/, '').replace(/-+$/, '');
  }
  return t;
}
