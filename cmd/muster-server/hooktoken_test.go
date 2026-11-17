package main

import (
	"strings"
	"testing"
)

// TestAPlaceholderOrShortHookTokenRefusesToBoot: the public .env.example's
// CHANGE-ME, or anything shorter than 12 characters, is refused by validate —
// the admin credential of every machine route must not be a known value.
func TestAPlaceholderOrShortHookTokenRefusesToBoot(t *testing.T) {
	base := memoBase()
	if err := base.validate(); err != nil {
		t.Fatalf("control: the base configuration is refused: %v", err)
	}
	for _, tok := range []string{"CHANGE-ME", "change-me", " CHANGE-ME ", "short-tok01"} {
		c := memoBase()
		c.HookToken = tok
		err := c.validate()
		if err == nil || !strings.Contains(err.Error(), envHookToken) {
			t.Errorf("HookToken %q: validate() = %v, want a refusal naming %s", tok, err, envHookToken)
		}
	}
	// The boundary: exactly 12 characters boots. Unset never does
	// (validateListeners; TestTheHookTierBootPolicy has the whole table).
	c := memoBase()
	c.HookToken = "twelve-chars"
	if err := c.validate(); err != nil {
		t.Errorf("HookToken %q (len %d): refused: %v", c.HookToken, len(c.HookToken), err)
	}
	c.HookToken = ""
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), envHookToken) {
		t.Errorf("HookToken unset: validate() = %v, want a refusal naming %s", err, envHookToken)
	}
}
