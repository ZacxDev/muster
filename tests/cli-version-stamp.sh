#!/usr/bin/env bash
# The LINK-TIME VERSION STAMP, asserted against a BUILT BINARY.
#
# Usage: tests/cli-version-stamp.sh <path-to-muster-binary> <expected-version>
#
# ---------------------------------------------------------------------------
# 🔴 WHY THIS EXISTS, STATED AS THE FAILURE IT CLOSES.
#
# `cmd/muster/client.go` declares `buildVersion = "dev"` and documents that it
# is overridable at link time. Nothing in this flake HELD it to that: `mkCLI`
# set no `ldflags`, so every nix-built CLI kept the Go default and
# `muster --version` answered
#
#     muster version dev
#
# — for a binary whose own store path said `muster-cli-4b5d128`. The revision a
# consumer actually fetched was recorded in the DERIVATION NAME and nowhere the
# program itself could reach, so "which muster is installed here?" was
# answerable only by someone who knew to run `readlink -f` on the binary. That
# is a provenance question asked during incidents, by people holding the
# artefact and not the store.
#
# 🔴 WHY IT IS A SHELL SCRIPT IN THE DERIVATION AND NOT A GO TEST. The defect is
# in the BUILD, not in the code: every Go test in `cmd/muster` compiles without
# the release `ldflags` and therefore observes the default — correctly, which is
# what `server_pins_test.go` pins. No in-process test can see whether the
# packaging applied the override, for the same reason `tree_test.go` cannot see
# which binary a build produced (see verb-ledger.sh). The only witness is the
# installed artefact's own output.
#
# 🔴 WHY IT IS A SEPARATE SCRIPT FROM verb-ledger.sh, WHICH ALSO RUNS AGAINST
# THE BUILT BINARY. verb-ledger.sh runs in TWO places: this flake's
# `installCheckPhase` and `make verb-ledger`, which CI runs against a plain
# `go build` output on a runner with no nix. A plain `go build` is UNSTAMPED by
# construction, so folding this assertion into that script would make it fail
# on every PR for a binary that is behaving exactly as specified. The stamp is a
# property of the nix build; its check belongs only where the stamp is applied.
#
# 🔴 WHAT THIS STRUCTURALLY CANNOT SEE, so nobody reads a green run as more than
# it is:
#   * It compares the binary's output against a version string the CALLER
#     supplies — the same `version` binding `flake.nix` feeds to `ldflags`. That
#     makes it a check of the PLUMBING (is the symbol name right, did `ldflags`
#     reach this `subPackages` build, is the override actually linked in), and
#     NOT a check that the binding itself is meaningful. If `flake.nix`'s
#     `version` degenerated to a constant, this would stay green.
#   * Consequently it does NOT refuse `unknown`, which is what that binding
#     yields for a build with no git metadata at all (`nix build path:.`). That
#     spelling is supported, so refusing it here would break it. A revision-
#     shaped assertion would catch the degenerate case and cost that support;
#     the trade is named here rather than left as an absence.
#   * It says nothing about the SERVER's version, and the two are not the same
#     claim. `warnSkew` in client.go compares this value against what /health
#     reports and notes a difference on stderr; a stamped client talking to a
#     differently-versioned server will emit that note. That was already true of
#     the `dev` default against any released server.
# ---------------------------------------------------------------------------

set -euo pipefail

BIN="${1:-}"
WANT="${2:-}"

if [ -z "$BIN" ] || [ -z "$WANT" ]; then
  echo "usage: $0 <path-to-muster-binary> <expected-version>" >&2
  exit 2
fi
if [ ! -x "$BIN" ]; then
  echo "cli-version-stamp: $BIN is not an executable file" >&2
  exit 2
fi

# 🔴 AN EMPTY EXPECTED VERSION IS A BROKEN HARNESS, NOT A FAILING BINARY, and it
# is refused separately so the two are never confused. It is also the one value
# that would make this check VACUOUS IN A HIDDEN WAY: cobra DISABLES `--version`
# entirely when `Version` is the empty string, so `-X main.buildVersion=` would
# produce a binary with no `--version` flag at all. Caught here as a usage error
# (exit 2), before any assertion below could report it as something else.
if [ -z "${WANT// /}" ]; then
  echo "cli-version-stamp: the expected version is empty or blank." >&2
  echo "  This is a defect in the CALLER (flake.nix's \`version\` binding), not in" >&2
  echo "  the binary. An empty -X value makes cobra drop the --version flag." >&2
  exit 2
fi

# UNSTAMPED_DEFAULT is the literal `cmd/muster/client.go` declares. It is used
# ONLY to produce a named diagnosis below — the gate is the equality check, not
# this value — so a future change to the Go default cannot silently weaken the
# assertion, only make one error message less specific.
UNSTAMPED_DEFAULT="dev"

# MARKER CONTROL — can this script read a version out of this binary AT ALL?
#
# 🔴 A STRING COMPARISON AGAINST AN EMPTY EXTRACTION IS NOT A MEASUREMENT. This
# script parses cobra's version template, so that FORMAT is a dependency it did
# not pin. Without this control, a cobra release that reworded the line and a
# binary that reports the wrong version produce the same empty capture, and the
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

echo "cli-version-stamp: $BIN reports version '$got'; the build stamped '$WANT'"

# THE GATE. Equality, in both directions by construction.
#
# 🔴 THIS IS WHAT THE DEFECT LOOKED LIKE AND WHY EQUALITY IS THE RIGHT SHAPE. A
# "not equal to dev" check would pass for a binary stamped with the WRONG
# revision — e.g. `ldflags` wired to a stale literal, or to a second version
# binding that drifted from the one naming the derivation. Equality against the
# value this build actually used refuses that too, and it is the only assertion
# here that can fail for the original reason: no `ldflags` at all.
if [ "$got" != "$WANT" ]; then
  echo "🔴 cli-version-stamp: FAIL — the built binary reports version '$got' but this" >&2
  echo "  build stamped '$WANT'." >&2
  if [ "$got" = "$UNSTAMPED_DEFAULT" ]; then
    echo "" >&2
    echo "  '$got' is the IN-SOURCE DEFAULT from cmd/muster/client.go, which means the" >&2
    echo "  link-time override NEVER REACHED THIS BINARY. This is the original defect:" >&2
    echo "  the derivation built the CLI with no \`ldflags\`, so \`muster --version\`" >&2
    echo "  answered 'dev' while the store path named the revision. Check that" >&2
    echo "  \`mkCLI\` in flake.nix sets" >&2
    echo "      ldflags = [ \"-X main.buildVersion=\${version}\" ];" >&2
    echo "  and that the symbol path matches the variable's package (it is \`main\`," >&2
    echo "  because cmd/muster IS a main package — a wrong path is accepted by the" >&2
    echo "  linker and silently stamps nothing)." >&2
  else
    echo "" >&2
    echo "  Both values are non-default, so \`ldflags\` IS being applied — it is" >&2
    echo "  applying the wrong value. The stamp and the derivation name must come" >&2
    echo "  from the one \`version\` binding in flake.nix; two sources is how they" >&2
    echo "  drift." >&2
  fi
  exit 1
fi

echo "cli-version-stamp: OK — the artefact names the revision it was built from."
