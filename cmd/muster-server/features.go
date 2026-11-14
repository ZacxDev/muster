package main

import (
	"strings"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/features"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/router"
)

// THE FEATURE REGISTRY'S CATALOG AND ITS EVALUATOR (internal/features).
//
// The catalog says what each feature is and which variables turn it on; the
// evaluator says whether this process runs it. Both live here, beside config.go,
// because this is the only package that may spell a variable name
// (TestEveryServerEnvNameIsSpelledOnceAndRead) and the only one that holds both
// the parsed config and the wired extensions.
//
// 🔴 THE EVALUATOR READS WHAT WAS WIRED, NOT WHAT WAS ASKED FOR. "Dictation is
// on" is `ext.Transcriber != nil` — the field the microphone and POST
// /transcribe read — not "MUSTER_STT_TOKEN is set". Where a feature's gate is
// already a predicate in this package (parseUploads, memoRelayEnabled,
// approvalGateFor, the auth refusals), the evaluator calls it rather than
// restating it. The config is read only to tell "not asked for" (off) from
// "asked for and not wired" (misconfigured), and for the one dependency no
// extension records: whether an OpenRouter key exists.

// composeS3StoreSecret is the Compose stack's own variable for the bundled S3
// store's secret (deploy/docker-compose.yml): an .env line muster-server never
// reads, so it is not in config.go's const block.
const composeS3StoreSecret = "S3_STORE_SECRET_KEY"

