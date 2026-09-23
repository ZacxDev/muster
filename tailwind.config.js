/** @type {import('tailwindcss').Config} */
//
// muster Tailwind config.
//
// Dark mode is the DEFAULT: the app root (<html>/<body>) carries the `dark`
// class by default so the UI renders dark-first, with light as an opt-in
// toggle. `darkMode: 'class'` makes that switch class-driven rather than tied
// to the OS preference.
//
// 🔴 TAILWIND DOES NOT ERROR ON A CONTENT PATH THAT MATCHES NOTHING. It emits
// no rules for it and exits 0, so a missing or mistyped entry produces a
// SMALLER, perfectly valid stylesheet and a page that renders unstyled — and it
// renders unstyled only where the built CSS is actually served, which is
// typically the container and not the developer's machine. There is no failure
// signal anywhere in the build. `make css-check` is that signal: it builds the
// stylesheet and then asserts that classes only reachable through each content
// entry are PRESENT in the output, so an entry that stopped matching fails
// loudly instead of shipping a thinner file.
module.exports = {
  darkMode: 'class',
  content: [
    // The Go UI components. This is where essentially every class in the app
    // is written, so losing this entry is the catastrophic case: the build
    // still succeeds and the stylesheet still parses.
    './internal/ui/**/*.go',
    // Static HTML/JS for the PWA shell and any hand-written templates.
    './web/**/*.{html,js}',
    //
    // ⚠ SEAM — A SECOND DOCUMENT SOURCE IS OWED BY THE API CARVE, AND IT WILL
    // FAIL SILENTLY IF NOBODY ADDS IT HERE.
    //
    //   WHAT: upstream emits one HTML document from OUTSIDE internal/ui — a
    //     hand-written login page that must render while the rest of the app is
    //     refusing, and therefore deliberately does not go through the component
    //     package. Its config names that ONE FILE explicitly.
    //   WHY NOT A PACKAGE-WIDE GLOB: upstream tried './internal/api/**/*.go'
    //     first. It scans _test.go files too, and Tailwind read fixture strings
    //     there as arbitrary-value classes — selectors built out of test data
    //     landed in the shipped stylesheet. An api package is full of JSON, SQL
    //     and log strings, so that recurs.
    //   CLOSING CONDITION: when the API carve lands a document-emitting file
    //     outside internal/ui, it adds that FILE (not its package) to this list
    //     and adds one of its classes to the css-check assertion set in the
    //     Makefile — so the entry is covered by a positive control rather than
    //     by having been typed.
    //   WHO CHECKS IT: the reviewer of the API-carve pull request. The
    //     mechanical check is `make css-check` failing before the entry is
    //     added and passing after.
  ],
  theme: {
    extend: {},
  },
  plugins: [],
};
