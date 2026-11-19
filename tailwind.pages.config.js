/** @type {import('tailwindcss').Config} */
//
// The pages content document's Tailwind config (web/css/page-content.css ->
// internal/pages/content_theme.css, `make css`).
//
// It IS tailwind.config.js (the same theme: every colour a role reading the
// palette's custom properties), with three changes:
//   - content: the markdown renderer ONLY. A page's markdown block is drawn by
//     ui.MarkdownHTML, which writes the app's utility classes; the content
//     document must define exactly those. TestEveryMarkdownClassHasARule
//     (internal/pages) renders every construct and fails on a class this
//     build does not define.
//   - important: '.mp-markdown' scopes every utility to a markdown block
//     (`.mp-markdown .my-1{...}`), so a page's own HTML block is untouched by
//     them whatever classes it writes.
//   - no preflight: the app's reset would restyle a page's own HTML blocks.
//     The few reset rules the utilities rely on (a border's style) are in
//     internal/pages contentCSS, scoped the same way.
const app = require('./tailwind.config.js');

module.exports = {
  ...app,
  content: ['./internal/ui/markdown.go'],
  important: '.mp-markdown',
  corePlugins: { preflight: false },
};
