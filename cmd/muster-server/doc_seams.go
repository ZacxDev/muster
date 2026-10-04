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
//	  NOTHING CALLS THE GATEWAY ON THE DISPATCH PATH. The two CHAT ROUTES stop
//	  REFUSING — which is not the same as reachable, see below; the kickoff is not a
//	  route at all. A dispatch with kickoff=true still CREATES the
//	  instance and does not deliver the first message — the note stays in
//	  agents.pending_note and the non-delivery is written to agents.kickoff_error
//	  (agentprovision.UndeliveredKickoffReason). An agent dispatched on such a
//	  deployment is still a real pod that was never told what to do.
//	🔴 THE PARAGRAPH ABOVE IS NOW TRUE ONLY WHERE MUSTER_AGENT_GATEWAY *NAMES* A
//	  RUNTIME, AND IT IS CORRECTED HERE RATHER THAN REWRITTEN BECAUSE THE SENTENCE
//	  "still CREATES the instance" IS WHAT agentprovision.KickoffRefusalReason CITES
//	  THIS ENTRY FOR. With the variable UNSET — which is every deployment running
//	  today — such a dispatch is now REFUSED instead: agentprovision.Adapter.Dispatch
//	  creates nothing, mints no token, marks the row `error` with the cause and the
//	  remedy, and returns agentprovision.ErrKickoffUndeliverable. buildAgentPlane
//	  passes `gw != nil` into the adapter, so the refusal keys on the RESOLVED
//	  configuration and a deployment that has named a runtime is unchanged.
//	  ⚠ THE REFUSAL IS ON THE CREATE-WITH-KICKOFF PATH ONLY. Start is deliberately
//	  still allowed and still records the non-delivery in agents.kickoff_error, where
//	  it is honest because an instance exists; refusing it would have removed
//	  restart, eviction-recovery and the save-then-start-later route this entry's own
//	  Dispatch doc calls the supported one. See Adapter.Start.
//	  🔴 WHAT IT DOES *NOT* FIX, STATED SO NOBODY READS IT AS CLOSED: the refusal
//	  does not reach the HTTP RESPONSE of the POST that asked. internal/api's
//	  createAndDispatchAgent calls Dispatch inside safeGo AFTER creating the row, so
//	  the operator's POST has already answered 200 and the returned error is logged.
//	  The row's `error` status is the strongest signal reachable from the provisioner.
//	  CLOSING CONDITION: a synchronous check in internal/api's create handler, before
//	  the row exists, keyed on the same capability, answering 4xx with
//	  agentprovision.KickoffRefusalReason's text. WHO CHECKS IT: the reviewer of that
//	  pull request, against this paragraph and against a POST /agents with
//	  action=dispatch on a deployment with no gateway.
//	🔴 AND "A HUMAN CAN NOW OPEN ITS CHAT AND TALK TO IT" WAS FALSE FOR AN AGENT
//	  *THIS BINARY* PROVISIONED — that sentence stood here and an audit measured it.
//	  TWO things block it, and both are OUTSIDE internal/agentgateway:
//
//	  ✅ BOTH ARE CLOSED NOW, AND WHAT REPLACES THEM IS A WEAKER CLAIM RATHER THAN
//	  NOTHING — read to the end of (2) before concluding a turn works. Each is kept
//	  in its original wording with its outcome appended, because both closing
//	  conditions were written here and the only way to check they were MET is to
//	  read what they asked for.
//
//	  (1) NO ADDRESS. [WAS] agentspec.Build renders provision.Spec with Ports nil
//	      and Endpoint nil (its committed golden says so), so k8s render creates NO
//	      Service and driver.Endpoint answers provision.ErrNoEndpoint. Every chat
//	      turn against such an agent fails per-turn — worse than the 503 it replaced,
//	      which at least named its own cause.
//	      CLOSING CONDITION: agentspec.Build declares a port (and the driver renders
//	      a Service), verified by a test that resolves an endpoint through a REAL
//	      driver — a fake clientset is enough — rather than through a stub resolver.
//	      Every test in this module today uses a stub, which is exactly why this
//	      shipped: both sides were tested and the SEAM was not.
//	      ✅ MET. agentspec.Build declares one port named provision.DefaultPortName,
//	      from agentspec.DefaultGatewayPort or MUSTER_AGENT_GATEWAY_PORT, so
//	      renderService produces a Service and renderAnnotations writes the port the
//	      driver resolves from. NO change to internal/provision/k8s was needed — the
//	      driver already rendered container ports, wrote the annotation and read it
//	      back; the whole defect was the spec. The test the condition asked for is
//	      TestAnAgentThisBinaryProvisionsResolvesAnEndpoint, in THIS package rather
//	      than in either package it exercises: internal/agentspec's suite has no
//	      cluster client and internal/provision/k8s's has no agent row, so neither
//	      could hold the combined state. Mutating buildPorts back to returning nil
//	      reproduces the exact string above — `declares no port`.
//	      ⚠ AN ALREADY-PROVISIONED INSTANCE DOES *NOT* GAIN THE PORT BY ITSELF, AND
//	      NO MIGRATION IS OWED — the two halves of that sentence are independent and
//	      both were checked. The mechanism is real: Driver.Endpoint reads the
//	      `muster.dev/port` pod-template annotation written at CREATE time, so an
//	      instance created before this change carries none and resolving its address
//	      would still fail. What makes it moot is that the victim set is EMPTY —
//	      measured live with controls: zero namespaces under the configured prefix
//	      (positive control: the upstream project's own namespaces were found, so the
//	      query worked) and zero objects labelled managed-by muster (positive
//	      control: objects managed by the upstream installer were found). muster has
//	      provisioned nothing, so there is nothing holding a stale annotation, and the
//	      next Start on any row takes the Create path and gets the port. Written down
//	      so the next reader does not re-derive the mechanism and conclude a migration
//	      is needed; if this binary ever provisions before a change of this shape, it
//	      will be.
//	  (2) NO CREDENTIAL UNDER THE NAME THE IMAGE READS. [WAS] The gateway bearer is
//	      sha256("gw-" + HOOKS_TOKEN) — the agent CONTAINER's variable — while
//	      agentspec ships the row's token as MUSTER_HOOK_TOKEN (agentspec.EnvToken)
//	      and nothing bridges the two. Measured against a live runtime provisioned by
//	      the upstream service, which ships HOOKS_TOKEN: that derivation is accepted
//	      (200, and a deliberately wrong bearer is refused 401). So an agent THIS
//	      binary provisions would 401 on every turn — 🔴 ONCE (1) IS FIXED, AND NOT
//	      BEFORE. The two blockers are ORDERED: Gateway.reach resolves the endpoint
//	      before it builds a request, so while (1) holds the observable is
//	      ErrNoEndpoint and NO 401 is reachable. An earlier revision of this line
//	      predicted the 401 unconditionally, which sends a debugger hunting a
//	      credential when the first failure is address resolution.
//	      CLOSING CONDITION: the provisioned container receives the token under the
//	      name its gateway derives from, proven by one real turn against an instance
//	      THIS binary created — not one created by the upstream service.
//	      ✅ THE FIRST HALF IS MET AND 🔴 THE PROOF IS NOT. agentspec.buildSecrets
//	      now emits the row's token under agentspec.EnvGatewayToken as well as
//	      EnvToken, from ONE branch so they cannot drift apart — a SECOND NAME and
//	      not a rename, because EnvToken is what cmd/muster's in-pod CLI reads and
//	      what the autosave daemon posts its durability alarm with, so renaming
//	      would have traded a chat 401 for a silent agent.
//	      TestTheProvisionedContainerCanDeriveTheBearerMusterSends reproduces the
//	      container's documented derivation over the built spec's own environment and
//	      requires the two bearers to match; it dies to a wrong name, a wrong value
//	      and an empty value, each of which has the same runtime symptom.
//	      🔴 WHAT IS STILL OWED IS THE SENTENCE THIS CONDITION ACTUALLY WROTE:
//	      "ONE REAL TURN AGAINST AN INSTANCE THIS BINARY CREATED". No such turn has
//	      been made. Two tests agreeing about bytes is not a runtime accepting them,
//	      the container half of the derivation is a shell command in the agent
//	      image's own deployment that nothing here can read, and a fake clientset
//	      resolves no DNS and runs no pod. So this entry stays OPEN on its evidence
//	      while being closed on its mechanism, and the boot banner says
//	      "NOT VERIFIED REACHABLE" for exactly that reason.
//	      WHO CHECKS IT: whoever runs the Makefile's test-liveenv target against a
//	      muster-provisioned instance — internal/agentgateway/liveruntime_test.go
//	      already owns that measurement and names the variables it needs.
//	  WHO CHECKED BOTH MECHANISMS: the reviewer of the pull request that declared the
//	      port and the second token name, against the two tests named above and
//	      against the mutation results in its own description. Neither blocker was a
//	      defect in the chat transport, and neither was introduced by the change that
//	      wired it.
//	  🔴 AND NOTHING ESCALATES THAT YET. agents.DecideReconcile already has the
//	  decision table (ActionRetryKickoff, then ActionError past
//	  ProvisioningStuckTimeout) and its own header records that NO loop drives it,
//	  so the undelivered kickoff does not become a red card on its own. Until a
//	  kickoff delivery or that loop lands, the kickoff-error field is the only place
//	  that says so.
//	CLOSING CONDITION FOR THE REMAINDER: a pull request in which a dispatch with
//	  kickoff=true delivers its note through api.Extensions.Gateway — or the
//	  reconcile loop that escalates a missing one. ⚠ THE CALL SITE IS ONE OF THREE
//	  BLOCKERS, NOT THE ONLY ONE — an earlier revision of this line said "the
//	  transport and the wiring are no longer the blocker; the CALL SITE is", which
//	  named one and hid the two above it. A kickoff delivered through a gateway that
//	  can resolve no address, over a credential the container never received, is not
//	  a delivered kickoff.
//	  ⚠ IT IS THE LAST OF THE THREE NOW, AND THAT CHANGES WHAT A CALL SITE WOULD
//	  ACHIEVE WITHOUT CHANGING THE CONDITION. Both blockers above are closed on their
//	  mechanism, so a dispatch call site is no longer guaranteed to deliver nothing —
//	  but "no longer guaranteed to fail" is not "delivers", and the sentence above
//	  stays because it is the reason this condition was written as a DELIVERY rather
//	  than as a call. What a call site would now produce is the first real turn
//	  against an instance this binary created, which is also the evidence blocker (2)
//	  is still owed; whoever writes it gets both.
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
// 2. ✅ api.PrivilegeApplier HAS AN IMPLEMENTATION — internal/agentprivilege —
//    AND THIS SEAM IS CLOSED. THE READINESS DEFECT STAYS, BECAUSE IT IS ABOUT
//    WHETHER THE TIER IS *ARMED*, NOT ABOUT WHETHER ONE EXISTS.
//
//	🔴 THIS ENTRY REVERSES A DECISION, AND THAT IS RECORDED HERE RATHER THAN
//	  QUIETLY OVERWRITTEN, BECAUSE THE PROSE BELOW ARGUES THE OPPOSITE CASE AT
//	  LENGTH AND A READER WHO STOPS AT IT WILL CONCLUDE THE WRONG THING.
//	  The operator's earlier standing decision (2026-09-25) was that the privilege
//	  domain stays WHOLE in the permission router — route, store AND the applier —
//	  so muster's privilege store was deliberately EMPTY: dark, not stale. The
//	  operator's later decision (2026-09-28) was to wire the applier in muster
//	  instead, with the contradiction pointed out. What follows below is the
//	  argument as it stood under the first of the two.
//	WHAT LANDED: internal/agentprivilege adapts provision.Provisioner to
//	  api.PrivilegeApplier — two methods, translating a privilege.Profile into a
//	  provision.Policy and delegating to provision.Grant / provision.Revoke. It
//	  IMPLEMENTS NO RBAC OF ITS OWN: internal/provision/k8s already renders every
//	  object, with the (instance, policy) digest name, the ownership predicate on
//	  every read/write/delete, and the strict rules decode. A second applier beside
//	  those would have been wrong at each of those points in the same direction.
//	  provisioner.go builds it over the SAME driver instance the lifecycle and chat
//	  tiers hold, behind MUSTER_AGENT_PRIVILEGE_APPLY.
//	🔴 THE DEFECT IN api.Extensions.defects IS UNCHANGED, AND THAT IS NOT AN
//	  OVERSIGHT. Its three conjuncts ask whether THIS SERVER can apply what it
//	  records; an unarmed applier is exactly as unable as an absent one, so the
//	  refusal must still fire. What changed is that the refusal now names a
//	  variable an operator can set, instead of naming a dead end. A readiness
//	  refusal with no escape is the shape that trains people to delete the probe.
//	⚠ THE CONSEQUENCE, STATED PLAINLY BECAUSE IT IS A BLOCKER SOMEONE WILL MEET:
//	  this deployment builds a privilege store whenever it has a database, so
//	  setting MUSTER_AGENT_PROVISIONER on it makes the pod UNREADY until
//	  MUSTER_AGENT_PRIVILEGE_APPLY is also set. That is fail-closed and deliberate.
//	  🔴 AND IT RUNS THE OTHER WAY TOO, WHICH IS THE HALF NOBODY WROTE DOWN UNTIL
//	  THE ROUND-1 AUDIT: once armed, taking MUSTER_AGENT_PRIVILEGE_APPLY away is an
//	  OUTAGE, not a rollback — the pod re-enters this defect and is pulled from its
//	  Service, taking the task board, notes, repos and runbooks with it. The escape
//	  is a two-variable change (this one and the provisioner, which
//	  config.validateProvisioner requires be removed together) or an image
//	  rollback. provisioner.go prerequisite 2 carries the full statement.
//	🔴 WHY THE TIER IS OFF BY DEFAULT, WHICH IS A DIFFERENT ARGUMENT FROM THE OTHER
//	  TWO KNOBS' DEFAULTS: applying a grant writes ClusterRoles, Roles and both
//	  kinds of binding, and needs the rbac `escalate` and `bind` verbs on top of
//	  those ordinary writes (they are what the apiserver asks when the rules being
//	  written exceed what muster itself holds). Nothing in this module can check for
//	  any of it, the last two are what a cluster administrator grants last, and
//	  without the set every grant fails at apply time with a 403. If naming a driver
//	  implied this tier, the first image bump that set MUSTER_AGENT_PROVISIONER
//	  would start attempting privileged writes.
//	  ⚠ THE SET IS NOT LISTED HERE. This entry used to name `escalate` and `bind`
//	  and nothing else — as did three other files — and that list is INCOMPLETE:
//	  it omits every ordinary verb the policy path calls, so a Role written from it
//	  403s on the first grant. It is enumerated once, as data, in
//	  internal/provision/k8s.PolicyRBACPrerequisite, 18 of whose 22 (resource, verb)
//	  pairs are DERIVED from the call sites and guarded against them by
//	  TestTheRBACPrerequisiteMatchesThePolicyCallSites.
//	  ⚠ THE OTHER FOUR ARE ASSERTED, NOT DERIVED: `escalate` and `bind` on `roles`
//	  and on `clusterroles` have no call site by construction — they are
//	  authorisation checks the apiserver layers on top of an ordinary write — so the
//	  DERIVATION excludes them, while the same guard pins all four EXPLICITLY and
//	  pins their ABSENCE on the two binding resources; what nothing in this module
//	  can redden is whether the apiserver really asks them.
//	IT IS LISTED IN defects() RATHER THAN WRAPPED AT A ROUTE, unlike the nil
//	  Provisioner, because there is no single route to refuse: the falsehood is
//	  rendered by every surface that shows a grant, and a grant recorded through
//	  one route is read back through several.
//	WHO CHECKS IT: TestAWiredProvisionerWithNoPrivilegeApplierIsNotReady, which
//	  drives /readyz rather than calling defects() — a defect list nothing reads
//	  is not a guard — plus TestThePrivilegeApplierSatisfiesTheConsumerInterface
//	  (the real type, not a stub) and TestEverySitePopulatingPrivilegeApplyIsOnTheLedger.
//	🔴 WHAT THIS DOES *NOT* CLOSE, AND IT IS OWED TO A DIFFERENT PACKAGE:
//	  internal/agentspec's instructions.go DROPPED the agent-facing "request
//	  elevated access" section, and its header states the condition for bringing it
//	  back — "When a privilege applier lands in muster, this section comes back".
//	  That condition has now fallen due. It is NOT done in the change that wrote
//	  this paragraph: internal/agentspec was concurrently owned by another change.
//	  CLOSING CONDITION: a pull request restoring that section to
//	  agentspec.WorkerInstructions, gated on the tier being armed or written so an
//	  agent on an unarmed deployment is not told to ask.
//	  WHO CHECKS IT: the reviewer of that pull request, against instructions.go's
//	  own header.
//
//	The original argument follows. Its first half is still correct for every
//	  deployment that wires no provisioner; its second half — "nothing implements
//	  it here" and the closing condition — is SUPERSEDED, and is kept because it is
//	  what the wrappers, the banner and the readiness defect were built on.
//
//	WHAT: api.Extensions.PrivilegeApply applies a granted profile's Kubernetes
//	  RBAC to an agent's ServiceAccount, live. ⚠ "Nothing implements it here, for
//	  the same reason as entry 1 — it needs the in-cluster client and the
//	  SA/namespace naming that live with the provisioner" was the next sentence,
//	  and it is now FALSE IN ITS CONCLUSION AND RIGHT IN ITS PREMISE:
//	  internal/agentprivilege implements it precisely BY not holding a client of
//	  its own — it delegates to the driver that already holds one and already knows
//	  the namespace, which is the thing this sentence identified as the obstacle.
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
//	CLOSING CONDITION (BOTH BRANCHES NOW TAKEN, IN THAT ORDER): "it closes WITH
//	  entry 1 or immediately after it, never later. The pull request that wires a
//	  real Provisioner must either wire a PrivilegeApplier in the same change or
//	  move this entry to defects() so a server in that combination does not report
//	  ready." The provisioner PR took the second branch; the change above took the
//	  first, three days later. "Never later" was not honoured and the cost is
//	  written down in the ⚠ CONSEQUENCE line at the top of this entry: for those
//	  three days, setting MUSTER_AGENT_PROVISIONER on a deployment with a database
//	  made the pod unready with no way out but unsetting something.
//	WHO CHECKED IT: the reviewer of the pull request that closes entry 1 — and then
//	  the reviewer of the pull request that wired the applier.
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
