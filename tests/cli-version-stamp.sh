#!/usr/bin/env bash
# The LINK-TIME REVISION STAMP, asserted against a BUILT BINARY.
#
# Usage: tests/cli-version-stamp.sh <path-to-muster-binary> <expected-revision>
#
# ---------------------------------------------------------------------------
# WHAT IT CHECKS. `flake.nix`'s `mkCLI` links the CLI with
# `-X main.buildRevision=${version}`, `cmd/muster/client.go`'s `cliVersion()`
# composes that into what cobra prints for `--version`, and this script reads it
# back off the installed artefact.
#
# 🔴 THE "WHY" FOR THE BUILD DECISION LIVES IN `flake.nix` AT THE `ldflags`
# LINE — read it there. It is not repeated here on purpose: the same rationale
# used to be written out in both files, and two copies of a reason for one line
# of `ldflags` is two things to keep in step. (`client.go`'s `buildRevision`
# declaration states the provenance story too, deliberately — see the note in
# that `flake.nix` block.) In short: the revision a consumer
# fetched was recorded in the DERIVATION NAME and nowhere the program could
# reach, and `buildRevision` is a SEPARATE variable from `buildVersion` because
# the latter is a server semver that two readers print beside the server's own.
#
# 🔴 WHY IT IS A SHELL SCRIPT IN THE DERIVATION AND NOT A GO TEST. The defect is
# in the BUILD, not in the code: every Go test in `cmd/muster` compiles without
# the release `ldflags` and therefore observes the defaults — correctly, which is
# what `server_pins_test.go` and `cmd/muster/version_semantics_test.go` pin. No
# in-process test can see whether the packaging applied the override, for the
# same reason `tree_test.go` cannot see which binary a build produced (see
# verb-ledger.sh).
# The only witness is the installed artefact's own output.
#
# 🔴 WHY IT IS A SEPARATE SCRIPT FROM verb-ledger.sh, WHICH ALSO RUNS AGAINST
# THE BUILT BINARY — I.E. WHY MERGING THEM WOULD BREAK CI. verb-ledger.sh runs
# in TWO places: this flake's `installCheckPhase` and `make verb-ledger`, which
# CI runs against a plain `go build` output on a runner with no nix. A plain
# `go build` is UNSTAMPED by construction, so folding this assertion into that
# script would make it fail on every PR for a binary that is behaving exactly as
# specified. The stamp is a property of the nix build; its check belongs only
# where the stamp is applied.
#
# 🔴 THE EXIT-CODE CONTRACT, AND IT IS LOAD-BEARING:
#     0  the artefact names the revision this build stamped
#     1  ASSERTION FAILURE — the binary is wrong (or the parser's format
#        dependency broke, which is reported with its own message)
#     2  BROKEN HARNESS — the caller passed something unusable
# A harness error reported as an assertion failure sends a reader to look at the
# binary for a defect that is in the invocation, so the two are never merged.
#
# 🔴 WHAT THIS STRUCTURALLY CANNOT SEE, so nobody reads a green run as more than
# it is:
#   * It compares the binary's output against a revision string the CALLER
#     supplies — the same `version` binding `flake.nix` feeds to `ldflags`. That
#     makes it a check of the PLUMBING (is the symbol name right, did `ldflags`
#     reach this `subPackages` build, is the override actually linked in), and
#     NOT a check that the binding itself is meaningful. If `flake.nix`'s
#     `version` degenerated to a constant, this would stay green.
#     Consequently the REV clause's assertion does NOT refuse `unknown`, which
#     is what that binding yields for a build with no git metadata at all
#     (`nix build path:.`). That spelling is supported, so refusing it there
#     would break it. ⚠ That trade is about the REV clause ONLY — it is not a
#     reason for the server-version half below to accept a revision, and it was
#     once mis-cited as one.
#   * It says nothing about whether the `buildVersion` half holds the RIGHT
#     server version — only that it does not hold a revision (the check at the
#     end of this script). That half is the muster SERVER version the client was
#     built against, it is pinned to `api.BuildVersion`'s DEFAULT by
#     `cmd/muster/server_pins_test.go`, and nothing in this repository stamps it
#     at all. `warnSkew` in client.go is what compares it against a live
#     `/health`.
# ---------------------------------------------------------------------------

set -euo pipefail

BIN="${1:-}"
WANT="${2:-}"

