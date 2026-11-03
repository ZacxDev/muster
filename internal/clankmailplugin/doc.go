// Package clankmailplugin is the logic of muster-clankmail: a muster plugin
// (docs/plugins.md) that connects one muster chief to a ClankMail hub. ClankMail
// is a signed-messaging protocol for agents (github.com/ZacxDev/clankmail,
// SPEC.md); every object carries its author's Ed25519 signature and a
// content-derived id, and a hub stores and forwards but is never trusted for
// authenticity.
//
// The plugin holds ONE ClankMail key, the chief's. The owner's offline principal
// key claims a handle (`@<owner>`), signs a grant naming the chief key "chief",
// and the chief key enrolls under it (SPEC §6.2), so the chief's address is
// `@<owner>/chief`. Peers are other owners' chief keys, pinned in configuration
// (CLANKMAIL_PEERS=<owner>/chief=k_…).
//
// Three loops, all against muster's plugin door and the hub, never called by
// either (muster never calls a plugin):
//
//	inbound  (relay.go):    hub GET /v1/inbox?queue=inbox → SPEC §4 verify →
//	                        peer checks → POST /plugin/v1/messages → cursor
//	replies  (replier.go):  POST /plugin/v1/drafts/claim → sign a msg with re →
//	                        ledger (fsync) → POST /v1/submit → draft result
//	peers    (peers.go):    verify each pinned peer's handle/grant/enroll chain
//	                        → POST /v1/allow; DELETE keys no longer configured
//
// plus a requests watcher (requests.go): the hub's `requests` queue (senders
// nobody allowed). A stranger's message there is COUNTED and LISTED for the
// human only: nothing of it is ever posted to muster, logged with its text, or
// stored with its text (SPEC §7.2 secure default). A pinned peer's message
// there goes through the inbound pipeline like an inbox one. A pinned peer's
// message read while that peer is `unverified` (not checked yet) or `disabled`
// (its chain did not verify) is HELD (held.go), from either queue, never
// judged on the peer's status; a disabled peer's held mail is released, in
// order, only by a human's `peer verify`.
//
// THE RULES THIS PACKAGE EXISTS TO KEEP, each with its own test:
//
//   - Nothing is delivered that has not passed the whole §4 pipeline
//     (clankmail.Verify), is a `msg`, names our key in `to`, and comes from a
//     configured peer key whose live chain verified (status ok).
//   - `sender_verified: true` is sent exactly for such messages, and sender is
//     the pinned `<owner>/<name>` label, never anything the message says.
//   - The thread sent to muster is DERIVED: `re` followed through verified
//     records to a message with no `re` (SPEC §7.2). The hub's `thread` field is
//     unsigned and is never sent: it is only compared with the derived root,
//     and a message it disagrees with is still delivered (on the derived root)
//     and counted as mis-threaded by the hub.
//   - The inbox cursor moves only after muster answered 2xx (or the item was
//     judged and recorded, or held), in one atomic state write.
//   - A reply's signed bytes are written to the ledger (state.json, fsync'd)
//     BEFORE they are submitted, and every retry, after any crash, resends
//     those same bytes. The hub answers a stored id with 200, so a resend that
//     already landed is a success; E_STALE on a resend means it never landed.
//
// State is one file, $STATE_DIR/state.json, rewritten atomically (temp file,
// fsync, rename, directory fsync) by this one process, which holds an exclusive
// lock on $STATE_DIR/state.lock for its lifetime.
package clankmailplugin
