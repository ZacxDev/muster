package hermeshost

import "strings"

// THE GATEWAY'S OWN MASKS (audit #212). Before an approval reaches any
// client, the pinned gateway rewrites its command through
// _redact_approval_command (P:gateway/run.py:577 →
// P:agent/redact.py, redact_sensitive_text(force=True)). A mask can swallow
// shell syntax — measured: `ls -H X-Api-Key:a;rm -rf ~/work` arrives as
// `ls -H X-Api-Key:*** -rf ~/work` — so a command carrying ANY of its masks is
// not the command that runs, and muster never offers to approve it from a
// card ("once" only for a command shown whole).
//
// The forms, read from the pinned agent/redact.py (every literal its mask
// functions emit; TestTheGatewayMasksMatchThePinnedRedactor, livehermes,
// holds this list against the source and runs the real redactor over a
// corpus, so a pin bump that changes the redactor is RED):
//
//   - "***"                       the placeholder (mask_secret, _mask_token,
//     the assignment, header, URL and repr passes)
//     and "****" (a phone's middle) contains it;
//   - "..."                       mask_secret's head...tail form
//     (f"{value[:head]}...{value[-tail:]}");
//   - "«redacted"                 the non-reusable sentinels («redacted:…»,
//     «redacted-secret», «redacted-vault-secret»);
//   - "[REDACTED"                 [REDACTED PRIVATE KEY];
//   - "[redacted"                 [redacted: nesting limit], [redacted-jwt].
//
// ⚠ "..." also appears in commands nobody masked (`git diff a...b`): such a
// command is offered deny only — a false refusal, the safe way round.
var GatewayMasks = []string{"***", "...", "«redacted", "[REDACTED", "[redacted"}

// GatewayMasked reports whether s carries any of the gateway's masks.
func GatewayMasked(s string) bool {
	for _, m := range GatewayMasks {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