// featureCatalog is every feature the registry knows, in the order the Features
// page lists them: the core first, then the optional ones.
func featureCatalog() []features.Feature {
	v := func(name, placeholder string) features.Var { return features.Var{Name: name, Placeholder: placeholder} }
	is := func(name, value string) features.Var { return features.Var{Name: name, Value: value} }
	provisioner := []features.Var{is(envAgentProvisioner, provisionerK8s), v(envAgentImageRepo, "agent image repository"),
		v(envAgentAPIURL, "the URL agents reach muster on")}
	return []features.Feature{
		{ID: features.Mirror, Name: "Mirrored sessions", Core: true,
			Description: "Every tmux window on every machine running muster-mirror, as a card; Claude Code as chat.",
			Enable:      []features.Var{is(envMirror, "1")},
			Settings:    []string{envStepUpWindow, envSendQueueIdle}},
		{ID: features.PasswordSignIn, Name: "Password sign-in", Core: true,
			Description: "One shared operator password for the web UI.",
			Enable:      []features.Var{v(envUIPassword, "at least 12 characters")},
			Settings:    []string{envSecureCookies}},
		{ID: features.Push, Name: "Push notifications",
			Description: "Phone and desktop notifications through Web Push.",
			Enable: []features.Var{v(envVAPIDPublicKey, "from muster-server vapid generate"), v(envVAPIDPrivateKey, "from muster-server vapid generate"),
				v(envVAPIDSubject, "mailto: or https: contact")},
			Settings: []string{envPushExtraHosts}},
		{ID: features.Dictation, Name: "Dictation",
			Description: "A microphone on every message box; speech is transcribed into the box.",
			// The documented install: the bundled `stt` profile, whose URL the
			// Compose file fills in when only the token is set (docs/features.md).
			Enable:   []features.Var{features.ProfileVar("stt"), v(envSTTToken, "a random secret")},
			Settings: []string{envSTTURL, envSTTModel}},
		{ID: features.Attachments, Name: "File attachments",
			Description: "Attach files in the message box; agents get signed links to them.",
			// The documented install: the bundled `s3` profile, whose store
			// the Compose file wires from its one secret (docs/features.md).
			Enable: []features.Var{features.ProfileVar("s3"), {Name: composeS3StoreSecret, Placeholder: "a random secret, 8+ characters", Compose: true},
				v(envUploadSigningKey, "32+ random bytes")},
			Settings: []string{envS3Endpoint, envS3Bucket, envS3AccessKey, envS3SecretKey, envPublicURL, envS3Region, envUploadMaxBytes, envUploadLinkTTL}},
		{ID: features.SuggestedReplies, Name: "Suggested replies",
			Description: "Up to three suggested replies when a Claude Code session hands back control. Off for each user until they turn them on in Account (with no database, until MUSTER_SUGGEST=on); needs an OpenRouter key.",
			Enable:      []features.Var{v(envAgentOpenRouterKey, "OpenRouter key")},
			Settings:    []string{envSuggest, envSuggestModel}},
		{ID: features.Focus, Name: "Session focus",
			Description: "A short title and summary for every session, and the chief's fleet tools. Needs an OpenRouter key.",
			Enable:      []features.Var{v(envAgentOpenRouterKey, "OpenRouter key")},
			Settings:    []string{envFocusInterval, envFocusRetention, envFocusModel}},
		{ID: features.Accounts, Name: "Per-user accounts",
			Description: "Users, passkeys, invites and roles through a GoTrue server you run.",
			Enable: []features.Var{is(envAuthMode, "gotrue"), v(envGoTrueURL, "GoTrue base URL"), v(envGoTrueSecret, "GoTrue JWT secret"),
				v(envBrowserPort, "browser listener port")}},
		{ID: features.Connectors, Name: "Connectors and credentials",
			Description: "Stored platform credentials agents use through muster's proxy, and each member's own Claude and OpenRouter credentials.",
			Enable:      []features.Var{v(envConnectorEncKey, "32-byte base64 key")},
			Settings:    []string{envConnectPort, envAgentConnectURL}},
		{ID: features.GitHubApp, Name: "GitHub App",
			Description: "muster's own GitHub identity: per-repository, one-hour tokens for agents.",
			Enable: []features.Var{v(envGitHubAppID, "App ID"), v(envGitHubAppKey, "App private key (PEM)"),
				v(envGitHubAppAccounts, "login:id,…")},
			Settings: []string{envGitHubAppAccess}},
		{ID: features.Dispatch, Kubernetes: true, Name: "Agent dispatch",
			Description: "Start, stop and destroy agents in their own Kubernetes pods.",
			Enable:      provisioner,
			Settings: []string{envAgentImageTag, envAgentModel, envAgentOpenRouterKey, envAgentNSPrefix, envAgentNSShared, envAgentNamespace,
				envAgentNSClusterRole, envAgentWorkspaceKeep, envAgentStorageClass, envAgentEndpointTmpl, envAgentCairnURL, envAgentCairnToken}},
		{ID: features.AgentGateway, Kubernetes: true, Name: "Agent chat",
			Description: "Talk to dispatched agents, and deliver their first turn, through the agent runtime's gateway.",
			Enable: []features.Var{is(envAgentGateway, gatewayHooksSHA256), v(envAgentGatewayModel, "the runtime's model value"),
				v(envAgentRuntimeConfig, "runtime config file"), v(envAgentRuntimeInstall, "install script")},
			Settings: []string{envAgentGatewayPort}},
		{ID: features.ClaudeCodeAgents, Kubernetes: true, Name: "Claude Code agents",
			Description: "Agents that run the Claude Code CLI in a pod, on your Claude subscription accounts.",
			Enable: []features.Var{is(envAgentKinds, "gateway,claude-code"), v(envAgentCCImage, "claude-code agent image"),
				v(envAgentCCAccounts, "account names"), v(envAgentCCTokenPrefix+"<NAME>", "claude setup-token")},
			Settings: []string{envAgentCCStorage, envAgentCCCooldown, envAgentCCMaxJobs, envAgentCCFailover, envAgentCCBypassPools,
				envAgentCCPoolDeployKeys, envAgentCCEgressLockedPools, envAgentNetpolFromNS, envAgentNetpolFromLabels,
				envOfficerEgressCIDRs, envOfficerEgressDNS, envCheckoutWaitTimeout,
				envChiefLLM, envCCUsagePoll, envCCUsagePollInterval, envCCUsagePushURL, envCCUsagePushToken, envCCUsagePushInstance,
				envCCConsumerUsage}},
		// The documented install: the Compose stack's `chief` profile and the
		// Compose chief runtime (composechief.go). A Kubernetes deployment runs
		// it through MUSTER_AGENT_PROVISIONER instead (docs/chief.md).
		{ID: features.Chief, Name: "Chief",
			Description: "A fleet agent that reports on your sessions and agents, and routes work to them; the admin turns it on under Account.",
			Enable: []features.Var{features.ProfileVar("chief"), is(envChiefRuntime, chiefRuntimeCompose),
				v(envConnectorEncKey, "32 random bytes, base64")},
			Note:     composeChiefCredentialNote,
			Settings: []string{envChiefSpecDir, envChiefClaudeToken, envChiefLLMModel}},
		{ID: features.ChiefSessionRuntimes, Kubernetes: true, Name: "Chief session runtimes",
			Description: "Start a chief chat session on Claude Code, with its own model.",
			Enable:      []features.Var{is(envChiefSessionRuntimes, "claude-code")}},
		{ID: features.ChiefNudges, Kubernetes: true, Name: "Chief stall nudges",
			Description: "The chief judges idle sessions and nudges or asks (shadow mode by default).",
			Enable:      []features.Var{is(envChiefNudges, "1")},
			Settings: []string{envChiefNudgeLiveOwners, envChiefNudgeNeedsYouLiveOwners, envChiefNudgeChiefChatQuotesLiveOwners, envChiefNudgeIdle,
				envChiefNudgeHourlyCap, envChiefNudgeAskThreshold}},
		{ID: features.SessionState, Name: "Session state",
			Description: "One push per stop, and the Asks page's Stalled / waiting section.",
			Enable:      []features.Var{is(envSessionState, "1")}},
		{ID: features.AskLog, Name: "Ask log",
			Description: "Every question an agent asks you, with your answer and how long it took, recorded for review.",
			Enable:      []features.Var{is(envAskLog, "1")},
			Settings:    []string{envAskLogTextTTL, envAskLogClassify}},
		{ID: features.Pages, Name: "Shareable pages",
			Description: "Agents publish pages for you — showcases, questions, forms, uploads, custom script in a sandbox — and ask before a link anyone can open goes live.",
			Enable:      []features.Var{is(envPages, "1")},
			Settings:    []string{envPublicURL, envPagesUploadBytes}},
		{ID: features.Plugins, NeedsChief: true, Name: "Plugins",
			Description: "External bots post messages to your chief; it drafts replies you approve.",
			Enable:      []features.Var{is(envPlugins, "1")}},
		{ID: features.PluginViewers, Name: "Plugin viewers",
			Description: "A viewer plugin's chip on your session views, opening its own origin as you.",
			Enable:      []features.Var{v(envPluginOrigins, "origin patterns")}},
		{ID: features.PluginTasks, Name: "Plugin tasks",
			Description: "A plugin files one task per upstream record.",
			Enable:      []features.Var{v(envPluginTaskNamespaces, "tag namespaces")}},
		{ID: features.SessionRouting, NeedsChief: true, Name: "Session routing",
			Description: "The chief hands one of your sessions an idea you typed in its chat, a task, or (with plugins) a plugin message; the session's answer comes back.",
			Enable:      []features.Var{is(envSessionRouting, "1")},
			Settings:    []string{envRouteRelayAllow}},
		{ID: features.TaskRouting, NeedsChief: true, Name: "Task routing",
			Description: "A plugin hands a task to the chief, which routes it (and may start an agent on it).",
			Enable:      []features.Var{is(envTaskRouting, "1")},
			Settings:    []string{envChiefDispatchOwners, envChiefDispatchModels}},
		{ID: features.PRMailRouting, Name: "Pull-request review routing",
			Description: "Review mail goes to the session that opened the pull request.",
			Enable:      []features.Var{v(envPRMailSenders, "owner/handle,…")}},
		{ID: features.FleetReports, Name: "Fleet reports",
			Description: "muster answers a listed sender's \"what did my sessions do\" mail itself.",
			Enable:      []features.Var{v(envFleetReportSenders, "owner/handle,…"), v(envFleetReportPaths, "working-directory prefixes")}},
		{ID: features.MailResume, Name: "Mail resume",
			Description: "Mail for an ended session resumes it on its host.",
			Enable:      []features.Var{v(envMailResumeAllow, "user:plugin:owner/handle,…")},
			Settings:    []string{envMailResumeOff}},
		{ID: features.MemoRelay, Kubernetes: true, Name: "Memo relay",
			Description: "cairn memos delivered to the operator's running agents.",
			Enable:      []features.Var{v(envMemoURL, "cairn memo listener URL"), v(envMemoToken, "read-only cairn credential")},
			Settings:    []string{envMemoScopeMap}},
		{ID: features.ApprovalRouter, Name: "Approval relay and permission router",
			Description: "Agents' permission prompts as cards you approve; transcript links and notifications from a separate permission router.",
			Enable:      []features.Var{v(envApprovalGateURL, "gate endpoint"), v(envApprovalGateToken, "gate credential")},
			Settings:    []string{envRouterURL, envRouterToken, envRouterActor, envRouterActorHeader, envRouterTokenHeader}},
		{ID: features.BrowserFlows, Name: "Browser flows",
			Description: "Scripted browser jobs an agent asks for and you approve.",
			Note:        "on with the database; jobs run only on a runner you host — mint its token on /flows",
			Settings:    []string{envFlowAgentDenyWords}},
		{ID: features.AgentAskTool, Name: "Muster tools in sessions",
			Description: "attention_ask and the task tools inside each mirrored Claude Code session, and AskUserQuestion cards.",
			HostSide:    true,
			Note: "on each host, run muster-mirror with -mcp-listen 127.0.0.1:<port>, register http://127.0.0.1:<port>/mcp " +
				"in Claude Code's MCP config, and add the `muster-mirror ask` PreToolUse hook"},
		// A real optional door (another service writes as the admin), off
		// until set, and no other check owns it — unlike the hook token, which
		// `muster doctor`'s hook-token check owns.
		{ID: features.ServiceToken, Name: "Service token",
			Description: "Service-to-service writes that assert an actor name.",
			Enable:      []features.Var{v(envServiceToken, "at least 32 characters")}},
		{ID: features.Android, Name: "Android app passkeys",
			Description: "Lets the Android app use this site's passkeys.",
			Enable:      []features.Var{v(envAndroidPackage, "package name"), v(envAndroidCertSHA256, "signing-certificate fingerprints")},
			Settings:    []string{envAndroidAllowPINKeys, envAndroidSSOOrigins}},
		{ID: features.Telemetry, Name: "Browser telemetry",
			Description: "Grafana Faro real-user monitoring from the web UI.",
			Enable:      []features.Var{v(envFaroUpstream, "Faro receiver collect URL")},
			Settings:    []string{envFaroURL, envFaroKey, envFaroTracing}},
		{ID: features.Officers, Name: "Officers",
			Description: "Long-running workers that take items from a queue (HQ, or muster's built-in backend), in pods or as host-spawned sessions on one of your machines.",
			Enable:      []features.Var{v(envOfficers, "JSON array")},
			Settings: []string{officers.TokenEnvPrefix + "<NAME>", officers.AppKeyEnvPrefix + "<NAME>", officers.EscalateWebhookEnvPrefix + "<NAME>", officers.LLMKeyEnvPrefix + "<NAME>",
				officers.KubeEndpointEnvPrefix + "<CLUSTER>", officers.KubeCAEnvPrefix + "<CLUSTER>", officers.KubeMinterEnvPrefix + "<CLUSTER>",
				officers.KubeAllowlistEnvPrefix + "<CLUSTER>", officers.ImageRegistriesEnv, envOfficerImagePullSecret}},
	}
}

