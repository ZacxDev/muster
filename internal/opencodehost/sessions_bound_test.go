package opencodehost

import (
	"context"
	"fmt"
	"testing"

	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// TestAListCutAtTheBoundSaysItIsNotWhole: AllSessions reports complete false
// when its page bound runs out with a page still to come — and true for a
// list that ended on its own, at and just under the bound.
//
// 🔴 REGRESSION TEST. At 2afa5ab8 a list cut at the bound was returned as if
// it were every session.
func TestAListCutAtTheBoundSaysItIsNotWhole(t *testing.T) {
	was := maxSessionPages
	maxSessionPages = 2
	t.Cleanup(func() { maxSessionPages = was })
	for _, c := range []struct {
		n, got   int
		complete bool
	}{
		{150, 150, true},  // ends on its second page
		{250, 200, false}, // a third page still to come
	} {
		srv := opencodetest.New("fixture-bound-password")
		for i := 0; i < c.n; i++ {
			srv.PutSession(fmt.Sprintf("ses_fixturebound%04d", i), fmt.Sprintf("bound fixture %d", i), "")
		}
		cl, err := NewClient(srv.URL, func() string { return srv.Issue() })
		if err != nil {
			t.Fatal(err)
		}
		list, complete, err := cl.AllSessions(context.Background())
		srv.Close()
		if err != nil || len(list) != c.got || complete != c.complete {
			t.Fatalf("%d sessions: read %d, complete %v (%v); want %d, %v", c.n, len(list), complete, err, c.got, c.complete)
		}
	}
}
