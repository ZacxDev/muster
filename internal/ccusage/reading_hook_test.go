package ccusage

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestEveryGoodReadingIsHandedToReadingWithItsMarksDetail: Poller.Reading
// (internal/limitpark's feed) gets each account's GOOD reading of the pass,
// with the detail of the mark it was read against — alpha's weekly mark, and
// "" for beta, which was not cooling. A failed attempt is not handed on.
func TestEveryGoodReadingIsHandedToReadingWithItsMarksDetail(t *testing.T) {
	for _, tc := range []struct {
		name string
		fx   *scripted
		want string
	}{
		{"both read", &scripted{}, "alpha|" + liveWeekly + "|allowed\nbeta||allowed"},
		{"the probes fail", &scripted{probe: []Result{{Status: http.StatusInternalServerError}, {Status: http.StatusInternalServerError}}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newLiveRig(t, tc.fx)
			var got []string
			rig.poller.Reading = func(_ context.Context, owner int64, account string, row Row, detail string) {
				if owner != 1 || row.ReadAt.IsZero() || !row.ReadAt.Equal(row.CheckedAt) {
					t.Errorf("Reading(%d, %s): row %+v is not a reading taken by this attempt", owner, account, row)
				}
				got = append(got, account+"|"+detail+"|"+row.Status)
			}
			if err := rig.poller.Pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			if s := strings.Join(got, "\n"); s != tc.want {
				t.Fatalf("Reading got\n%s\nwant\n%s", s, tc.want)
			}
		})
	}
}