// openRouterKeyMissing is the ONE answer to "is there no OpenRouter key for the
// features that spend one": suggested replies, focus and the chief's stall
// classifier all take theirs from the credential plane, whose environment key is
// MUSTER_AGENT_OPENROUTER_KEY (buildCredentialPlane).
func (c config) openRouterKeyMissing() bool { return c.AgentOpenRouterKey == "" }

// openRouterReason is what a feature that spends an OpenRouter key says when
// the environment has none. With a credential store, members can still bring
// their own; without one, nobody has a key.
func openRouterReason(what string, ext api.Extensions) string {
	r := envAgentOpenRouterKey + " is unset, so " + what
	if ext.Credentials != nil {
		return r + " except for members who saved their own OpenRouter key on Credentials"
	}
	return r + " for anyone (no member can store a key either: " + envConnectorEncKey + " is unset)"
}

// needsOpenRouterKey is the OFF reason of a feature that is on by default and
// does nothing without an OpenRouter key (suggested replies, focus).
//
// 🔴 OFF, NOT MISCONFIGURED, AND THAT IS THE OPERATOR'S DECISION (#441 round
// 0, Q1): the keyless Compose install is the documented default, nothing was
// asked for, and MISCONFIGURED means only "asked for, a dependency missing" —
// which `muster doctor` FAILs on. A default install must not fail its doctor.
func needsOpenRouterKey() string { return "needs " + envAgentOpenRouterKey }

