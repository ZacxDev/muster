package api

import (
	"strconv"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/hostjobs"
	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/ZacxDev/muster/internal/pages"
)

// TestAJobsPageTakes105ChangingOutputsAndKeepsItsID: a job whose output
// changes on every run, fired 105 times, writes 105 versions — none refused —
// to ONE page, which holds the newest pages.MaxVersionsPerPage of them, keeps
// its id, and shows the last output.
func TestAJobsPageTakes105ChangingOutputsAndKeepsItsID(t *testing.T) {
	f := newJobsFixture(t)
	rec := &stdoutRecorder{}
	f.connect(t, f.owner, f.host, []mirrorhost.JobSpec{declared("tick", "* * * * *", "/usr/bin/env", "true")}, rec.exec)
	j := f.waitJobs(t, f.host, 1)[0]
	var page int64
	for i := 1; i <= 105; i++ {
		rec.set("# Run " + strconv.Itoa(i))
		f.tick(jobsT0.Add(time.Duration(i) * time.Minute))
		rs := f.settled(t, j, i)
		if rs[0].Status != hostjobs.StatusOK || rs[0].PageVersion != i {
			t.Fatalf("run %d: %+v; want ok, version %d", i, rs[0], i)
		}
		if page == 0 {
			page = rs[0].PageID
		}
		if rs[0].PageID != page {
			t.Fatalf("run %d wrote page %d; the job's page is %d", i, rs[0].PageID, page)
		}
	}
	vs := f.pageVersions(t, page)
	if len(vs) != pages.MaxVersionsPerPage || vs[0].Version != 105 || vs[len(vs)-1].Version != 6 {
		t.Fatalf("the job's page holds %d versions, %d..%d; want %d, 6..105", len(vs), vs[len(vs)-1].Version, vs[0].Version, pages.MaxVersionsPerPage)
	}
	if v, _ := f.srv.ext.Pages.Version(f.ownerCtx(), page, 0); v.Blocks[0].Text != "# Run 105" {
		t.Fatalf("the page shows %q", v.Blocks[0].Text)
	}
	if n := len(f.notices(t, "tick", f.host)); n != 0 {
		t.Fatalf("%d Attention items over 105 good runs", n)
	}
}
