package builtin

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// maxInvestigationBytes bounds a stored investigation block.
const maxInvestigationBytes = 32 << 10

// investigationInvalid is the contract's check of an investigation block on
// a complete — HQ's rules (civ-hq store/officer_investigation.go, as the
// hqtest fake keeps them): "" when valid, else the refusal naming the field.
// It is kept on outcome escalated only.
func investigationInvalid(raw json.RawMessage, outcome string) string {
	if len(raw) > maxInvestigationBytes {
		return "investigation is over 32 KiB"
	}
	var inv map[string]any
	if json.Unmarshal(raw, &inv) != nil || inv == nil {
		return "investigation must be a JSON object"
	}
	strs := func(k string) ([]string, bool) {
		list, ok := inv[k].([]any)
		if !ok {
			return nil, inv[k] == nil
		}
		out := []string{}
		for _, v := range list {
			s, ok := v.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	if outcome != "escalated" {
		return "investigation is kept only for outcome escalated"
	}
	if h, ok := strs("hypotheses"); !ok || len(h) < 1 || len(h) > 10 {
		return "investigation.hypotheses must hold 1 to 10 entries"
	}
	if e, ok := inv["eliminated"].([]any); inv["eliminated"] != nil && (!ok || len(e) > 10) {
		return "investigation.eliminated must hold at most 10 entries"
	}
	if rc, _ := inv["root_cause"].(string); strings.TrimSpace(rc) == "" || utf8.RuneCountInString(rc) > 300 {
		return "investigation.root_cause must be 1 to 300 characters"
	}
	conf, _ := inv["confidence"].(string)
	if conf != "low" && conf != "medium" && conf != "high" {
		return "investigation.confidence must be one of low, medium, high"
	}
	fix, _ := inv["fix"].(map[string]any)
	prs, _ := fix["pr_urls"].([]any)
	steps, _ := fix["operator_steps"].([]any)
	for _, v := range steps {
		if s, ok := v.(string); !ok || strings.TrimSpace(s) == "" || utf8.RuneCountInString(s) > 1000 {
			return "investigation.fix.operator_steps entries must be 1 to 1000 characters"
		}
	}
	switch {
	case (len(prs) > 0) == (len(steps) > 0):
		return "investigation.fix must hold exactly one of pr_urls (draft PRs) or operator_steps"
	case len(prs) > 0 && conf == "low":
		return "investigation.fix.pr_urls needs confidence medium or high: at confidence low, give fix.operator_steps"
	case len(steps) > 20:
		return "investigation.fix.operator_steps must hold 1 to 20 entries"
	}
	if nc, ok := strs("not_checked"); !ok || len(nc) > 20 {
		return "investigation.not_checked must hold at most 20 entries"
	}
	return ""
}