// openRouterStatus is the state of a feature that spends the owner's
// OpenRouter key (suggested replies, focus), with no environment key: OFF
// when nobody can have a key, ON when members can store their own — a
// credential store exists — which is the only state in which it does
// anything for anyone, and the reason says for whom (#441 round 1).
func openRouterStatus(ext api.Extensions) features.Status {
	if ext.Credentials != nil {
		return features.Status{State: features.On, Reason: "for members who saved their own OpenRouter key on Credentials; " +
			"the admin (and anyone without one) " + needsOpenRouterKey()}
	}
	return features.Status{State: features.Off, Reason: needsOpenRouterKey()}
}

// chiefMissing is why a feature that acts through the chief cannot, or "": the
// chief needs an agent provisioner and a runtime to run on (the Chief entry's
// own predicate, chiefRuntimeMissing).
func chiefMissing(c config, ext api.Extensions) string {
	if ext.Provisioner == nil || chiefRuntimeMissing(c, ext) {
		return "it acts through the chief, which is not set up — see docs/features.md#chief"
	}
	return ""
}

// chiefNudgesProblem is why the wired stall nudges cannot work, or "": the ONE
// answer, read by the registry and by the banner's `chief nudges:` line, so
// the two cannot disagree (#441 round 0, D1).
func chiefNudgesProblem(c config, ext api.Extensions) string {
	if why := chiefMissing(c, ext); why != "" {
		return envChiefNudges + "=1 but " + why
	}
	if c.openRouterKeyMissing() {
		return envChiefNudges + "=1 but " + openRouterReason("the stall classifier cannot run", ext)
	}
	return ""
}

