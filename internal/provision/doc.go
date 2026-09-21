// Package provision is muster's pluggable provisioner contract: the seam
// between "an agent should exist, configured like this" and whatever actually
// runs it — a Kubernetes cluster, a container daemon, a process on the box.
//
// It declares the interface, the data that crosses it, and a capability
// declaration callers are expected to BRANCH on. It contains no driver-specific
// code and imports nothing outside the standard library, so a caller that wires
// the no-op driver pays for none of a cluster client's dependency tree.
//
// # Why this exists at all
//
// The project muster was extracted from had this boundary physically — its
// 1,201-line provisioning file imported nothing from any cluster library and
// talked to two injected clients at roughly 25 call sites — but never declared
// it. The consequences were not theoretical:
//
//   - The lifecycle entry point took a DATABASE ROW ID and loaded the row
//     itself, so a second driver would have needed the first one's schema.
//   - The conversation with the running agent was on the same interface as
//     "create a pod", so the interface carried a sentinel error naming a
//     specific vendor's image VERSION.
//   - The one address an agent was reachable at was a hardcoded string with no
//     override anywhere in the codebase.
//   - Every file placed into an agent was shipped as base64 inside a shell
//     one-liner, wrapped in `{ … } || :` because the surrounding script ran
//     under `set -e` and a bare non-zero exit was a crash loop.
//
// Each of those is fixed here by the shape of the types rather than by a rule
// somebody has to remember: [Ref] carries identity without a database, the
// interface has no chat method at all, [Provisioner.Endpoint] is a method, and
// [Spec.Files] is data.
//
// # The four contract rules
//
// These are behavioural promises, not documentation. Each is pinned by a test
// in provisiontest.RunContract, which every driver in this repository must
// pass.
//
//  1. 🔴 A DRIVER THAT CANNOT SEE ITS BACKEND RETURNS AN ERROR, NEVER AN EMPTY
//     SET. [Provisioner.List] and [Provisioner.Get] must fail loudly when the
//     backend is unreachable. This looks like a nicety and is not: the caller
//     treats a List error as "no live information, keep the stored status" and
//     an empty List as "nothing is running" — so a blind driver returning
//     `nil, nil` silently rewrites every instance's state to stopped. Return
//     [ErrBlind] (wrapped is fine) rather than an empty slice.
//
//  2. 🔴 [Provisioner.Destroy] RETURNS nil ONLY IF THE INSTANCE WAS REMOVED OR
//     WAS ALREADY ABSENT. A bare `return nil` satisfies the compiler and means
//     nothing; it is the "declared but inert" shape, and it reads as coverage.
//     A driver must be able to say afterwards that the thing is gone —
//     [Provisioner.Get] must return [ErrNotFound].
//
//  3. 🔴 [Provisioner.Create] IS IDEMPOTENT, AND A DIVERGENT SPEC IS AN ERROR.
//     Creating the same instance twice with the same spec succeeds twice.
//     Creating it with a DIFFERENT spec returns [ErrDivergentSpec] rather than
//     silently overwriting — the caller that wanted the overwrite has
//     [Provisioner.Update] and can say so.
//
//  4. 🔴 [Capabilities] IS USELESS UNLESS CALLERS BRANCH ON IT. A field that
//     exists is not a guard. [CheckSpec] is that branch, it lives in exactly
//     one place, and every driver's Create/Update calls it before touching a
//     backend. The security case is [Grant]: a policy a driver cannot
//     interpret must be REFUSED, because an interface reporting "granted" for
//     a policy nobody applied is worse than having no policy feature at all.
//
// # What is deliberately NOT here
//
// Talking to a running agent. Sending it a message, streaming its tokens,
// servicing its tool calls — none of that is provisioning, and putting it here
// is what made the original interface un-implementable by anything that was not
// one specific container image. A runtime interface is separate work.
package provision