# 🔴 ONE USAGE CHECK, COVERING EMPTY AND BLANK ALIKE. There used to be a second,
# dedicated guard below for a whitespace-only `$WANT`; it was removed because
# the only caller — `version = self.shortRev or self.dirtyShortRev or "unknown"`
# — cannot produce whitespace, it had never fired, and its comment credited it
# with catching the EMPTY case that this check was already catching (measured:
# an empty second argument printed this usage line and exited 2, never reaching
# the dedicated guard). Trimming whitespace here keeps the blank case an exit-2
# harness error rather than letting it fall through to the exit-1 assertion and
# blame the binary for the caller's argument.
if [ -z "$BIN" ] || [ -z "${WANT//[[:space:]]/}" ]; then
  echo "usage: $0 <path-to-muster-binary> <expected-revision>" >&2
  echo "  An empty or blank expected revision is a defect in the CALLER" >&2
  echo "  (flake.nix's \`version\` binding), not in the binary." >&2
  exit 2
fi
if [ ! -x "$BIN" ]; then
  echo "cli-version-stamp: $BIN is not an executable file" >&2
  exit 2
fi

# UNSTAMPED_DEFAULT is the literal `cmd/muster/client.go` declares for
# `buildVersion`, which is what `cliVersion()` returns ALONE when no revision was
# stamped. It is used ONLY to produce a named diagnosis below — the gate is the
# equality check, not this value — so a future change to the Go default cannot
# silently weaken the assertion, only make one error message less specific.
UNSTAMPED_DEFAULT="dev"

# MARKER CONTROL — can this script read a version line out of this binary AT ALL?
#
# 🔴 A STRING COMPARISON AGAINST AN EMPTY EXTRACTION IS NOT A MEASUREMENT. This
# script parses cobra's version template, so that FORMAT is a dependency it did
# not pin. Without this control, a cobra release that reworded the line and a
# binary that reports the wrong revision produce the same empty capture, and the
# assertion below would blame the binary for the parser's problem. Those are
# different problems; this message exists so they are not confused.
raw="$("$BIN" --version 2>&1)" || {
  echo "🔴 cli-version-stamp: FAIL — '$BIN --version' exited non-zero." >&2
  echo "  Output was: $raw" >&2
  exit 1
}

if ! printf '%s\n' "$raw" | grep -q ' version '; then
  echo "🔴 cli-version-stamp: FAIL — '$BIN --version' produced no line matching" >&2
  echo "  '<name> version <value>'. Either the binary has no --version flag, or" >&2
  echo "  cobra's version template changed and this parser needs updating." >&2
  echo "  Output was: $raw" >&2
  exit 1
fi

