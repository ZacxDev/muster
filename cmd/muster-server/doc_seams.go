package main

// ---------------------------------------------------------------------------
// SEAMS THIS BINARY OPENS, AND WHAT CLOSES EACH ONE.
//
// This file declares nothing. It follows internal/api/doc_seams.go's shape
// because a seam recorded in a commit message is a seam nobody finds, and each
// entry names WHAT, WHY, the CLOSING CONDITION and WHO CHECKS IT — the four
// things that separate a work item from an object nobody can close.
//
// 🔴 THE DISTINCTION api.Extensions.defects DRAWS GOVERNS EVERY ENTRY HERE:
// "degrades visibly" versus "lies silently". A nil Runbooks store renders an
// empty Runbooks list, which is TRUE — this server has no runbooks. A notes
// store with no session-liveness probe renders "no transcript recorded" over
// transcripts that may be alive, with nothing logged and nothing 404ing. The
// first is a configuration; the second is a page that states a falsehood, and
// /readyz refuses to serve with it. Each nil below is argued on that line, not
// on convenience.
//
// ---------------------------------------------------------------------------
// 1. 🔴 api.Provisioner IS NIL, AND WILL BE UNTIL THE PROVISIONER ADAPTER IS
//    CARVED. THIS IS THE LARGEST SEAM IN THE BINARY.
//
//	WHAT: api.Extensions.Provisioner is the agent-pod lifecycle driver — nine
//	  methods: Dispatch, Start, Stop, Destroy, Instances, TailLogs, StreamLogs,
//	  Chat and ChatWithTools. NOTHING in this module implements it. The only
//	  implementation of that interface anywhere in this tree is a stub in
//	  internal/api/registration_test.go, which exists to prove route
//	  registration does not branch on it.
//	WHY IT IS NIL RATHER THAN FAKED: the upstream implementation is
//	  internal/agents/provision.go, which deliberately STAYED with the
//	  permission router — it is a helm/client-go client to a specific cluster,
//	  not domain logic. internal/provision is muster's from-scratch replacement
//	  and it is a DIFFERENT SHAPE: a driver contract (Create/Update/Scale/…)
//	  over instances, with a noop driver and a Kubernetes driver. Adapting one to
//	  the other is a chunk of work with its own design question — where
//	  gateway chat and tool dispatch live, since provision.Driver has no notion
//	  of either. Wiring a fake here would make the Agents tab LOOK operational:
//	  dispatch would report success, no pod would exist, and the agent would sit
//	  in `provisioning` for ever. That is the "lies silently" side of the line.
//	WHAT IT COSTS MEANWHILE, PRECISELY: every agent-control route is REGISTERED
//	  — registration in internal/api is unconditional by design, which is what
//	  makes the route golden a claim about production — and each one refuses at
//	  request time with 503 + provisionerUnwired:true, through the
//	  api.requireProvisioner wrapper it is registered behind. The Agents tab is
//	  read-only: existing rows render, nothing can be provisioned, started,
//	  stopped or destroyed, no logs stream and no chat turn runs. The boot banner
//	  says so on every start, unconditionally.
//	  🔴 THE ROUTE GOLDEN DOES NOT MOVE FOR THIS. If it ever does, the nil has
//	  started changing the route set, which is a defect in its own right.
//	  requireProvisioner is a WRAPPER for exactly that reason — the golden records
//	  patterns, not handlers, so the honest refusal costs no route drift.
//
//	🔴 THE SENTENCE ABOVE WAS FALSE WHEN IT WAS FIRST WRITTEN, AND THAT IS
//	  RECORDED HERE RATHER THAN QUIETLY CORRECTED. "Each one refuses at request
//	  time" was a claim about a wrapper that did not exist. Measured against the
//	  image this tree builds, with Provisioner nil, NINE routes answered 200 and
//	  did nothing — POST /agents, POST /agents/{id}/start, POST /agents/{id}/stop,
//	  DELETE /agents/{id}, GET /agents/{name}/logs/stream, GET /agents/{name}/ws,
//	  POST /chief/provision, POST /runbooks/{id}/dispatch and
//	  POST /api/agents/{name}/messages. A dispatch rendered a card over a row that
//	  stayed `provisioning`; a delete removed the card and left the row; the log
//	  stream wrote `: connected` and closed, which a reader takes for "no logs".
//	  🔴 THAT IS THE IDENTICAL OBSERVABLE THIS ENTRY REJECTS A FAKE FOR, six
//	  paragraphs up. Wiring nil and wiring a fake were the same lie; only the
//	  wrapper made the nil the honest option this entry always claimed it was.
//	  POST /chief/provision was worse than a lie: its goroutine was bare, so the
//	  nil deref killed the process rather than being recovered.
//	⚠ THE WRAPPER'S LEDGER IS THE THING TO UPDATE, NOT THIS PROSE. The routes
//	  above are pinned by TestEveryProvisionerRouteRefusesWhenItIsUnwired in
//	  internal/api/provisioner_seam_test.go, which fails when the set GROWS or
//	  SHRINKS. A tenth route reaching Provisioner without the wrapper reddens
//	  there, which is the mechanical half this paragraph cannot be.
//	CLOSING CONDITION: a pull request adding an adapter from provision.Driver to
//	  api.Provisioner — in its own package, consumer-side interfaces unchanged —
//	  wired here behind the configuration that names a driver, with
//	  internal/provision/provisiontest's contract suite green against it. When
//	  it lands, internal/provision/k8s LEAVES the not-linked ledger in
//	  internal/modulegate/linkage_test.go, and that departure is the mechanical
//	  signal this seam closed.
//	WHO CHECKS IT: the reviewer of that pull request, against the provisiontest
//	  contract results and the linkage ledger.
//
// ---------------------------------------------------------------------------
// 2. api.PrivilegeApplier IS NIL: GRANTS ARE RECORDED, NOT APPLIED.
//
//	WHAT: api.Extensions.PrivilegeApply applies a granted profile's Kubernetes
//	  RBAC to an agent's ServiceAccount, live. Nothing implements it here, for
//	  the same reason as entry 1 — it needs the in-cluster client and the
//	  SA/namespace naming that live with the provisioner.
//	WHY THIS ONE IS A CONFIGURATION AND NOT A LIE: a grant recorded and not
//	  applied is VISIBLE where it matters. The handlers log at the grant path,
//	  and the thing a grant is FOR — an agent pod with wider RBAC — does not
//	  exist either, because entry 1 means no pod is ever provisioned. There is
//	  no surface on which a user is told a privilege is live when it is not,
//	  because there is no live agent to hold it. 🔴 THAT ARGUMENT DIES THE
//	  MOMENT ENTRY 1 CLOSES. A provisioner that creates real pods, with a
//	  privilege store that records grants nobody applies, IS a page stating a
//	  falsehood — the grant chip would say granted over a ServiceAccount with
//	  none of the permissions.
//	CLOSING CONDITION: it closes WITH entry 1 or immediately after it, never
//	  later. The pull request that wires a real Provisioner must either wire a
//	  PrivilegeApplier in the same change or move this entry to defects() so a
//	  server in that combination does not report ready.
//	WHO CHECKS IT: the reviewer of the pull request that closes entry 1. This
//	  sentence is the instruction to check it.
//
// ---------------------------------------------------------------------------
// 3. api.ProfileReapplier IS UNREACHABLE, WHICH IS ONE STEP BEYOND NIL.
//
//	WHAT: the interface re-applies an agent's granted-profile env/kubeconfig to
//	  a running pod. It has no field of its own: the grant path TYPE-ASSERTS the
//	  Provisioner for it. With a nil Provisioner the assertion can never
//	  succeed, so this is not a dependency that is absent — it is a branch that
//	  cannot be taken.
//	WHY THAT IS WORTH WRITING DOWN SEPARATELY: because it will silently start
//	  working, or silently not, when entry 1 closes. An adapter that does not
//	  implement ReapplyProfiles compiles, wires, and takes the else branch for
//	  ever — with no nil anywhere for a reader to notice. A type assertion is the
//	  one form of optional dependency that leaves no trace when it is not
//	  satisfied.
//	CLOSING CONDITION: the pull request that closes entry 1 states, in its
//	  description, whether its adapter satisfies ProfileReapplier — and if it
//	  does, adds a test that the assertion succeeds. "It compiles" is not that
//	  test; the assertion is about a method set, and a method added to the
//	  wrong type compiles just as well.
//	WHO CHECKS IT: the reviewer of that pull request.
//
// ---------------------------------------------------------------------------
// 4. THE BACKGROUND LOOPS RUN UNGATED: THIS DEPLOYMENT IS SINGLE-REPLICA.
//
//	WHAT: api.Server.SetTaskLeaderGate takes a cross-process singleton gate for
//	  RunTaskRetention and RunTaskReap, and a nil gate means "unguarded". This
//	  binary passes nothing, because internal/db carries no lease — the upstream
//	  implementation is a Postgres session advisory lock that went with the
//	  permission router's half of the loop split.
//	WHY IT IS TOLERATED RATHER THAN FATAL: at replicas: 1 the loops are exactly
//	  correct, and both are idempotent row-level sweeps — a `DELETE … older
//	  than` and a tag-and-comment reap whose store takes `SELECT … FOR UPDATE`
//	  as its own command, which serialises across PROCESSES. So the cost of a
//	  doubled run is duplicated WORK, not duplicated EFFECT. It is not zero.
//	🔴 WHAT IT WOULD COST AT replicas > 1, STATED SO NOBODY SCALES THIS BY
//	  ACCIDENT: every prune and every reap runs once per pod, including across
//	  the overlap window of every rolling update. Deploy this single-replica
//	  until the lease exists.
//	CLOSING CONDITION: a pull request adding a lease to internal/db (a Postgres
//	  session advisory lock is the shape upstream uses) and passing it to
//	  SetTaskLeaderGate here, with a test that two processes cannot both hold
//	  it. Only then may the deployment carry more than one replica.
//	WHO CHECKS IT: the reviewer of that pull request, and whoever writes the
//	  first deployment manifest — the replica count is the thing to look at.
//
// ---------------------------------------------------------------------------
// 5. NOT A SEAM: WHAT IS NIL HERE ON PURPOSE AND CLOSES NOTHING.
//
//   - api.Extensions.GitHub is nil without MUSTER_GITHUB_ENCRYPTION_KEY. There
//     is deliberately no generate-a-key-if-unset branch: an ephemeral key makes
//     every stored token undecryptable after a restart, which presents as "the
//     Repos tab forgot my account" with nothing in the log. Unset means the
//     store is not built, which the Repos tab says on screen.
//   - api.RouterPort and api.GatePort are nil without a router. That IS a
//     supported deployment — see RouterPort's own doc — and the four features
//     that depend on it each degrade where a human can see it. The one bit that
//     must NOT be inferred is session liveness, which is why MUSTER_STANDALONE
//     exists and why /readyz refuses without it. See buildSessionLiveness.
//
// ---------------------------------------------------------------------------