// chiefRuntimeMissing: a provisioner, but nothing a chief can run on.
func chiefRuntimeMissing(c config, ext api.Extensions) bool {
	return ext.Gateway == nil && !c.claudeCodeEnabled()
}

// featureStatuses evaluates every catalog entry against this process.
func (a *app) featureStatuses(ext api.Extensions, port router.Port) map[string]features.Status {
	c := a.cfg
	on := func(reason string) features.Status { return features.Status{State: features.On, Reason: reason} }
	off := func(reason string) features.Status { return features.Status{State: features.Off, Reason: reason} }
	bad := func(reason string) features.Status {
		return features.Status{State: features.Misconfigured, Reason: reason}
	}
	noDB := "there is no database (" + envDatabase + ")"
	pick := func(wired bool, asked bool, onR, badR, offR string) features.Status {
		switch {
		case wired:
			return on(onR)
		case asked:
			return bad(badR)
		}
		return off(offR)
	}
	gotrue := api.AuthMode(c.AuthMode) == api.AuthModeGoTrue
	st := map[string]features.Status{}

	st[features.Mirror] = pick(ext.Mirror != nil, c.Mirror, "", noDB, envMirror+" is unset")
	switch reason := c.browserAuthRefusal(); {
	case gotrue:
		st[features.PasswordSignIn] = off(envAuthMode + "=gotrue: per-user accounts replace the shared password")
	case reason != "":
		st[features.PasswordSignIn] = bad(reason + ", so every browser page answers 503")
	default:
		st[features.PasswordSignIn] = on("")
	}

	push := ext.Push != nil
	st[features.Push] = pick(push, c.pushConfigured(), "", noDB, "the VAPID keys are unset")
	switch {
	case ext.Transcriber != nil:
		st[features.Dictation] = on("")
	case c.STTURL != "":
		st[features.Dictation] = off(envSTTURL + " is set but " + envSTTToken + " is not")
	default:
		st[features.Dictation] = off(envSTTToken + " is unset")
	}
	if ext.Uploads != nil {
		st[features.Attachments] = on("")
	} else if u := parseUploads(c, a.pool != nil); u.misconfigured {
		st[features.Attachments] = bad(u.off)
	} else {
		st[features.Attachments] = off(u.off)
	}

	switch {
	case ext.Suggest == nil:
		st[features.SuggestedReplies] = off("not built")
	case ext.Suggest.Off:
		st[features.SuggestedReplies] = off(envSuggest + "=off turns them off for everyone")
	case c.openRouterKeyMissing():
		st[features.SuggestedReplies] = openRouterStatus(ext)
	case ext.Suggest.OffForEveryone():
		// After the key (without it nothing can turn them on). No database:
		// nobody can turn them on for themselves, so they are off for
		// everyone (the engine's own default) until the server says on.
		st[features.SuggestedReplies] = off(noDB + ", so they are off for everyone unless " + envSuggest + "=on")
	default:
		st[features.SuggestedReplies] = on("")
	}
	switch {
	case ext.Focus == nil:
		st[features.Focus] = off(noDB)
	case c.FocusInterval <= 0:
		st[features.Focus] = off(envFocusInterval + "=0 turns the refresh off")
	case c.openRouterKeyMissing():
		st[features.Focus] = openRouterStatus(ext)
	default:
		st[features.Focus] = on("")
	}

	st[features.Accounts] = pick(gotrue && c.goTrueConfigured() && a.pool != nil, gotrue,
		"", envAuthMode+"=gotrue but GoTrue or the database is not configured", envAuthMode+" is not gotrue (the shared password is in use)")
	st[features.Connectors] = pick(ext.Connectors != nil, false, "", "", envConnectorEncKey+" is unset")
	st[features.GitHubApp] = pick(ext.GitHubApp != nil, false, "", "", envGitHubAppID+" is unset")
	// The Compose chief's provisioner runs the chief alone: dispatch is off
	// there, for the reason api.dispatchServed reads it as off.
	switch {
	case ext.ComposeChief != nil:
		st[features.Dispatch] = off(envChiefRuntime + "=" + chiefRuntimeCompose + " runs the chief alone; agent dispatch needs muster in Kubernetes")
	default:
		st[features.Dispatch] = pick(ext.Provisioner != nil, false, "", "", envAgentProvisioner+" is unset (none)")
	}
	st[features.AgentGateway] = pick(ext.Gateway != nil, false, "", "", envAgentGateway+" is unset")
	if ext.ComposeChief != nil {
		st[features.ClaudeCodeAgents] = off(envChiefRuntime + "=" + chiefRuntimeCompose + " runs the chief on claude-code and dispatches no agent")
	} else {
		st[features.ClaudeCodeAgents] = pick(c.claudeCodeEnabled() && ext.Provisioner != nil, c.claudeCodeEnabled(),
			"", "the claude-code kind is enabled but there is no agent provisioner", envAgentKinds+" does not list claude-code")
	}
	// The chief has no variable of its own, so it is never "asked for": with
	// what it needs missing it is OFF, never MISCONFIGURED.
	switch {
	case ext.Provisioner == nil:
		st[features.Chief] = off(envChiefRuntime + " is unset (and there is no agent provisioner, " + envAgentProvisioner + ")")
	case chiefRuntimeMissing(c, ext):
		st[features.Chief] = off("there is no runtime a chief can run on (" + envAgentGateway +
			" is unset and " + envAgentKinds + " does not list claude-code)")
	case ext.ComposeChief != nil:
		st[features.Chief] = on("in its Compose container (" + envChiefRuntime + "=" + chiefRuntimeCompose +
			"); the admin turns it on under Account; network not confined")
	default:
		st[features.Chief] = on("each user turns it on under Account")
	}
	st[features.ChiefSessionRuntimes] = pick(len(ext.SessionRuntimes) > 0, false, "", "", envChiefSessionRuntimes+" is unset")
	// MUSTER_CHIEF_NUDGES=1 is an explicit ask, so a missing dependency here
	// IS misconfigured: the chief (an owner with none has nothing decided,
	// chief_nudge.go) and the stall classifier's key.
	switch {
	case ext.ChiefNudge != nil && chiefNudgesProblem(c, ext) != "":
		st[features.ChiefNudges] = bad(chiefNudgesProblem(c, ext))
	case ext.ChiefNudge != nil && len(ext.ChiefNudge.Live) == 0:
		st[features.ChiefNudges] = on("shadow for every owner (" + envChiefNudgeLiveOwners + " is empty): decisions are recorded, nothing is sent")
	case ext.ChiefNudge != nil:
		st[features.ChiefNudges] = on("")
	case c.ChiefNudges:
		st[features.ChiefNudges] = bad(envChiefNudges + "=1 but " + noDB)
	default:
		st[features.ChiefNudges] = off(envChiefNudges + " is unset")
	}
	st[features.SessionState] = pick(ext.Episodes != nil, c.SessionState, "", envSessionState+"=1 but "+noDB, envSessionState+" is unset")
	st[features.AskLog] = pick(ext.AskLog != nil, c.AskLog.On, "", envAskLog+"=1 but "+noDB, envAskLog+" is unset")
	switch {
	case ext.Pages != nil && c.PublicURL == "":
		st[features.Pages] = on("links are written as paths until " + envPublicURL + " is set, so an agent cannot hand one on")
	case ext.Pages != nil && ext.Uploads == nil:
		st[features.Pages] = on("an upload block is drawn disabled until file attachments are on (docs/features.md#attachments)")
	case ext.Pages != nil:
		st[features.Pages] = on("")
	case c.Pages:
		st[features.Pages] = bad(envPages + "=1 but " + noDB)
	default:
		st[features.Pages] = off(envPages + " is unset")
	}

	switch {
	case ext.Plugins != nil && ext.PluginGateway != nil:
		st[features.Plugins] = on("")
	case ext.Plugins != nil:
		st[features.Plugins] = bad("plugin messages reach your chief through the agent gateway (" + envAgentGateway + "), which " +
			features.KubernetesHint + " — see docs/features.md#agent-gateway; until then they are stored and stay queued")
	case c.Plugins:
		st[features.Plugins] = bad(envPlugins + "=1 but " + noDB)
	default:
		st[features.Plugins] = off(envPlugins + " is unset")
	}
	pluginsOff := "the plugin interface is off (" + envPlugins + ")"
	st[features.PluginTasks] = pick(len(c.PluginTaskNamespaces) > 0 && ext.Plugins != nil, len(c.PluginTaskNamespaces) > 0,
		"", envPluginTaskNamespaces+" is set but "+pluginsOff, envPluginTaskNamespaces+" is unset")
	st[features.PluginViewers] = pick(len(c.PluginOrigins) > 0 && ext.Plugins != nil, len(c.PluginOrigins) > 0,
		"", envPluginOrigins+" is set but "+pluginsOff, envPluginOrigins+" is unset")
	// Session and task routing are the chief's decisions: wired, they still do
	// nothing without one, and both were asked for.
	routed := func(wired, asked bool, name string) features.Status {
		if wired {
			if why := chiefMissing(c, ext); why != "" {
				return bad(name + "=1 but " + why)
			}
		}
		return pick(wired, asked, "", name+"=1 but "+pluginsOff, name+" is unset")
	}
	// Session routing does NOT need plugins: without them the chief still routes
	// the owner's own request and tasks, and only a plugin message is not routed.
	switch {
	case ext.Routing != nil && chiefMissing(c, ext) != "":
		st[features.SessionRouting] = bad(envSessionRouting + "=1 but " + chiefMissing(c, ext))
	case ext.Routing != nil && ext.Plugins == nil:
		st[features.SessionRouting] = on("your typed requests and tasks only: a plugin message is not routed (" + pluginsOff + ")")
	case ext.Routing != nil:
		st[features.SessionRouting] = on("")
	case c.SessionRouting:
		st[features.SessionRouting] = bad(envSessionRouting + "=1 but " + noDB)
	default:
		st[features.SessionRouting] = off(envSessionRouting + " is unset")
	}
	st[features.TaskRouting] = routed(ext.TaskRouting != nil, c.TaskRouting, envTaskRouting)
	st[features.PRMailRouting] = pick(ext.PRMail != nil, len(c.PRMailSenders) > 0, "",
		envPRMailSenders+" is set but session routing ("+envSessionRouting+"=1, with plugins) is off", envPRMailSenders+" is unset")
	st[features.FleetReports] = pick(ext.FleetReport != nil, len(c.FleetReportSenders) > 0, "",
		envFleetReportSenders+" is set but "+pluginsOff, envFleetReportSenders+" is unset")
	switch {
	case ext.MailResume != nil && ext.MailResume.Off:
		st[features.MailResume] = off("its kill switch " + envMailResumeOff + "=1 is set")
	case ext.MailResume != nil:
		st[features.MailResume] = on("")
	case c.MailResumeAllow.Owners() > 0:
		st[features.MailResume] = bad(envMailResumeAllow + " is set but session routing (" + envSessionRouting + "=1, with plugins) is off")
	default:
		st[features.MailResume] = off(envMailResumeAllow + " is unset")
	}
	switch {
	case c.MemoToken == "":
		st[features.MemoRelay] = off(envMemoToken + " is unset")
	case !memoRelayEnabled(c, a.pool != nil, ext.Gateway != nil):
		st[features.MemoRelay] = bad("the relay needs an agent gateway (" + envAgentGateway + ") and a database, and one is missing")
	default:
		st[features.MemoRelay] = on("")
	}
	routerNote := "no permission router: transcript links read \"no transcript recorded\""
	if port.Configured() {
		routerNote = "a permission router is configured (" + envRouterURL + ")"
	}
	if approvalGateFor(c) != nil {
		st[features.ApprovalRouter] = on(routerNote)
	} else {
		st[features.ApprovalRouter] = off(envApprovalGateURL + "/" + envApprovalGateToken + " are unset, so agents' permission prompts fall back to their terminal; " + routerNote)
	}
	st[features.BrowserFlows] = pick(ext.Flows != nil, false,
		"jobs run only on a runner you host; with none, a job expires unclaimed", "", noDB)
	st[features.AgentAskTool] = pick(ext.Mirror != nil, false,
		"set up on each machine, which this server cannot see: `muster doctor` on that machine checks it",
		"", "mirrored sessions are off ("+envMirror+")")
	st[features.ServiceToken] = pick(c.serviceWriteRefusal() == "", false, "", "", c.serviceWriteRefusal())
	st[features.Android] = pick(len(ext.AssetLinks) > 0, false, "", "", envAndroidPackage+" and "+envAndroidCertSHA256+" are unset")
	st[features.Telemetry] = pick(c.FaroUpstream != "" || c.FaroURL != "", false, "", "", envFaroUpstream+" and "+envFaroURL+" are unset")
	st[features.Officers] = pick(len(c.Officers) > 0, false, "", "", envOfficers+" declares none")

	for id, s := range st {
		s.Reason = c.redact(s.Reason)
		st[id] = s
	}
	return st
}

