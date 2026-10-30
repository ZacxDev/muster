package api

import (
	"net/url"
	"testing"
)

// TestADispatchRecordsWhetherTheAccountWasPinned: a dispatch that names
// cc_account stores a PINNED agent (not failed over), one that lets the pool
// choose stores an unpinned one (Auto: failed over by default), and cc_failover
// is the pinned agent's opt-in. A gateway agent has neither.
func TestADispatchRecordsWhetherTheAccountWasPinned(t *testing.T) {
	s, h, store := kindsServer(t, stubKinds{pick: "work"})
	for _, form := range []url.Values{
		{"kind": {"claude-code"}},
		{"kind": {"claude-code"}, "cc_account": {"personal"}},
		{"kind": {"claude-code"}, "cc_account": {"personal"}, "cc_failover": {"on"}},
		{},
	} {
		if rec := postDispatch(t, s, h, form); !dispatched(rec, store) {
			t.Fatalf("%v: %d %s", form, rec.Code, rec.Body)
		}
	}
	rows := store.rows()
	if len(rows) != 4 {
		t.Fatalf("%d rows", len(rows))
	}
	type pin struct {
		account       string
		pinned, optIn bool
	}
	want := []pin{{"work", false, false}, {"personal", true, false}, {"personal", true, true}, {"", false, false}}
	for i, w := range want {
		got := pin{rows[i].CCAccount, rows[i].CCAccountPinned, rows[i].CCFailover}
		if got != w {
			t.Errorf("row %d = %+v, want %+v", i, got, w)
		}
	}
}
