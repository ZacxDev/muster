package mirror

import (
	"testing"
	"time"
)

// TestAHeldMachineInATightLoopIsAnnouncedOncePerInterval (#685 round 1, G2):
// every held attempt is COUNTED (the card's attempts), but one address's
// attempts are logged and nudge the team's views at most once per
// heldAnnounceEvery: the first attempt announces, fifty more inside the
// interval do not, and the first after it does. Another attempt's first hold
// inside the interval is not announced either: the bound is per host.
func TestAHeldMachineInATightLoopIsAnnouncedOncePerInterval(t *testing.T) {
	h := NewHub(nil)
	clock := time.Unix(1_700_000_000, 0)
	h.now = func() time.Time { return clock }
	key := hostKey{900251, "build-06"}
	h.hosts[key] = &hostConn{key: key} // the live team host
	if admit, announce := h.admitPinned(key, "192.0.2.99", nil); admit || !announce {
		t.Fatalf("the first held attempt: admit %v, announce %v; want held and announced", admit, announce)
	}
	for i := 0; i < 50; i++ {
		clock = clock.Add(200 * time.Millisecond) // 10 s in all, inside the interval
		if admit, announce := h.admitPinned(key, "192.0.2.99", nil); admit || announce {
			t.Fatalf("held attempt %d inside the interval: admit %v, announce %v; want held, not announced", i+2, admit, announce)
		}
	}
	if got := h.HeldReplacements(key.owner); len(got) != 1 || got[0].Attempts != 51 {
		t.Fatalf("the card = %+v; want one address counting all 51 attempts", got)
	}
	// #688 round 1, Y3: the bound is per (team, host), so ANOTHER attempt's
	// first hold inside the interval is counted, not announced.
	if _, announce := h.admitPinned(key, "203.0.113.7", nil); announce {
		t.Fatal("ANOTHER attempt's first hold inside the interval was announced; the bound is per host")
	}
	clock = clock.Add(heldAnnounceEvery)
	if _, announce := h.admitPinned(key, "192.0.2.99", nil); !announce {
		t.Fatal("the first attempt after the interval was not announced")
	}
}
