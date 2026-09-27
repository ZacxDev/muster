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
// 1. 🔴 api.Provisioner IS NIL *UNLESS* MUSTER_AGENT_PROVISIONER NAMES A DRIVER,
//    AND api.Gateway IS NIL UNLESS MUSTER_AGENT_GATEWAY NAMES A RUNTIME. BOTH
//    HALVES OF THIS SEAM NOW HAVE AN IMPLEMENTATION; NEITHER IS ON BY DEFAULT.
//
//	WHAT CHANGED, AND WHAT ITS MECHANICAL SIGNAL WAS: internal/agentprovision
//	  adapts provision.Provisioner to api.Provisioner's seven LIFECYCLE methods,
//	  and provisioner.go constructs a driver behind MUSTER_AGENT_PROVISIONER
//	  (none | noop | kubernetes). The closing condition this entry set was that
//	  internal/provision/k8s LEAVE the not-linked ledger in
//	  internal/modulegate/linkage_test.go — it has, together with
//	  internal/agentspec, and that ledger is ASSERTED so neither could be removed
//	  from it without something really linking.
//	✅ THE CHAT HALF NOW HAS AN IMPLEMENTATION, AND THE PARAGRAPH HERE THAT SAID
//	  OTHERWISE IS REPLACED RATHER THAN LEFT TO READ AS OPEN. It said: "WHAT IS
//	  STILL NIL, AND IT IS NOT A DETAIL: api.Gateway. Nothing in this module
//	  implements a model gateway". internal/agentgateway does, over the SAME driver
//	  the lifecycle adapter holds, behind MUSTER_AGENT_GATEWAY (none | hooks-sha256)
//	  plus MUSTER_AGENT_GATEWAY_MODEL. Naming a runtime with no driver is refused at
//	  boot, because this binary's only source of an instance's address is a driver.
//	🔴 WHAT THAT DOES *NOT* CLOSE, AND IT IS THE HALF THAT MATTERS FOR A KICKOFF:
//	  NOTHING CALLS THE GATEWAY ON THE DISPATCH PATH. The two CHAT ROUTES are live;
//	  the kickoff is not a route. A dispatch with kickoff=true still CREATES the
//	  instance and does not deliver the first message — the note stays in
//	  agents.pending_note and the non-delivery is written to agents.kickoff_error
//	  (agentprovision.UndeliveredKickoffReason). An agent dispatched on such a
//	  deployment is still a real pod that was never told what to do; what changed is
//	  that a human can now open its chat and talk to it.
//	  🔴 AND NOTHING ESCALATES THAT YET. agents.DecideReconcile already has the
//	  decision table (ActionRetryKickoff, then ActionError past
//	  ProvisioningStuckTimeout) and its own header records that NO loop drives it,
//	  so the undelivered kickoff does not become a red card on its own. Until a
//	  kickoff delivery or that loop lands, the kickoff-error field is the only place
//	  that says so.
//	CLOSING CONDITION FOR THE REMAINDER: a pull request in which a dispatch with
//	  kickoff=true delivers its note through api.Extensions.Gateway — or the
//	  reconcile loop that escalates a missing one. The transport and the wiring are
//	  no longer the blocker; the CALL SITE is.
//	WHO CHECKS IT: the reviewer of that pull request, against agents.reconcile.go's
//	  header and against a dispatched agent's kickoff_error being empty.
//
//	The original entry follows, kept rather than rewritten because its argument is
//	what the wrappers, the banner and the readiness defect are all still built on.
//
// ---------------------------------------------------------------------------
// 1b. 🔴 THE ORIGINAL ENTRY 1: WHY A FAKE WAS REFUSED WHILE THIS WAS NIL.
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
//	  api.requireLifecycleProvisioner / api.requireGatewayProvisioner wrapper it is
//	  registered behind. The Agents tab is
//	  read-only: existing rows render, nothing can be provisioned, started,
//	  stopped or destroyed, no logs stream and no chat turn runs. The boot banner
//	  says so on every start, unconditionally.
//	  🔴 THE ROUTE GOLDEN DOES NOT MOVE FOR THIS. If it ever does, the nil has
//	  started changing the route set, which is a defect in its own right.
//	  each is a WRAPPER for exactly that reason — the golden records
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
//	CLOSING CONDITION (MET for the lifecycle half — see entry 1 above): a pull
//	  request adding an adapter from provision.Driver to api.Provisioner — in its
//	  own package, consumer-side interfaces unchanged — wired here behind the
//	  configuration that names a driver, with
//	  internal/provision/provisiontest's contract suite green against it. When
//	  it lands, internal/provision/k8s LEAVES the not-linked ledger in
//	  internal/modulegate/linkage_test.go, and that departure is the mechanical
//	  signal this seam closed.
//	WHO CHECKS IT: the reviewer of that pull request, against the provisiontest
//	  contract results and the linkage ledger.
//
// ---------------------------------------------------------------------------
// 2. 🔴 api.PrivilegeApplier IS NIL — AND WITH A PROVISIONER WIRED THAT IS NOW A
//    READINESS DEFECT RATHER THAN A CONFIGURATION.
//
//	🔴 THE PARAGRAPHS BELOW NAMED THE EXACT MOMENT THIS WOULD HAPPEN AND IT HAS
//	  HAPPENED. "That argument dies the moment entry 1 closes." Entry 1's
//	  lifecycle half is closed, so the combination Provisioner != nil &&
//	  Privilege != nil && PrivilegeApply == nil is listed in
//	  api.Extensions.defects and /readyz REFUSES it. This entry's own closing
//	  condition offered exactly two ways out — wire an applier in the same change,
//	  or move the entry to defects() — and the second was taken.
//	⚠ THE CONSEQUENCE, STATED PLAINLY BECAUSE IT IS A BLOCKER SOMEONE WILL MEET:
//	  this deployment builds a privilege store whenever it has a database, so
//	  setting MUSTER_AGENT_PROVISIONER on it makes the pod UNREADY until a
//	  PrivilegeApplier exists. That is fail-closed and deliberate; it is also a
//	  prerequisite for proving a real dispatch end to end, which no earlier plan
//	  step named.
//	IT IS LISTED IN defects() RATHER THAN WRAPPED AT A ROUTE, unlike the nil
//	  Provisioner, because there is no single route to refuse: the falsehood is
//	  rendered by every surface that shows a grant, and a grant recorded through
//	  one route is read back through several.
//	WHO CHECKS IT: TestAWiredProvisionerWithNoPrivilegeApplierIsNotReady, which
//	  drives /readyz rather than calling defects() — a defect list nothing reads
//	  is not a guard.
//
//	The original argument follows, because it is still correct for every
//	  deployment that wires no provisioner.
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
// 3. ✅ api.ProfileReapplier IS SATISFIED, AND THE ASSERTION IS ASSERTED.
//
//	THE ANSWER THIS ENTRY DEMANDED, IN ONE LINE: *agentprovision.Adapter DOES
//	  implement ReapplyProfiles — it re-renders the agent's spec from its current
//	  row and calls the driver's Update, which reconciles the live instance and
//	  rolls it. TestTheLifecycleAdapterSatisfiesTheConsumerInterface performs the
//	  type assertion, and
//	  TestReapplyProfilesReconcilesRatherThanRecreates asserts the driver call
//	  rather than a nil return — because a body that returned nil and did nothing
//	  would satisfy the assertion, compile, wire, and make every model change and
//	  every privilege re-apply a silent no-op.
//	⚠ ONE LATENT CONSEQUENCE, INTENDED: rolling an instance re-delivers its
//	  kickoff once a gateway exists, which agents.kickoffLost's own doc calls
//	  intended and agents.MaxKickoffAttempts caps. Nothing delivers a kickoff
//	  today, so it is dormant.
//
//	The original entry follows; its reasoning about type assertions is what the
//	  new test exists to answer, and it is worth keeping for the next optional
//	  dependency.
//
// ---------------------------------------------------------------------------
// 3b. api.ProfileReapplier WAS UNREACHABLE, WHICH IS ONE STEP BEYOND NIL.
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
