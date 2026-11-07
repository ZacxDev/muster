package main

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/asklog"
)

// TestTheAskLogsConfiguration: off by default with a 30-day text TTL; on with
// MUSTER_ASK_LOG=1; the TTL takes a duration from one hour to the row TTL and
// refuses anything else, naming the variable; classification at capture is its
// own switch, off by default.
func TestTheAskLogsConfiguration(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	c, err := parseAskLog(env(nil))
	if err != nil || c.On || c.JevAtCapture || c.TextTTL != 30*24*time.Hour {
		t.Fatalf("the defaults: %+v %v", c, err)
	}
	c, err = parseAskLog(env(map[string]string{envAskLog: "1", envAskLogTextTTL: "72h", envAskLogClassify: "1"}))
	if err != nil || !c.On || !c.JevAtCapture || c.TextTTL != 72*time.Hour {
		t.Fatalf("set: %+v %v", c, err)
	}
	for _, bad := range []string{"30m", "4321h", "a month", "-1h"} {
		if _, err := parseAskLog(env(map[string]string{envAskLogTextTTL: bad})); err == nil || !strings.Contains(err.Error(), envAskLogTextTTL) {
			t.Errorf("%s=%q: %v, want a refusal naming the variable", envAskLogTextTTL, bad, err)
		}
	}
	for _, ok := range []string{"1h", "4320h"} {
		if _, err := parseAskLog(env(map[string]string{envAskLogTextTTL: ok})); err != nil {
			t.Errorf("%s=%q (a bound) refused: %v", envAskLogTextTTL, ok, err)
		}
	}
}

// TestTheBannerSaysWhetherTheAskLogIsOn: the boot banner prints the ask log's
// line in each of its three states, naming the variable, and the ON line
// states the text TTL it will purge with.
func TestTheBannerSaysWhetherTheAskLogIsOn(t *testing.T) {
	off := renderBanner(t, config{}, api.Extensions{})
	if !strings.Contains(off, "ask log: OFF — "+envAskLog+" is unset") {
		t.Fatalf("off:\n%s", off)
	}
	inert := renderBanner(t, config{AskLog: askLogConfig{On: true}}, api.Extensions{})
	if !strings.Contains(inert, "ask log: INERT — "+envAskLog+"=1 but there is no database") {
		t.Fatalf("inert:\n%s", inert)
	}
	on := renderBanner(t, config{AskLog: askLogConfig{On: true}}, api.Extensions{AskLog: &api.AskLog{Store: asklog.NewPG(nil), TextTTL: 72 * time.Hour}})
	if !strings.Contains(on, "ask log: ON ("+envAskLog+"=1)") || !strings.Contains(on, "cleared after 72h0m0s ("+envAskLogTextTTL+")") ||
		!strings.Contains(on, "classification at capture off ("+envAskLogClassify+" is unset)") {
		t.Fatalf("on:\n%s", on)
	}
}
