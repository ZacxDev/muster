// The e2e suite's fixed inputs, spelled once.
//
// Every value here is invented for the suite: the password and token guard a
// throwaway server on localhost, and the database name must end in _e2e (the
// seed refuses anything else — it truncates what it seeds).
import path from 'node:path';

export const repoRoot = path.resolve(__dirname, '..', '..');
export const e2eRoot = path.resolve(__dirname, '..');
export const binDir = path.join(e2eRoot, '.bin');
export const stateDir = path.join(e2eRoot, '.state');
export const authState = path.join(stateDir, 'auth.json');

export const dsn = process.env.MUSTER_E2E_DATABASE_URL ?? '';
// 🔴 With this set, a missing database FAILS the run instead of skipping it. A
// skip is invisible in a pass/fail verdict; CI sets it, and the verdict step
// also caps the skip count (e2e/verdict.mjs).
export const requireFull = process.env.MUSTER_E2E_REQUIRE_FULL === '1';

export const password = 'e2e-operator-password-not-a-real-one';
export const hookToken = 'e2e-hook-token-not-a-real-one';

// The shared server every spec but the update spec uses, and the version it
// is built with. The update spec runs its own pair on its own port, because a
// service worker is per ORIGIN and a fresh port is a fresh origin.
export const port = Number(process.env.MUSTER_E2E_PORT ?? 18181);
export const updatePort = port + 1;
// The agent-kinds spec's own server (claude-code enabled over the noop driver).
export const kindsPort = port + 2;
// The terminal spec's own server: a binary built with `-tags e2eterm`, whose
// noop driver plays a terminal (cmd/muster-server/e2eterm.go).
export const termPort = port + 3;
// The UI-polish spec's own server (kinds enabled over the noop driver).
export const polishPort = port + 4;
// The provisioning-indicator spec's own server: the e2eterm binary with its
// readiness hold armed (MUSTER_E2E_HOLD_READY_DIR, cmd/muster-server/e2eterm.go).
export const provPort = port + 5;
// The connector spec's own server (the e2eterm binary, whose connector proxy may
// reach a FAKE upstream on loopback), its dedicated connect listener, and the
// fake upstream itself.
export const connectorsPort = port + 6;
export const connectPort = port + 7;
export const fakeUpstreamPort = port + 8;
export const mirrorPort = port + 9;
// accounts PR 4: a gotrue-mode server (machine port + browser listener) and
// the real GoTrue it signs in against (e2e/gotrue/compose.yml).
export const gotrueMainPort = port + 10;
export const gotrueBrowserPort = port + 11;
export const gotruePort = Number(process.env.MUSTER_E2E_GOTRUE_PORT ?? port + 12);
export const gotrueSecret = 'e2e-gotrue-jwt-secret-not-a-real-one-0123456789';
// accounts PR 5: the invites spec's own gotrue-mode server and real GoTrue, so
// it never shares a GoTrue database (or a claimed admin) with gotrue-login.
export const invitesMainPort = port + 13;
export const invitesBrowserPort = port + 14;
export const invitesGotruePort = Number(process.env.MUSTER_E2E_INVITES_GOTRUE_PORT ?? port + 15);
// The terminal-overflow spec's own e2eterm server (its fake pod reports a
// session window: MUSTER_E2E_TERM_WINDOW).
export const overflowPort = port + 16;
// The mirror step-up spec: a gotrue-mode server with the mirror plugin on
// (machine port + browser listener) and its own real GoTrue.
export const stepupMainPort = port + 17;
export const stepupBrowserPort = port + 18;
export const stepupGotruePort = Number(process.env.MUSTER_E2E_STEPUP_GOTRUE_PORT ?? port + 19);
// Passkeys: their own gotrue-mode server and real GoTrue (passkeys ON, RP
// localhost, origin = this browser port), for the same reason as invites.
export const passkeysMainPort = port + 20;
export const passkeysBrowserPort = port + 21;
export const passkeysGotruePort = Number(process.env.MUSTER_E2E_PASSKEYS_GOTRUE_PORT ?? port + 22);
// muster-native Web Push: the push spec's own server, with a VAPID keypair
// minted by the binary under test (`muster-server vapid generate`).
export const pushPort = port + 23;
// Per-window mirrors: the mirror-windows spec's own server (the e2eterm binary
// with the mirror plugin on), so its host and its cards are no other spec's.
export const windowsPort = port + 24;
// The agent view-switch and composer spec's own e2eterm server (kinds enabled).
export const agentComposerPort = port + 25;
// Dictation: the dictation spec's own server (the e2eterm binary, mirror plugin
// on, kinds enabled, MUSTER_STT_* pointing at a fake speech-to-text service),
// and that fake service.
export const dictationPort = port + 26;
export const fakeSttPort = port + 27;
// Duplicate mirror labels: the spec's own server (the e2eterm binary with the
// mirror plugin on), so only ITS two hosts are listed — the clash it draws is
// between them and no other spec's host.
export const dupLabelsPort = port + 28;
// Plugins: a gotrue-mode server with MUSTER_PLUGINS=1 and the connector store
// (for /mcp, on its own connect port), and its own real GoTrue with passkeys on —
// so no other spec's server ever has plugins on.
export const pluginsMainPort = port + 29;
export const pluginsBrowserPort = port + 30;
export const pluginsGotruePort = Number(process.env.MUSTER_E2E_PLUGINS_GOTRUE_PORT ?? port + 31);
export const pluginsConnectPort = port + 32;
// Pins: the pins spec's own server (the e2eterm binary with the mirror plugin
// on), so its mirrored host and its pins' order are no other spec's.
export const pinsPort = port + 36;
// The mirrored chat spec's own server (the e2eterm binary with the mirror plugin
// on): notices, bubble clamps, the message index and context usage.
export const mirrorChatPort = port + 50;
// Layout shift (cls.spec.ts): the FULL server (every optional store built) and
// its connector listener, and a gotrue-mode server's two listeners for the
// public sign-in pages.
export const clsFullPort = port + 37;
export const clsConnectPort = port + 46;
export const clsGotrueMainPort = port + 43;
export const clsGotrueBrowserPort = port + 44;
// …and the signed-in pages (cls.spec.ts): a gotrue-mode server (passkeys and
// plugins on) with its own real GoTrue and its connector listener.
export const clsSignedMainPort = port + 51;
export const clsSignedBrowserPort = port + 52;
export const clsSignedGotruePort = Number(process.env.MUSTER_E2E_CLS_GOTRUE_PORT ?? port + 53);
export const clsSignedConnectPort = port + 54;
// The composer-autosize, tool-group and agents-hold specs' own servers (each
// the e2eterm binary with the mirror plugin on and a fake mirrored host).
export const composerFitPort = port + 38;
export const toolGroupsPort = port + 39;
export const agentsHoldPort = port + 47;
// The pickers spec's own e2eterm server, and its fake upstreams (GitHub,
// OpenRouter's model list, a permission router) on one loopback port.
export const pickersPort = port + 48;
export const pickersFakePort = port + 49;
// …and its connect listener: the GitHub App the repo picker reads needs the
// connector store, which refuses to boot without one.
export const pickersConnectPort = port + 247;
// The send queue: its own server (the e2eterm binary, mirror plugin on, a short
// MUSTER_SEND_QUEUE_IDLE) and its two mirrored hosts — one that reports Claude
// Code's status and one that plays an older muster-mirror.
export const sendQueuePort = port + 45;
// Session focus (focus.spec.ts): its own server (the e2eterm binary, mirror
// plugin on) and one mirrored host whose Claude window reports a session id.
export const focusPort = port + 55;
// Session routing (session-routing.spec.ts): its own server (plugins, mirror
// plugin and routing on) and its connect listener (the chief's /mcp door).
export const routingPort = port + 56;
export const routingConnectPort = port + 57;
// The Rendered view composer's split Send/Queue button and its `/` and `:`
// menu (composer-menu.spec.ts): its own server (the e2eterm binary, mirror
// plugin on) and two mirrored hosts — one that reports its catalog and one
// that plays an older muster-mirror.
export const composerMenuPort = port + 58;
// The attention queue (attention.spec.ts): its own server (the e2eterm binary,
// mirror plugin on) with its connect listener (the asking agent's /mcp door) and
// one mirrored host whose Claude window is waiting on the person.
export const attentionPort = port + 62;
export const attentionConnectPort = port + 63;
// File attachments (uploads.spec.ts): the server with S3 configured, the one
// without, and the fake S3. Far from the others so a sibling spec added in the
// meantime is unlikely to land on them.
export const uploadsPort = port + 70;
export const uploadsOffPort = port + 71;
export const fakeS3Port = port + 72;
// The Agents list's host tabs (host-tabs.spec.ts): its own server (the
// e2eterm binary, mirror plugin on) and up to three mirrored hosts. A high
// offset, clear of the specs being added beside it.
export const hostTabsPort = port + 64;
// AskUserQuestion answered from the Rendered view (mirror-ask.spec.ts): its own
// server (the e2eterm binary, mirror plugin on) and one interactive mirrored
// host, with `muster-mirror ask` run as the hook.
export const mirrorAskPort = port + 80;
// …and the mirrored host's own loopback MCP endpoint (muster-mirror -mcp-listen).
export const attentionMirrorMCPPort = port + 65;
// The fake service's bearer token. Invented, like every value here.
export const sttToken = 'e2e-stt-token-not-a-real-one';
// The docs screenshots (e2e/docshots, `make screenshots`, never in the suite):
// a gotrue-mode server with the mirror plugin on, and its own real GoTrue.
export const docsMainPort = port + 40;
export const docsBrowserPort = port + 41;
export const docsGotruePort = Number(process.env.MUSTER_E2E_DOCS_GOTRUE_PORT ?? port + 42);
// The seeded `docs-sweeper` agent's own token (e2e/seed). Invented, like every
// value here.
export const agentToken = 'e2e-agent-token-not-a-real-one';
// The chief's sessions on more than one runtime (chief-runtimes.spec.ts): its
// own server (the e2eterm binary, both kinds over the noop driver). port + 91
// was its fake OpenRouter model list, gone with the gateway session runtime.
export const chiefRuntimesPort = port + 90;
// An answer delivered to the chief (attention-delivery.spec.ts): its own server
// (the e2eterm binary, the agent gateway on, every agent endpoint pointed at the
// spec's fake runtime), its /mcp door, and that fake runtime.
export const attentionDeliveryPort = port + 96;
export const attentionDeliveryConnectPort = port + 97;
export const attentionDeliveryFakePort = port + 98;
// The Agents list's cards (agent-cards.spec.ts): its own server (the e2eterm
// binary, mirror plugin on), its connect listener (the asking agent's /mcp
// door), and one mirrored host with a Claude window and two other windows.
export const cardsPort = port + 100;
export const cardsConnectPort = port + 101;
// Host icons and the Agents list's scroll positions (host-icons.spec.ts): its
// own server (the e2eterm binary, mirror plugin on) and two mirrored hosts.
export const hostIconsPort = port + 110;
// port + 120 was a since-removed spec's: left unused,
// so no other spec's ports move.
// The Agents list UX batch (agents-list-ux.spec.ts): its own server (the
// e2eterm binary, mirror plugin on) and one mirrored host with four windows.
export const listUxPort = port + 137;
// Live work counts and identity colours on mirrored cards
// (agent-cards-work.spec.ts): its own server (mirror plugin on), one mirrored
// host with four windows and an OLDER host that sends neither message.
export const workCardsPort = port + 145;
// A mirrored window's Rendered | Terminal switch keeping both views' host
// streams (view-swap.spec.ts): its own server (mirror plugin on) and one
// per-window mirrored host with two Claude Code windows.
export const viewSwapPort = port + 165;
// The long Agents list's layout shift (cls.spec.ts "a long Agents list"): its
// own server (the e2eterm binary, mirror plugin on) and one mirrored host with
// sixty-odd windows. Clear of the offsets above and of the specs beside them.
export const clsListPort = port + 170;
// The Agents list's Idle section (agents-idle.spec.ts): its own server (the
// e2eterm binary, mirror plugin on) and one mirrored host.
export const agentsIdlePort = port + 175;
// Host tab order, named-host icons and the host chip (host-names.spec.ts): its
// own server (the e2eterm binary, mirror plugin on) and four mirrored hosts.
export const hostNamesPort = port + 180;
// The side nav on every signed-in page (sidenav.spec.ts): its own server (the
// e2eterm binary, mirror and plugins on) and one per-window mirrored host.
export const sidenavPort = port + 185;
// The rendered view's snapshot (mirror-snapshot.spec.ts): its own server (the
// e2eterm binary, mirror plugin on) and one per-window mirrored host.
export const mirrorSnapPort = port + 190;
// The back/forward cache control (bfcache.spec.ts): a two-page static site the
// spec serves itself, so whether a page is restored depends on the browser and
// the harness alone, never on muster's headers.
export const bfcacheControlPort = port + 194;
// The long-transcript layout shift on a mirrored view (cls-long-transcript.spec.ts):
// its own server (the e2eterm binary, mirror plugin on) and one mirrored host.
export const clsLongMirrorPort = port + 196;
// Suggested replies' chip row (suggested-replies.spec.ts): its own server
// (the e2eterm binary, mirror plugin on, the model pointed at the spec's fake)
// and that fake chat-completions endpoint.
export const suggestPort = port + 197;
export const suggestFakePort = port + 198;
// Room for a title (title-room.spec.ts): the Rendered | Terminal switch's
// padding, the rendered view's full-title popover, and the width of the
// switcher's entries. Its own server (the e2eterm binary,
// mirror plugin on) and one per-window mirrored host with long session names.
export const titleRoomPort = port + 205;
// An opencode window's read-only rendered view (opencode-view.spec.ts): its own
// server (the e2eterm binary, mirror plugin on) and one mirrored host whose
// opencode server is a fake in the host's own process.
export const openCodeViewPort = port + 199;
// Back and Forward restoring the view pages (bfcache-restore.spec.ts): its own
// mirrored server (the e2eterm binary, mirror plugin on) and one mirrored host,
// and a port of its own for the build A / build B pair (a deploy while a page
// sits in the back/forward cache).
export const bfcacheRestorePort = port + 150;
export const bfcacheBuildPort = port + 151;
// …and a static page of its own on another origin, to leave a page for when
// the muster page left behind must not answer anything meanwhile.
export const bfcacheAwayPort = port + 152;
// Telemetry (telemetry.spec.ts): its own server (the e2eterm binary, mirror
// plugin on) with tracing and the Faro relay pointed at the spec's own fake
// receivers, which listen on ports the OS picks.
export const telemetryPort = port + 210;
// …and its metrics listener (MUSTER_METRICS_PORT): /metrics is on no other port.
export const telemetryMetricsPort = port + 211;
// How long opening a mirrored view takes in the browser (open-perf.spec.ts):
// its own server (the e2eterm binary, mirror plugin on, Faro relayed to the
// spec's own fake receiver) and one mirrored host with a long transcript.
export const openPerfPort = port + 215;
// The session switcher's view menu and search palette (switcher-search.spec.ts):
// its own server (the e2eterm binary, mirror plugin on) and one mirrored host
// whose windows carry codenames.
export const switcherSearchPort = port + 220;
// The session switcher docked on the Agents list (agents-switcher.spec.ts): its
// own server (the e2eterm binary, mirror plugin on) and one mirrored host.
export const agentsSwitcherPort = port + 225;
// tests/reconnect.spec.ts: a mirror rig whose server beats every 500 ms
// (MUSTER_LIVE_HEARTBEAT), so a silent socket is judged stale in about a second.
export const reconnectPort = port + 230;
// The tool rows' context badges on the mirrored view (tool-context.spec.ts):
// its own server (the e2eterm binary, mirror plugin on) and one mirrored host.
export const toolContextPort = port + 235;
// tests/ask-page.spec.ts: the ask page a notification opens — its own server
// (the e2eterm binary, its suggestion model pointed at a FAKE this spec runs),
// the agent connect door its asker uses, and that fake.
export const askPagePort = port + 243;
export const askPageConnectPort = port + 244;
export const askPageFakePort = port + 245;
// The first-run empty state on a fresh database (onboarding.spec.ts): its own
// server (versionA) on a database e2e/emptydb creates.
export const onboardingPort = port + 240;
// tests/layout.spec.ts's dispatch-sheet checks: a server WITH an agent
// provisioner (noop). The shared server has none, like the docker compose
// install, so it offers no dispatch sheet to measure.
export const layoutDispatchPort = port + 250;
// tests/agent-page-actions.spec.ts: where the retired Agents list's actions
// went — a server WITH an agent provisioner (noop), so Start, Stop, Delete and
// the sidebar's dispatch "+" are offered.
export const agentActionsPort = port + 251;
// tests/switcher-dock.spec.ts: the switcher dock — its own server (a mirror rig
// that can dispatch a claude-code agent, for the agent page's Terminal frame)
// and one mirrored host with enough windows to fill the dock's Recent tab.
export const dockPort = port + 255;
// The feature registry spec (features.spec.ts): its own server with a mirrored
// host and no dictation, attachments or connectors, so their hints show.
export const featuresPort = port + 256;
// tests/chief-mobile.spec.ts: a claude-code agent's page on a phone — its own
// server with the claude-code kind on (noop driver), so the header draws the
// account and the failover marker.
export const chiefMobilePort = port + 260;
// tests/optimistic-send.spec.ts: a sent message shows at once as pending — its
// own server (mirror plugin on, a 1 s send-queue idle) and its own fake host.
export const optimisticSendPort = port + 261;
// tests/session-tags.spec.ts: session tags and the Rendered view's collapsible
// box — its own server (mirror plugin on) and its own fake host.
export const sessionTagsPort = port + 270;
// tests/opencode-send.spec.ts: sending to a launcher-bound opencode window
// through opencode's API — its own server (mirror plugin on, a 1 s send-queue
// idle) and two fake hosts (one opted in to the send, one not), each with a
// fake opencode server in its own process.
export const openCodeSendPort = port + 271;
// tests/chief-ux.spec.ts: the chief's working indicator across a reload, its
// ask_owner question cards and its dispatched-agent cards — its own server (the
// e2eterm binary, task routing and chief dispatch on, every agent endpoint
// pointed at the spec's FAKE runtime), its /mcp door, and that fake runtime.
export const chiefUxPort = port + 272;
export const chiefUxConnectPort = port + 273;
export const chiefUxFakePort = port + 274;
// tests/agent-outcomes.spec.ts: a dispatched agent ends in a visible outcome —
// its own server (the e2eterm build: a fake agent runtime every agent's
// endpoint points at, a fake model catalogue, a 3 s silence watchdog).
export const outcomesPort = port + 290;
export const outcomesRuntimePort = port + 291;
export const outcomesCatalogPort = port + 292;
// tests/mcp-dispatch.spec.ts: the owner's mirrored session starts an agent in
// two verbs (dispatch_request, dispatch_approve) — its own server (mirror plugin
// on, chief dispatch on, both pod kinds over the noop driver), its connect
// listener, and its fake host's loopback MCP endpoint.
export const mcpDispatchPort = port + 300;
export const mcpDispatchConnectPort = port + 301;
export const mcpDispatchMirrorMCPPort = port + 302;
// Its fake model catalogue (MUSTER_E2E_OPENROUTER_MODELS): the spec reaches no network.
export const mcpDispatchCatalogPort = port + 303;
// tests/session-names.spec.ts: a session's stable name, its rename and its
// live line — its own server (mirror plugin on, the focus pass off) and its
// own fake host. (+320: clear of the blocks other branches took.)
export const sessionNamesPort = port + 320;
// tests/waiting-composer.spec.ts: the ☰ button's waiting badge, the palette's
// waiting mark and the paperclip at rest — its own server (the e2eterm binary,
// mirror plugin on, attachments configured) and its connect listener (the
// asking agent's /mcp door).
export const waitingComposerPort = port + 330;
export const waitingComposerConnectPort = port + 331;
// tests/notify-tmux.spec.ts: "Show tmux sessions" — its own
// server (mirror plugin on) and its own fake host. (+340: clear of the blocks
// other branches took.)
export const notifyTmuxPort = port + 340;
// tests/chief-fold-in.spec.ts: the chief's page in a mirrored session's layout —
// its own mirror rig (the e2eterm binary, mirror plugin on, the claude-code
// kind over the noop driver) and one fake host, for the session it is compared
// with. (+345: it was +340 until notify-tmux took that on main.)
export const chiefFoldInPort = port + 345;
// tests/chief-in-view.spec.ts: asking the chief from a session's view — its own
// mirror rig (the e2eterm binary, mirror plugin on, the agent gateway on with
// every agent endpoint pointed at the spec's FAKE runtime) with one fake host
// whose opencode server is a fake in its own process, and that fake runtime.
export const chiefInViewPort = port + 350;
export const chiefInViewFakePort = port + 351;
// tests/agent-chat-markdown.spec.ts: the agent chat's messages drawn by the
// server's one markdown renderer, live and stored — its own server (the
// e2eterm build, every agent endpoint pointed at the spec's FAKE runtime) and
// that runtime. (+355: chief-in-view took +350/+351 on main.)
export const chatMarkdownPort = port + 355;
export const chatMarkdownRuntimePort = port + 356;
// tests/chief-succession.spec.ts: a succession from a session's view (phase 2)
// — its own mirror rig (as chief-in-view's, with task routing and the chief's
// dispatch on), one fake host whose agent spawn is played, the spec's FAKE
// runtime (the scripted chief) and its fake GitHub App. (+362: agent-chat-
// markdown took +355/+356 on main.)
export const chiefSuccessionPort = port + 362;
export const chiefSuccessionFakePort = port + 363;
export const chiefSuccessionGitHubPort = port + 364;
// tests/chief-end-session.spec.ts: ending a session from its view — its own
// mirror rig (as chief-succession's, no GitHub App) with one fake host that
// plays agent-close (MUSTER_MIRROR_E2E_CLOSE), and the spec's FAKE runtime.
// (+366: chief-succession takes +362..+365.)
export const chiefEndSessionPort = port + 366;
export const chiefEndSessionFakePort = port + 367;
// tests/plugin-chip.spec.ts: the Preview button and panel on a mirrored window's
// session views — its own server (mirror plugin on, MUSTER_PLUGINS=1,
// MUSTER_PLUGIN_ORIGINS set) and one fake host. Its plugin's entry page is
// played by the spec (page.route), so it needs no port of its own.
export const pluginChipPort = port + 372;
// cls.spec.ts "the session views with a live preview": its own server
// (mirror plugin, plugins and MUSTER_PLUGIN_ORIGINS on) and one fake host, so
// the Rendered and Terminal views are measured WITH the Preview button.
export const clsChipPort = port + 373;
// tests/branding.spec.ts: the instance branding — a password-mode server of
// its own (the admin's upload), and a gotrue-mode pair with its own GoTrue
// compose project (a member's Account page).
export const brandingPort = port + 374;
export const brandingMainPort = port + 375;
export const brandingBrowserPort = port + 376;
export const brandingGotruePort = Number(process.env.MUSTER_E2E_BRANDING_GOTRUE_PORT ?? port + 377);
// tests/previews.spec.ts: the whole previews path — its own server (mirror
// plugin, plugins and MUSTER_PLUGIN_ORIGINS on) and one fake host running
// `-preview`; the REAL gateway (cmd/muster-preview: its public listener, which
// browsers reach, and its mesh listener, which the host's tunnel dials); the
// TLS edge in front of the public listener (e2e/previewfix edge), which the
// browser's every preview name resolves to; and the dev app's two listeners
// (e2e/previewfix app), the only ports the host's allowlist names.
export const previewsPort = port + 380;
export const previewsGatewayPort = port + 381;
export const previewsMeshPort = port + 382;
export const previewsEdgePort = port + 383;
export const previewsAppPort = port + 384;
export const previewsSiblingPort = port + 385;
// tests/routing-rules.spec.ts: routing rules and the usage page — its own
// server (plugins on, a gateway chief) and the spec's FAKE runtime (the
// scripted chief, which proposes a rule). No mirror host. (+386: previews.spec
// takes +380..+385.)
export const routingRulesPort = port + 386;
export const routingRulesFakePort = port + 387;
// The tail-first bubble clamp (tail-clamp.spec.ts): its own server (the e2eterm
// binary, mirror plugin on) and one fake mirrored host, for the mirrored view,
// the agent chat page and the chief panel.
export const tailClampPort = port + 388;
// The mirrored view's context chip (mirror-context-chip.spec.ts): its own
// server (the e2eterm binary, mirror plugin on) and one fake host whose fake
// opencode server reports a long model id and a large window.
export const contextChipPort = port + 389;
// The message bar kept usable beside the preview panel
// (preview-composer.spec.ts): its own server (mirror plugin, plugins and
// MUSTER_PLUGIN_ORIGINS, speech-to-text and uploads configured so the mic and
// the paperclip are drawn) and one interactive fake host. +391 is the
// speech-to-text address it names, where nothing listens.
export const previewComposerPort = port + 390;
export const previewComposerSttPort = port + 391;
export const baseURL = `http://localhost:${port}`;
// tests/pages.spec.ts: shareable pages end to end — its own server (mirror
// plugin, pages and attachments on, a fake permission router capturing the
// owner's notifications), its fake S3, the fake router, and its fake host's
// loopback MCP endpoint (the agent's tools). (+400: clear of the blocks above.)
export const pagesPort = port + 400;
export const pagesFakeS3Port = port + 401;
export const pagesRouterPort = port + 402;
export const pagesMirrorMCPPort = port + 403;
// tests/pages-share.spec.ts: shareable pages shared at once
// (a link key; its owner opts in to their agents sharing without asking) — its own server and its fake
// host's loopback MCP endpoint, and the connect listener a link key needs
// beside it. (+405 to +407: clear of the block above.)
export const pagesSharePort = port + 405;
export const pagesShareMirrorMCPPort = port + 406;
export const pagesShareConnectPort = port + 407;
// tests/server-switcher.spec.ts: a FAKE second muster server (its own origin,
// 127.0.0.1) whose instance document and icon the spec controls.
export const serverSwitcherStubPort = port + 404;
// tests/team-pages.spec.ts: a team's page read by a member who is not its
// creator — its own server (pages on, a link key) and the connect listener
// the key needs. (+463, +464: clear of every block, +462 the highest taken.)
export const teamPagesPort = port + 463;
export const teamPagesConnectPort = port + 464;
// tests/stepup-card-page.spec.ts: a step-up confirmation card's page at a
// phone width — its own server (+465: clear of every block, +464 the highest
// taken).
export const stepupCardPagePort = port + 465;
// tests/draft-card.spec.ts: the Asks page's reply-draft card at a phone and a
// desktop width — its own server (plugins on), no mirror host.
export const draftCardPort = port + 413; // was +405, pagesSharePort's too (e2e/env-ports.test.mjs)
// tests/asks-log.spec.ts: the Asks page's Log tab (the ask log on), at a phone
// and a desktop width — its own server (+414: clear of the blocks above).
export const asksLogPort = port + 414;
// tests/asks-feed.spec.ts: the Asks page's card feed (plugins, task routing
// and chief nudges on, so every tab is drawn) at a phone and a desktop width —
// its own server (+421: clear of the blocks above).
export const asksFeedPort = port + 421;
// tests/chief-fleet-digest.spec.ts: the chief panel's fleet digest card at a
// phone and a desktop width — its own mirror rig (session state on), its fake
// runtime (which must hear nothing), its fake GitHub App and the App's connect
// listener (+425 to +428: clear of the blocks above).
export const chiefFleetPort = port + 425;
export const chiefFleetFakePort = port + 426;
export const chiefFleetGitHubPort = port + 427;
export const chiefFleetConnectPort = port + 428;
// tests/chief-fleet-proposals.spec.ts: the fleet digest's proposal cards
// (MUSTER_FLEET_ORCHESTRATION=propose) at a phone and a desktop width — its
// own mirror rig, fake runtime, fake GitHub App and the App's connect listener
// (+446 to +449: clear of the blocks above, and of features-member's +442 to
// +444).
export const chiefProposePort = port + 446;
export const chiefProposeFakePort = port + 447;
export const chiefProposeGitHubPort = port + 448;
export const chiefProposeConnectPort = port + 449;
// tests/team-ux.spec.ts: a team's manager pages and a viewer's Asks, signed
// in through its own real GoTrue (+410 to +412: clear of the blocks above).
export const teamUxMainPort = port + 410;
export const teamUxBrowserPort = port + 411;
export const teamUxGotruePort = Number(process.env.MUSTER_E2E_TEAMUX_GOTRUE_PORT ?? port + 412);
// tests/ask-media.spec.ts: an agent's ask carrying an image — its own server
// (uploads on, against a fake S3 the spec plays), its connect listener, and
// the fake S3 (+430 to +432: clear of the blocks above, and of asksFeedPort's +421).
export const askMediaPort = port + 430;
export const askMediaConnectPort = port + 431;
export const askMediaS3Port = port + 432;
// tests/agent-sidebar.spec.ts: the sidebar's Officers group with an officer
// that runs in a pod — its own server over the noop driver, and its connect
// listener (a pod officer in MUSTER_OFFICERS needs one). +440/+441: clear of
// the blocks above, and of askMediaPort's +430 to +432.
export const sidebarOfficersPort = port + 440;
export const sidebarOfficersConnectPort = port + 441;
// tests/features-member.spec.ts: a member's Features page, on a gotrue-mode
// server of its own (+442 its machine port, +443 its browser listener) and
// its own GoTrue (+444).
export const featuresMemberMainPort = port + 442;
export const featuresMemberBrowserPort = port + 443;
export const featuresMemberGotruePort = Number(process.env.MUSTER_E2E_FEATURES_MEMBER_GOTRUE_PORT ?? port + 444);
// The session view's Open button and its Previews | Pages panel
// (session-open.spec.ts): its own mirror server (plugins and pages on).
// +457: clear of officerNeedsYou's +445, pluginAutostart's +450 to +452,
// morningReport's +453 and pagesDeclutter's +455/+456.
export const sessionOpenPort = port + 457;
// tests/morning-report.spec.ts: the morning report's page on a phone — its own
// server with MUSTER_MORNING_REPORT=1 (+453: clear of the blocks above,
// officerNeedsYouPort's +445 and chiefPropose's +446 to +449).
export const morningReportPort = port + 453;
// tests/plugin-autostart.spec.ts: a plugin's auto-start on its /plugins card —
// its own e2eterm server over the noop driver, its connect listener, and the
// fake GitHub App it plays (+450 to +452: clear of the blocks above).
export const pluginAutostartPort = port + 450;
export const pluginAutostartConnectPort = port + 451;
export const pluginAutostartFakePort = port + 452;
// tests/opencode-ask-media.spec.ts: a mirrored opencode session's ask carrying
// a file — its own e2eterm server (uploads on, against a fake S3 the spec
// plays), the fake S3, and the fake host's loopback MCP endpoint (+460 to
// +462: clear of the blocks above).
export const ocAskMediaPort = port + 460;
export const ocAskMediaS3Port = port + 461;
export const ocAskMediaMCPPort = port + 462;
// tests/officer-needs-you.spec.ts: an officer's page as the place its owner
// deals with it — its own server with plugins on (+445: clear of the blocks
// above, of sidebarOfficersPort's +440/+441 and of featuresMember's +442 to +444).
export const officerNeedsYouPort = port + 445;
// tests/pages-declutter.spec.ts: the Pages screens, plain — its own server
// (pages on, a link key, so +456 its connect listener). +455: clear of the
// blocks above.
export const pagesDeclutterPort = port + 455;
export const pagesDeclutterConnectPort = port + 456;
// tests/plugin-app.spec.ts: the app tab's page — its own server (plugins and
// MUSTER_PLUGIN_ORIGINS on), the stub app plugin (e2e/lib/appstub.ts) and the
// previews specs' TLS edge in front of it (e2e/previewfix edge), which every
// name of the stub's zone resolves to (+470 to +472: clear of the blocks above).
export const pluginAppPort = port + 470;
export const pluginAppStubPort = port + 471;
export const pluginAppEdgePort = port + 472;
// cls.spec.ts "the app tab's page": the same three, its own (+473 to +475).
export const clsAppPort = port + 473;
export const clsAppStubPort = port + 474;
export const clsAppEdgePort = port + 475;
// The knock push spec (plugin-knock-push.spec.ts): its own server (push and
// plugins on, a paired test phone) and its push sink's CONNECT proxy
// (e2e/pushsink), through which that server reaches the sink's TLS endpoint.
export const knockPushPort = port + 476;
export const knockPushSinkPort = port + 477;
// tests/pages-phone.spec.ts: a markdown page read on a phone — its own server
// (pages on, a link key, so +479 its connect listener). +478: clear of the
// blocks above.
export const pagesPhonePort = port + 478;
export const pagesPhoneConnectPort = port + 479;
// tests/plugin-server-app.spec.ts: a server-installed app (core PR 1h.2) — its
// own gotrue-mode server (main and browser listeners), its own GoTrue, and the
// stub app plugin behind the TLS edge (+480 to +484: clear of the blocks above).
export const serverAppMainPort = port + 480;
export const serverAppBrowserPort = port + 481;
export const serverAppGotruePort = Number(process.env.MUSTER_E2E_SERVERAPP_GOTRUE_PORT ?? port + 482);
export const serverAppStubPort = port + 483;
export const serverAppEdgePort = port + 484;
// tests/asks-search.spec.ts: the Asks page's search box and the ids on its
// cards — its own server (plugins and task routing on, for the fixture's
// draft and task). +485: clear of the blocks above.
export const asksSearchPort = port + 485;
// tests/signin-progress.spec.ts: its own password-mode server, so its refused
// and accepted sign-ins are charged to a budget of their own (lib/server.ts).
export const signinProgressPort = port + 486;
// tests/instance-theme.spec.ts: the server's theme (docs/ui-kit.md) — its own
// password-mode server (+487: clear of the blocks above), so no other server
// caches the theme it saves.
export const instanceThemePort = port + 487;
// tests/ui-kit-parity.spec.ts: muster's own components against the UI kit's —
// its own password-mode server (+488: clear of the blocks above), with a logo
// set so the branding section's secondary button is drawn.
export const uiKitParityPort = port + 488;
// tests/qr-signin.spec.ts: sign in with the muster app in gotrue mode — its
// own server's two listeners and its own real GoTrue. +490 to +492: clear of
// the blocks above.
export const qrMainPort = port + 490;
export const qrBrowserPort = port + 491;
export const qrGotruePort = Number(process.env.MUSTER_E2E_QR_GOTRUE_PORT ?? port + 492);
export const versionA = 'e2e-a';
export const versionB = 'e2e-b';
// A third build: the update spec's "a newer build after a dismissal" case.
export const versionC = 'e2e-c';
export const versionTerm = 'e2e-term';
