package previewtunnel

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Run one criterion at a time, e.g.
//
//	go test -run 'TestC1$' -v .
//	go test -run 'TestNC' -v .
//	SPIKE_LONG=1 go test -run TestC4Long -timeout 15m -v .
//
// SPIKE_RUNS (default 3) repeats each measured criterion.

var transports = []Transport{H2, Yamux}

func runs() int {
	if n, err := strconv.Atoi(os.Getenv("SPIKE_RUNS")); err == nil && n > 0 {
		return n
	}
	return 3
}

func env(t *testing.T, o EnvOpts) *Env {
	t.Helper()
	e, err := NewEnv(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

const (
	c2Size  = 100 << 20
	c2Pause = 10 * time.Second
)

func TestC1(t *testing.T) {
	for _, tr := range transports {
		for i := 1; i <= runs(); i++ {
			e := env(t, EnvOpts{Transport: tr})
			r, err := CheckC1(e, 500)
			t.Logf("RESULT C1 %s run=%d %v", tr, i, r)
			if err != nil {
				t.Error(err)
			}
		}
	}
}

func TestC2(t *testing.T) {
	for _, tr := range transports {
		for i := 1; i <= runs(); i++ {
			e := env(t, EnvOpts{Transport: tr})
			r, err := CheckC2(e, c2Size, c2Pause)
			t.Logf("RESULT C2 %s run=%d %v", tr, i, r)
			if err != nil {
				t.Error(err)
			}
		}
	}
}

func TestC3(t *testing.T) {
	for _, tr := range transports {
		for i := 1; i <= runs(); i++ {
			e := env(t, EnvOpts{Transport: tr})
			r, err := CheckC3(e, c2Size, c2Pause, 200)
			t.Logf("RESULT C3 %s run=%d %v", tr, i, r)
			if err != nil {
				t.Error(err)
			}
		}
	}
}

// Scaled C4: the edge kills after 3 s idle, pings every 1 s, idle for 20 s
// (6.7× the edge timeout). Same ratio-direction as 100 s / 30 s / 10 min.
func TestC4Scaled(t *testing.T) {
	for _, tr := range transports {
		e := env(t, EnvOpts{Transport: tr, EdgeIdle: 3 * time.Second, Ping: time.Second})
		r, err := CheckC4(e, 20*time.Second)
		t.Logf("RESULT C4scaled %s %v", tr, r)
		if err != nil {
			t.Error(err)
		}
	}
}

// The doc's own numbers, against the LOCAL edge simulator only.
func TestC4Long(t *testing.T) {
	if os.Getenv("SPIKE_LONG") == "" {
		t.Skip("set SPIKE_LONG=1 (takes 10 min)")
	}
	for _, tr := range transports {
		t.Run(string(tr), func(t *testing.T) {
			t.Parallel()
			e := env(t, EnvOpts{Transport: tr, EdgeIdle: 100 * time.Second, Ping: 30 * time.Second})
			r, err := CheckC4(e, 10*time.Minute)
			t.Logf("RESULT C4long %s %v", tr, r)
			if err != nil {
				t.Error(err)
			}
		})
	}
}

func TestC5(t *testing.T) {
	for _, tr := range transports {
		for i := 1; i <= runs(); i++ {
			e := env(t, EnvOpts{Transport: tr})
			r, err := CheckC5(e, 20, 300*time.Millisecond)
			t.Logf("RESULT C5 %s run=%d %v", tr, i, r)
			if err != nil {
				t.Error(err)
			}
		}
	}
}

// ------------------------------------------------------- negative controls
// Each breaks one thing on purpose and asserts the check goes red WITH ITS
// OWN failure message, so a green above is evidence, not a harness wired to
// nothing.

func wantRed(t *testing.T, err error, substr string, r any) {
	t.Helper()
	t.Logf("NC result: %v", r)
	if err == nil {
		t.Fatalf("negative control stayed GREEN: %v", r)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("red for the WRONG reason (want %q): %v", substr, err)
	}
	t.Logf("NC red as expected: %v", err)
}

func TestNC_C1_SlowHost(t *testing.T) {
	for _, tr := range transports {
		e := env(t, EnvOpts{Transport: tr, Faults: Faults{HostWriteDelay: 100 * time.Millisecond}})
		r, err := CheckC1(e, 30)
		wantRed(t, err, "C1 FAIL: overhead", r)
	}
}

func TestNC_C2_Truncated(t *testing.T) {
	for _, tr := range transports {
		e := env(t, EnvOpts{Transport: tr, Faults: Faults{HostTruncateAfter: 50 << 20}})
		r, err := CheckC2(e, c2Size, 2*time.Second)
		wantRed(t, err, "not byte-exact", r)
	}
}

func TestNC_C2_UnboundedHostBuffer(t *testing.T) {
	for _, tr := range transports {
		e := env(t, EnvOpts{Transport: tr, Faults: Faults{HostUnboundedBuffer: true}})
		r, err := CheckC2(e, c2Size, 3*time.Second)
		wantRed(t, err, "heap grew", r)
	}
}

// HTTP/2 only: a connection window SMALLER than one stream's window lets the
// paused reader take all of it, which is head-of-line blocking. (Measured: a
// connection window EQUAL to the stream window stays green, because x/net
// grants the connection initialWindowSize on top, and returns connection
// credit as the other streams are read. So the real hazard is
// stalled streams × stream window ≥ connection window; see the results doc.)
func TestNC_C3_H2ConnWindowBelowStreamWindow(t *testing.T) {
	e := env(t, EnvOpts{Transport: H2, Faults: Faults{H2ConnWindow: 65535}})
	r, err := CheckC3(e, c2Size, 5*time.Second, 50)
	wantRed(t, err, "C3 FAIL", r)
	if r.AssetsOK == r.Assets {
		t.Fatalf("red, but not because assets stalled: %v", r)
	}
}

func TestNC_C4_NoPings(t *testing.T) {
	for _, tr := range transports {
		e := env(t, EnvOpts{Transport: tr, EdgeIdle: 3 * time.Second, Ping: time.Second, Faults: Faults{NoPings: true}})
		r, err := CheckC4(e, 8*time.Second)
		wantRed(t, err, "C4 FAIL", r)
		if r.EdgeKillsTun+r.EdgeKillsPrev == 0 {
			t.Fatalf("red, but the edge simulator killed nothing: %v", r)
		}
	}
}

func TestNC_C5_HostDoesNotFlush(t *testing.T) {
	for _, tr := range transports {
		e := env(t, EnvOpts{Transport: tr, Faults: Faults{HostNoFlush: true}})
		r, err := CheckC5(e, 1, 1*time.Second)
		wantRed(t, err, "event not delivered", r)
	}
}

// Finding check (HTTP/2 only, not one of the five criteria): with a
// connection window of 2 stream windows, ONE paused reader leaves room for
// everyone (control), while THREE paused readers exhaust it and stall all
// other streams. This is the arithmetic stalled×streamWindow ≥ connWindow,
// measured rather than assumed.
func TestH2_PausedReadersExhaustConnWindow(t *testing.T) {
	for _, extra := range []int{0, 2} {
		e := env(t, EnvOpts{Transport: H2, Faults: Faults{H2ConnWindow: 2 * StreamWindow}})
		for i := 0; i < extra; i++ {
			d, err := StartPausedDownload(e, c2Size, uint64(50+i))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { d.resp.Body.Close() })
		}
		r, err := CheckC3(e, c2Size, 5*time.Second, 50)
		t.Logf("RESULT H2connwindow paused=%d err=%v %v", 1+extra, err, r)
		if extra == 0 && err != nil {
			t.Errorf("control: 1 paused reader should not stall a 2-window connection: %v", err)
		}
		if extra > 0 && (err == nil || r.AssetsOK == r.Assets) {
			t.Errorf("3 paused readers should exhaust a 2-window connection: %v", r)
		}
	}
}