got="$(printf '%s\n' "$raw" | sed -n 's/^.* version \(.*\)$/\1/p' | head -n1)"
got="${got%"${got##*[![:space:]]}"}"

# THE REVISION CLAUSE. `cliVersion()` renders "<server-version> (rev <revision>)"
# when a revision was stamped and "<server-version>" alone when it was not, so
# the clause's ABSENCE is exactly the original defect: no `ldflags` reached this
# binary. That is reported here, with the diagnosis, rather than letting an
# empty capture reach the equality check below as a mystery mismatch.
rev="$(printf '%s\n' "$got" | sed -n 's/^.*(rev \(.*\))$/\1/p')"
if [ -z "$rev" ]; then
  echo "🔴 cli-version-stamp: FAIL — '$BIN --version' reports '$got', which carries no" >&2
  echo "  '(rev <revision>)' clause. The link-time override NEVER REACHED THIS" >&2
  echo "  BINARY." >&2
  if [ "$got" = "$UNSTAMPED_DEFAULT" ]; then
    echo "" >&2
    echo "  '$got' is the IN-SOURCE DEFAULT of \`buildVersion\` from" >&2
    echo "  cmd/muster/client.go, printed alone because \`buildRevision\` is still" >&2
    echo "  the empty default. This is the original defect: the derivation built" >&2
    echo "  the CLI with no \`ldflags\`, so \`muster --version\` answered 'dev' while" >&2
    echo "  the store path named the revision." >&2
  fi
  echo "" >&2
  echo "  Check that \`mkCLI\` in flake.nix sets" >&2
  echo "      ldflags = [ \"-X main.buildRevision=\${version}\" ];" >&2
  echo "  and that the symbol path matches the variable's package (it is \`main\`," >&2
  echo "  because cmd/muster IS a main package — a wrong path is accepted by the" >&2
  echo "  linker and silently stamps nothing)." >&2
  exit 1
fi

vers="$(printf '%s\n' "$got" | sed -n 's/^\(.*\) (rev .*)$/\1/p')"

echo "cli-version-stamp: $BIN reports version '$got'; the build stamped revision '$WANT'"

# 🔴 THE SERVER-VERSION HALF MUST NOT CARRY A REVISION, AND THIS IS THE ONLY
# CHECK IN THE PROJECT THAT CAN SEE IT. `main.buildVersion` is the muster SERVER
# version the client was built against; it is the value `warnSkew` COMPARES
# against a live `/health`, and it is the unlabelled first half of every line
# `cliVersion()` renders. A build that stamps the revision into that symbol puts
# a git rev there with nothing marking it as one, where it reads as a real
# version and cannot be compared to one — the exact regression the
# `buildVersion`/`buildRevision` split removed. Every Go test in `cmd/muster`
# compiles without release `ldflags` and is therefore blind to it by
# construction; only an assertion on the linked artefact can refuse it.
#
# 🔴 THE TEST IS THE SHAPE OF `$vers`, NOT EQUALITY WITH `$WANT`, AND THE
# DIFFERENCE IS THE WHOLE FINDING. This was `[ "$vers" = "$WANT" ]`, which only
# sees the case where the SAME STRING reached both symbols. Measured green for
# exactly the regression the paragraph above declares it the only refuser of:
#
#     -X main.buildRevision=8ad413e -X main.buildVersion=8ad413e36e4b…
#     -> muster version 8ad413e36e4b… (rev 8ad413e)
#     -> cli-version-stamp: OK, exit 0
#
# A full sha in `buildVersion` and a short rev in `buildRevision` is the most
# likely spelling of this mistake, and equality was blind to it. A shape test is
# not: no muster SERVER version is 7-to-40 lowercase hex characters with no
# separator. The equality clause is kept alongside it because it catches the one
# leak the shape test cannot — `$WANT` being the literal `unknown`, which
# `nix build path:.` produces and which is not revision-shaped.
#
# ⚠ WHAT THIS WOULD FALSELY REFUSE, stated rather than left as an absence: a
# server version that is itself 7+ lowercase hex characters with no dot or dash
# (`1234567`). `api.BuildVersion` is a semver and `make image` passes one, so
# that spelling does not occur; if it ever does, this message names the check to
# relax.
if printf '%s' "$vers" | grep -Eq '^[0-9a-f]{7,40}$' || [ "$vers" = "$WANT" ]; then
  echo "🔴 cli-version-stamp: FAIL — the SERVER-VERSION half of '$got' is '$vers'," >&2
  echo "  which is a value from the build's REVISION binding (a git revision, or" >&2
  echo "  its 'unknown' fallback), not a muster server version." >&2
  echo "" >&2
  echo "  The '(rev …)' half is provenance and is correct; the half before it is" >&2
  echo "  \`main.buildVersion\`, the muster SERVER version this client was built" >&2
  echo "  against, and something has stamped a git revision into it too." >&2
  echo "" >&2
  echo "  That puts an UNLABELLED revision on every surface client.go renders" >&2
  echo "  beside the server's own semver — the skew note and the route-absent" >&2
  echo "  404 — where it invites a comparison that cannot be made, and it is the" >&2
  echo "  value \`warnSkew\` compares, so the note would fire forever. Stamp ONLY" >&2
  echo "  \`-X main.buildRevision=\${version}\`; the server's version travels with" >&2
  echo "  the server (\`make image\` passes VERSION)." >&2
  exit 1
fi

# THE GATE. Equality, in both directions by construction.
#
# 🔴 THIS IS WHY EQUALITY IS THE RIGHT SHAPE AND "is a revision present" IS NOT.
# A mere presence check would pass for a binary stamped with the WRONG revision
# — e.g. `ldflags` wired to a stale literal, or to a second version binding that
# drifted from the one naming the derivation. Equality against the value this
# build actually used refuses that too.
if [ "$rev" != "$WANT" ]; then
  echo "🔴 cli-version-stamp: FAIL — the built binary reports revision '$rev' but this" >&2
  echo "  build stamped '$WANT'." >&2
  echo "" >&2
  echo "  Both values are non-empty, so \`ldflags\` IS being applied — it is" >&2
  echo "  applying the wrong value. The stamp and the derivation name must come" >&2
  echo "  from the one \`version\` binding in flake.nix; two sources is how they" >&2
  echo "  drift." >&2
  exit 1
fi

echo "cli-version-stamp: OK — the artefact names the revision it was built from."