// buildFeatures is the evaluated registry this process serves.
//
// An error is a catalog and evaluator that disagree (an entry with no state, a
// state for no entry): a programming error, refused at boot like any other.
func (a *app) buildFeatures(ext api.Extensions, port router.Port) (features.Set, error) {
	return features.Evaluate(featureCatalog(), a.featureStatuses(ext, port))
}

// logFeatures prints the registry's one summary line. The per-feature detail
// is on the Features page and in `muster doctor`; the banner's posture lines
// above it say the rest (and agree with it: see the chief-nudges line).
func (a *app) logFeatures(set features.Set) {
	a.logger.Print(set.Summary(api.FeaturesPagePath))
}

// redact replaces any configured secret or address that a reason might have
// picked up (a parse error quoting its input) with "<redacted>".
//
// 🔴 A REASON IS SERVED TO EVERY SIGNED-IN USER (the Features page, /api/doctor), SO IT MAY
// NAME VARIABLES AND NEVER QUOTE THEM. The evaluator's own sentences quote
// nothing; this is the guard for the ones it borrows (parseUploads' reasons
// carry the S3 client's error text). TestTheFeatureRegistryLeaksNoConfiguredValue
// feeds every variable below a distinctive value and requires none to come out.
func (c config) redact(s string) string {
	if s == "" {
		return s
	}
	vals := []string{c.Database, c.UIPassword, c.HookToken, c.ServiceToken, c.GoTrueURL, c.GoTrueSecret,
		string(c.ConnectorEncKey), c.AgentConnectURL, c.FaroURL, c.FaroKey, c.FaroUpstream, c.MemoURL, c.MemoToken,
		c.STTURL, c.STTToken, c.S3Endpoint, c.S3Bucket, c.S3AccessKey, c.S3SecretKey, c.UploadSigningKey,
		c.RouterURL, c.RouterToken, c.ApprovalGateURL, c.ApprovalGateToken, c.VAPIDPublicKey, c.VAPIDPrivateKey,
		c.VAPIDSubject, c.AgentRuntimeConfig, c.AgentRuntimeInstall, c.AgentAPIURL, c.AgentOpenRouterKey,
		c.AgentCairnURL, c.AgentCairnToken, c.PublicURL}
	for _, t := range c.AgentCCTokens {
		vals = append(vals, t)
	}
	for _, v := range vals {
		// A very short value would redact ordinary words; every secret this
		// server accepts is far longer than this.
		if len(v) >= 6 {
			s = strings.ReplaceAll(s, v, "<redacted>")
		}
	}
	return s
}
