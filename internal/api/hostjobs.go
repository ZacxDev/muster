package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/hostjobs"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/pages"
)

// ---------------------------------------------------------------------------
// SCHEDULED HOST JOBS (docs/host-jobs.md): a host runs a FIXED command on a
// cron and its stdout becomes a version of a muster page. The protocol is
// internal/mirror's hostjobs.go, the host half internal/mirrorhost's, the
// store internal/hostjobs.
//
//	GET  /api/host-jobs                     the caller's declared jobs, per host
//	GET  /api/host-jobs/{name}/runs         ?host=&limit= a job's runs, newest first
//	POST /api/host-jobs/{name}/run          ?host= run it now
//	DELETE /api/host-jobs/{name}            ?host= forget it (a host that is gone)
//
// 🔴 MUSTER TRIGGERS, RECORDS AND ALERTS — IT NEVER DECLARES. A job exists
// here only because its host's jobs message reported it (hostJobsReported);
// a trigger names the job, never a command (mirror.Hub.TriggerJob sends the
// run id and the name), and the host runs its own copy of the argv.
//
// 🔴 MUSTER FIRES THE CRON, not the host: one scheduler (RunHostJobScheduler,
// the officer schedules' compare-and-set), so a fire while the host is OFFLINE
// is a run of its own (offline) with its Attention item — a missed run is
// seen, not silent.
//
// 🔴 ONE ATTENTION ITEM PER FAILED RUN: a non-zero exit, a timeout, an output
// that is not a valid page, a refusal, the host offline at fire time or a
// result that never came (lost) raises one notice, recorded on the run by a
// compare-and-set (hostjobs.Store.SetNotice), with the stderr tail where
// there is one. A run that succeeded raises none, and neither does a skip.
//
// 🔴 PAGES: a job writes its OWN page — one of its owner's pages, made by
// muster as its HOST (pages.Creator MirrorHost) on the job's first output; a
// host names no page. The page is ROLLING (0163's pages.rolling_versions): it
// keeps its newest pages.MaxVersionsPerPage versions, dropping the oldest
// unreferenced one, so it never fills. A new version is written only when the
// output CHANGED from the page's current version. A TEAM host's jobs are refused (hostJobsReported): pages are
// personal, so a page a team owned would be readable by none of its members.
//
// 🔴 NO HOST OFFICER, NO STEP-UP: nothing here declares an officer or marks a
// token, so declaring jobs leaves the host's mirror door exactly as it was
// (TestDeclaringJobsLeavesTheHostOutOfTheStepUp).
// ---------------------------------------------------------------------------

// mirrorJobs is what the job runner needs of the mirror plugin
// (*mirror.Service implements it).
type mirrorJobs interface {
	TriggerJob(ctx context.Context, owner int64, host string, run int64, job string) error
	HostConnected(owner int64, host string) bool
	JobsError(owner int64, host string) string
}

// HostJobScheduleInterval is how often the scheduler looks for due jobs and
// lost runs.
const HostJobScheduleInterval = 30 * time.Second

// hostJobBatch bounds one pass's fires and lost runs.
const hostJobBatch = 100

// hostJobRunsMax bounds `muster job runs`.
const hostJobRunsMax = 200

// EnableHostJobs turns scheduled host jobs on over st (muster-server, with a
// database and the mirror plugin).
func (s *Server) EnableHostJobs(st *hostjobs.Store) { s.hostJobs = st }

// hostJobClock is the job runner's clock (a test sets hostJobsNow).
func (s *Server) hostJobClock() time.Time {
	if s.hostJobsNow != nil {
		return s.hostJobsNow()
	}
	return time.Now()
}

// hostJobPrincipal is owner's principal for the job runner: authz.ViaHostJob,
// so row-level security confines every read and write to owner's rows.
// 🔴 false for a TEAM (pages are personal: a team's jobs are refused) and for
// an owner whose account cannot be read (it may be a team): the caller acts
// on nothing then.
func (s *Server) hostJobPrincipal(ctx context.Context, owner int64) (context.Context, bool) {
	p := s.principalFor(ctx, owner, authz.ViaHostJob)
	if p.Team || p.RoleUncertain {
		return ctx, false
	}
	return authz.WithPrincipal(ctx, p), true
}

// hostJobLabel is a job's Attention label and its pages' creator label.
func hostJobLabel(job, host string) string { return "job " + job + " on " + host }

// HostJobsReported is the hub's jobs hook (mirror.Hub.SetJobsNotify): owner's
// host's declared jobs, recorded as reported.
func (s *Server) HostJobsReported(owner int64, host string, r mirror.HostJobsReport) {
	if s.hostJobs == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, d := range r.Dropped {
		s.logger.Printf("host jobs: %s (owner %d): an entry of its jobs message was dropped: %s", host, owner, d)
	}
	if r.Error != "" {
		// What it declared before stands: a fire of a job it can no longer
		// read is refused by the host, and that reaches the owner.
		s.logger.Printf("host jobs: %s (owner %d) could not read its jobs file (its jobs as last reported stand): %s", host, owner, r.Error)
		return
	}
	octx, ok := s.hostJobPrincipal(ctx, owner)
	if !ok {
		if len(r.Jobs) > 0 {
			s.logger.Printf("host jobs: %s's %d job(s) REFUSED: its owner %d is a team (or could not be read), and a job's page is personal", host, len(r.Jobs), owner)
		}
		return
	}
	if err := s.hostJobs.Report(octx, host, r.Jobs, s.hostJobClock()); err != nil {
		// 🔴 NEVER SILENT: the jobs as last recorded stand (Report is one
		// transaction), and the owner is told — jobs_error in `job list`, and
		// one Attention item per host while the error lasts.
		s.logger.Printf("host jobs: record %s's jobs (owner %d): %v", host, owner, err)
		s.hostJobReportFailed(octx, owner, host, err)
		return
	}
	s.hostJobReportOK(octx, owner, host)
	// The host is connected: its jobs' open offline items are settled.
	s.withdrawHostJobOffline(octx, host, 0)
	s.logger.Printf("host jobs: %s (owner %d) declares %d job(s)", host, owner, len(r.Jobs))
}

// hostJobReportErrs is, per (owner, host), why muster could not record the
// host's last jobs message, and the Attention item that said so. In memory:
// the next jobs message (each reconnect) records or fails again.
type hostJobReportErrs struct {
	mu sync.Mutex
	m  map[hostJobReportKey]hostJobReportErr
}

type hostJobReportKey struct {
	owner int64
	host  string
}

type hostJobReportErr struct {
	err    string
	notice int64
}

// hostJobReportFailed records why owner's host's jobs could not be recorded
// and raises its ONE Attention item (none while an earlier one is open).
func (s *Server) hostJobReportFailed(octx context.Context, owner int64, host string, err error) {
	why := "muster could not record them (" + truncate(err.Error(), 300) + ")"
	k := hostJobReportKey{owner, host}
	s.hostJobReports.mu.Lock()
	if s.hostJobReports.m == nil {
		s.hostJobReports.m = map[hostJobReportKey]hostJobReportErr{}
	}
	prev := s.hostJobReports.m[k]
	s.hostJobReports.m[k] = hostJobReportErr{err: why, notice: prev.notice}
	s.hostJobReports.mu.Unlock()
	actx := s.attentionPrincipal(octx, owner)
	if s.ext.Attention == nil {
		return
	}
	if prev.notice > 0 {
		if a, err := s.ext.Attention.Get(actx, prev.notice); err == nil && a.Status == attention.Open {
			return
		}
	}
	a, cerr := s.ext.Attention.Create(actx, attention.New{Asker: attention.Asker{System: true, Label: "jobs on " + host},
		Question: hostJobReportText(host, why), Notice: true})
	if cerr != nil {
		s.logger.Printf("host jobs: raise %s's report notice: %v", host, cerr)
		return
	}
	s.hostJobReports.mu.Lock()
	s.hostJobReports.m[k] = hostJobReportErr{err: why, notice: a.ID}
	s.hostJobReports.mu.Unlock()
	s.askFiled(a)
}

// hostJobReportOK clears owner's host's report error and withdraws its item.
func (s *Server) hostJobReportOK(octx context.Context, owner int64, host string) {
	k := hostJobReportKey{owner, host}
	s.hostJobReports.mu.Lock()
	prev, had := s.hostJobReports.m[k]
	delete(s.hostJobReports.m, k)
	s.hostJobReports.mu.Unlock()
	if had && prev.notice > 0 && s.ext.Attention != nil {
		if _, err := s.cancelAsk(s.attentionPrincipal(octx, owner), prev.notice); err != nil && !errors.Is(err, attention.ErrNotOpen) {
			s.logger.Printf("host jobs: withdraw %s's report notice: %v", host, err)
		}
	}
}

// hostJobReportError is why owner's host's jobs could not be recorded, ""
// for none.
func (s *Server) hostJobReportError(owner int64, host string) string {
	s.hostJobReports.mu.Lock()
	defer s.hostJobReports.mu.Unlock()
	return s.hostJobReports.m[hostJobReportKey{owner, host}].err
}

// withdrawHostJobOffline withdraws the open offline items of host's jobs
// (job 0) or of one job: the host is connected again, or a run succeeded.
func (s *Server) withdrawHostJobOffline(octx context.Context, host string, job int64) {
	if s.ext.Attention == nil {
		return
	}
	ids, err := s.hostJobs.OfflineNotices(octx, host, job)
	if err != nil {
		s.logger.Printf("host jobs: read %s's offline items: %v", host, err)
		return
	}
	for _, id := range ids {
		if _, err := s.cancelAsk(octx, id); err != nil && !errors.Is(err, attention.ErrNotOpen) && !errors.Is(err, attention.ErrNotFound) {
			s.logger.Printf("host jobs: withdraw offline item %d: %v", id, err)
		}
	}
}

// raiseHostJobOffline is an offline run's item: consecutive offline fires of
// one job share ONE open item (each run records it), so a decommissioned or
// long-off host does not file one per fire.
func (s *Server) raiseHostJobOffline(octx context.Context, run hostjobs.Run, text string) {
	if s.ext.Attention != nil {
		if id, err := s.hostJobs.OpenOfflineNotice(octx, run.JobID, run.ID); err == nil && id > 0 {
			if a, err := s.ext.Attention.Get(octx, id); err == nil && a.Status == attention.Open {
				_, _ = s.hostJobs.SetNotice(octx, run.ID, id)
				return
			}
		}
	}
	s.raiseHostJobNotice(octx, run, text)
}

// RunHostJobScheduler fires due jobs and settles lost runs every interval
// until ctx ends. Returns at once while jobs are off.
func (s *Server) RunHostJobScheduler(ctx context.Context, interval time.Duration) {
	if s.hostJobs == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.hostJobsTick(ctx, s.hostJobClock())
		}
	}
}

// hostJobsTick is one scheduler pass at now: every due job fired (each by
// one replica only: FireDue's compare-and-set), then every overdue run
// called lost.
func (s *Server) hostJobsTick(ctx context.Context, now time.Time) {
	fires, err := s.hostJobs.FireDue(ctx, now, hostJobBatch)
	if err != nil {
		s.logger.Printf("host jobs: fire: %v", err)
	}
	for _, f := range fires {
		if _, err := s.startHostJobRun(ctx, f.Job, hostjobs.TriggerCron, f.DueAt); err != nil {
			s.logger.Printf("host jobs: %s on %s (owner %d): start its run: %v", f.Job.Name, f.Job.Host, f.Job.Owner, err)
		}
	}
	lost, err := s.hostJobs.LostRuns(ctx, now, hostJobBatch)
	if err != nil {
		s.logger.Printf("host jobs: lost runs: %v", err)
	}
	for _, r := range lost {
		octx, ok := s.hostJobPrincipal(ctx, r.Owner)
		if !ok {
			continue
		}
		s.logger.Printf("host jobs: %s on %s (owner %d): run %d never reported back: lost", r.Job, r.Host, r.Owner, r.ID)
		s.raiseHostJobNotice(octx, r, hostJobLostText(r))
	}
}

// startHostJobRun records a run of j (its Owner set) and, when it is one
// that runs, sends its host the job-run. due is when it was due (a manual
// run: now). It answers the run as recorded: running, skipped, offline — or
// refused / offline when the send itself failed.
func (s *Server) startHostJobRun(ctx context.Context, j hostjobs.Job, trigger string, due time.Time) (hostjobs.Run, error) {
	octx, ok := s.hostJobPrincipal(ctx, j.Owner)
	if !ok {
		return hostjobs.Run{}, errors.New("its owner is a team, or could not be read")
	}
	mj, _ := s.ext.Mirror.(mirrorJobs)
	online := mj != nil && mj.HostConnected(j.Owner, j.Host)
	now := s.hostJobClock()
	r, err := s.hostJobs.StartRun(octx, j, trigger, now, online)
	if err != nil {
		return hostjobs.Run{}, err
	}
	switch r.Status {
	case hostjobs.StatusSkipped:
		s.logger.Printf("host jobs: %s on %s (owner %d): run %d skipped: its previous run is still going", j.Name, j.Host, j.Owner, r.ID)
		return r, nil
	case hostjobs.StatusOffline:
		s.logger.Printf("host jobs: %s on %s (owner %d): run %d not started: the host is offline", j.Name, j.Host, j.Owner, r.ID)
		s.raiseHostJobOffline(octx, r, hostJobOfflineText(r.Job, r.Host, due))
		return r, nil
	}
	terr := mj.TriggerJob(octx, j.Owner, j.Host, r.ID, j.Name)
	if terr == nil {
		s.logger.Printf("host jobs: %s on %s (owner %d): run %d sent (%s)", j.Name, j.Host, j.Owner, r.ID, trigger)
		return r, nil
	}
	fin := hostjobs.Finish{EndedAt: now, Status: hostjobs.StatusRefused, Detail: terr.Error()}
	text := hostJobRefusedText(r.Job, r.Host, terr.Error())
	if errors.Is(terr, mirror.ErrHostOffline) {
		fin.Status, fin.Detail = hostjobs.StatusOffline, "the host was not connected"
		text = hostJobOfflineText(r.Job, r.Host, due)
	}
	done, won, err := s.hostJobs.FinishRun(octx, r.ID, fin)
	if err != nil || !won {
		return r, err
	}
	s.logger.Printf("host jobs: %s on %s (owner %d): run %d not sent: %v", j.Name, j.Host, j.Owner, r.ID, terr)
	if done.Status == hostjobs.StatusOffline {
		s.raiseHostJobOffline(octx, done, text)
	} else {
		s.raiseHostJobNotice(octx, done, text)
	}
	return done, nil
}

// HostJobResult is the hub's job-result hook (mirror.Hub.SetJobResultNotify):
// owner's host reports how run res.Run ended. 🔴 It is applied only to a run
// of THIS owner, of THIS host and THIS job, that is still running: a result
// for anything else — another host's run, a finished run, a run that was
// never sent — is dropped (logged).
func (s *Server) HostJobResult(owner int64, host string, res mirror.JobResult) {
	if s.hostJobs == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	octx, ok := s.hostJobPrincipal(ctx, owner)
	if !ok {
		s.logger.Printf("host jobs: a job-result from %s dropped: its owner %d is a team, or could not be read", host, owner)
		return
	}
	r, err := s.hostJobs.Run(octx, res.Run)
	if err != nil || r.Host != host || r.Job != res.Job || r.Status != hostjobs.StatusRunning {
		s.logger.Printf("host jobs: a job-result from %s for run %d (job %q) dropped: it names no running run of that job on that host", host, res.Run, res.Job)
		return
	}
	j, err := s.hostJobs.Job(octx, r.JobID)
	if err != nil {
		s.logger.Printf("host jobs: run %d: read its job: %v", r.ID, err)
		return
	}
	now := s.hostJobClock()
	// 🔴 CLAIMED BEFORE ANYTHING IS WRITTEN: of two results for one run (a
	// duplicate, or the lost sweep), only the claim's winner writes a page
	// version and records the ending.
	if won, err := s.hostJobs.ClaimRun(octx, r.ID, now); err != nil || !won {
		if err != nil {
			s.logger.Printf("host jobs: run %d: claim its ending: %v", r.ID, err)
		}
		return
	}
	fin := hostjobs.Finish{EndedAt: now, StderrTail: string(res.Stderr)}
	if res.StartedMs > 0 {
		t := time.UnixMilli(res.StartedMs)
		fin.StartedAt = &t
		if res.EndedMs >= res.StartedMs {
			fin.EndedAt = time.UnixMilli(res.EndedMs)
			d := res.EndedMs - res.StartedMs
			fin.DurationMs = &d
		}
	}
	if res.Refusal == "" {
		code := res.ExitCode
		fin.ExitCode = &code
	}
	tail := hostjobs.CleanTail(string(res.Stderr))
	var text string
	switch {
	case res.Refusal != "":
		fin.Status, fin.Detail = hostjobs.StatusRefused, res.Refusal+": "+res.Error
		text = hostJobRefusedText(r.Job, r.Host, res.Refusal+": "+res.Error)
	case res.TimedOut:
		fin.Status, fin.Detail = hostjobs.StatusTimeout, "stopped at its timeout"
		text = hostJobTimeoutText(r.Job, r.Host, time.Duration(r.TimeoutS)*time.Second, tail)
	case res.Error != "":
		fin.Status, fin.Detail = hostjobs.StatusFailed, res.Error
		text = hostJobFailedText(r.Job, r.Host, res.Error, tail)
	case res.ExitCode != 0:
		fin.Status, fin.Detail = hostjobs.StatusFailed, "exited with status "+strconv.Itoa(res.ExitCode)
		text = hostJobExitText(r.Job, r.Host, res.ExitCode, tail)
	default:
		page, version, status, detail := s.writeHostJobPage(octx, j, res.Stdout)
		fin.Status, fin.PageID, fin.PageVersion, fin.Detail = status, page, version, detail
		switch status {
		case hostjobs.StatusInvalid:
			text = hostJobInvalidText(r.Job, r.Host, detail, tail)
		case hostjobs.StatusFailed:
			text = hostJobFailedText(r.Job, r.Host, detail, tail)
		}
	}
	done, won, err := s.hostJobs.FinishRun(octx, r.ID, fin)
	if err != nil {
		s.logger.Printf("host jobs: run %d: record its end: %v", r.ID, err)
		return
	}
	if !won {
		return
	}
	s.logger.Printf("host jobs: %s on %s (owner %d): run %d %s (%s)", r.Job, r.Host, owner, r.ID, done.Status, done.Detail)
	if text != "" {
		s.raiseHostJobNotice(octx, done, text)
		return
	}
	if done.Status == hostjobs.StatusOK {
		// A run that succeeded settles the job's open offline item.
		s.withdrawHostJobOffline(octx, r.Host, r.JobID)
	}
}

// writeHostJobPage writes stdout as j's page when it changed, and answers the
// page, the version written (0: none, unchanged), the run's status and its
// detail.
func (s *Server) writeHostJobPage(ctx context.Context, j hostjobs.Job, stdout []byte) (page int64, version int, status, detail string) {
	decl := j.Decl()
	var blocks []pages.Block
	var size int
	var err error
	switch decl.OutputFormat() {
	case mirror.JobOutputBlocks:
		blocks, size, err = pages.Parse(stdout)
	default:
		blocks, size, err = pages.Validate([]pages.Block{{Type: pages.BlockMarkdown, Text: string(stdout)}})
	}
	if err != nil {
		return 0, 0, hostjobs.StatusInvalid, strings.ReplaceAll(err.Error(), pages.ErrInvalid.Error()+": ", "")
	}
	if !s.pagesOn() {
		return 0, 0, hostjobs.StatusFailed, "shareable pages are not on for this server (MUSTER_PAGES), so it has no page to write"
	}
	title, err := pages.CleanTitle(decl.PageTitle())
	if err != nil {
		return 0, 0, hostjobs.StatusFailed, "its page title is not valid: " + strings.TrimPrefix(err.Error(), pages.ErrInvalid.Error()+": ")
	}
	// The job's own page: made by muster on the job's first output, as its
	// host, and ROLLING (pages.Creator.RollingVersions): it keeps its newest
	// pages.MaxVersionsPerPage versions, so a job whose output changes every
	// run never fills it and its id and links never change.
	creator := pages.Creator{MirrorHost: j.Host, Label: hostJobLabel(j.Name, j.Host), RollingVersions: true}
	target := j.PageID
	if target > 0 {
		_, perr := s.ext.Pages.PageFor(ctx, creator, target)
		switch {
		case errors.Is(perr, pages.ErrNotFound):
			target = 0 // the page muster made is gone: make another
		case perr != nil:
			return 0, 0, hostjobs.StatusFailed, "its page could not be read"
		default:
			if cur, err := s.ext.Pages.Version(ctx, target, 0); err == nil && sameVersion(cur, title, blocks) {
				return target, 0, hostjobs.StatusOK, "unchanged: no new version"
			}
		}
	}
	if target > 0 {
		_, v, err := s.ext.Pages.AddVersion(ctx, creator, target, title, blocks, size)
		if err != nil {
			return target, 0, hostjobs.StatusFailed, "its page could not be updated: " + pageStoreWords(err)
		}
		return target, v.Version, hostjobs.StatusOK, "wrote version " + strconv.Itoa(v.Version)
	}
	p, v, err := s.ext.Pages.Create(ctx, creator, title, blocks, size)
	if err != nil {
		return 0, 0, hostjobs.StatusFailed, "its page could not be made: " + pageStoreWords(err)
	}
	if err := s.hostJobs.SetPage(ctx, j.ID, p.ID); err != nil {
		s.logger.Printf("host jobs: %s on %s: record its page %d: %v", j.Name, j.Host, p.ID, err)
	}
	return p.ID, v.Version, hostjobs.StatusOK, "made page " + strconv.FormatInt(p.ID, 10)
}

// pageStoreWords is a pages store error as a run's detail.
func pageStoreWords(err error) string {
	switch {
	case errors.Is(err, pages.ErrTooMany):
		return strings.TrimPrefix(err.Error(), pages.ErrTooMany.Error()+": ")
	case errors.Is(err, pages.ErrInvalid):
		return strings.TrimPrefix(err.Error(), pages.ErrInvalid.Error()+": ")
	case errors.Is(err, pages.ErrNotFound):
		return "no such page"
	}
	return "the store refused it"
}

// sameVersion reports whether version v already IS title and blocks: the
// "only when it changed" rule.
func sameVersion(v pages.Version, title string, blocks []pages.Block) bool {
	if v.Title != title {
		return false
	}
	a, err1 := json.Marshal(v.Blocks)
	b, err2 := json.Marshal(blocks)
	return err1 == nil && err2 == nil && string(a) == string(b)
}

// raiseHostJobNotice files run's ONE Attention item: a notice muster raises
// itself, recorded on the run by a compare-and-set (a second copy is
// withdrawn). octx acts as the run's owner.
func (s *Server) raiseHostJobNotice(octx context.Context, run hostjobs.Run, text string) {
	if s.ext.Attention == nil {
		s.logger.Printf("host jobs: %s on %s: run %d needs its owner, and this server has no attention store: %s", run.Job, run.Host, run.ID, text)
		return
	}
	actx := s.attentionPrincipal(octx, run.Owner)
	a, err := s.ext.Attention.Create(actx, attention.New{Asker: attention.Asker{System: true, Label: hostJobLabel(run.Job, run.Host)}, Question: text, Notice: true})
	if err != nil {
		s.logger.Printf("host jobs: %s on %s: raise run %d's notice: %v", run.Job, run.Host, run.ID, err)
		return
	}
	won, err := s.hostJobs.SetNotice(octx, run.ID, a.ID)
	if err != nil || !won {
		if _, cerr := s.cancelAsk(actx, a.ID); cerr != nil && !errors.Is(cerr, attention.ErrNotOpen) {
			s.logger.Printf("host jobs: withdraw a duplicate notice %d for run %d: %v", a.ID, run.ID, cerr)
		}
		return
	}
	s.askFiled(a)
}

// --- the Attention items' words (each pinned literally by a test) -----------

// hostJobTail is the stderr part of a failure's item.
func hostJobTail(tail string) string {
	tail = strings.TrimRight(tail, " \t\r\n")
	if tail == "" {
		return "\n\nIts stderr was empty."
	}
	return "\n\nThe end of its stderr:\n" + tail
}

func hostJobOfflineText(job, host string, due time.Time) string {
	return fmt.Sprintf("Job %s on %s did not run at %s: %s was not connected to muster.", job, host, due.UTC().Format("2006-01-02 15:04 UTC"), host)
}

func hostJobExitText(job, host string, code int, tail string) string {
	return fmt.Sprintf("Job %s on %s failed: it exited with status %d.", job, host, code) + hostJobTail(tail)
}

func hostJobTimeoutText(job, host string, timeout time.Duration, tail string) string {
	return fmt.Sprintf("Job %s on %s timed out: the host stopped it after its %s limit.", job, host, timeout) + hostJobTail(tail)
}

// maxHostJobWhyRunes bounds the reason an item quotes (a pages.Parse error
// can quote its input), so the item — header, reason and the stderr tail —
// stays inside attention.MaxQuestionRunes.
const maxHostJobWhyRunes = 1000

// hostJobWhy is why bounded to maxHostJobWhyRunes.
func hostJobWhy(why string) string {
	if r := []rune(why); len(r) > maxHostJobWhyRunes {
		return string(r[:maxHostJobWhyRunes]) + "…"
	}
	return why
}

// hostJobReportText is the item for a host whose jobs muster could not record.
func hostJobReportText(host, why string) string {
	return fmt.Sprintf("The jobs %s declares were not recorded: %s. Its jobs as muster last recorded them stand; `muster job list` shows this until its next connection records them.", host, why)
}

func hostJobInvalidText(job, host, why, tail string) string {
	why = hostJobWhy(why)
	return fmt.Sprintf("Job %s on %s ran, but its output is not a valid page, so the page was not changed: %s.", job, host, why) + hostJobTail(tail)
}

func hostJobFailedText(job, host, why, tail string) string {
	why = hostJobWhy(why)
	return fmt.Sprintf("Job %s on %s failed: %s.", job, host, why) + hostJobTail(tail)
}

func hostJobRefusedText(job, host, why string) string {
	why = hostJobWhy(why)
	return fmt.Sprintf("Job %s on %s did not run: %s refused it (%s).", job, host, host, why)
}

func hostJobLostText(r hostjobs.Run) string {
	return fmt.Sprintf("Job %s on %s was sent at %s, and muster never got its result back (it was due within %s). Check muster-mirror's log on %s.",
		r.Job, r.Host, r.FiredAt.UTC().Format("2006-01-02 15:04 UTC"), time.Duration(r.TimeoutS)*time.Second, r.Host)
}

// --- the routes (`muster job …`) ----------------------------------------------

func (s *Server) registerHostJobRoutes(mux Mux) {
	h := func(f http.HandlerFunc) http.HandlerFunc { return s.requireArmedHookToken(s.requireHostJobs(f)) }
	mux.HandleFunc("GET /api/host-jobs", h(s.handleHostJobs))
	mux.HandleFunc("GET /api/host-jobs/{name}/runs", h(s.handleHostJobRuns))
	mux.HandleFunc("POST /api/host-jobs/{name}/run", h(s.handleHostJobRun))
	mux.HandleFunc("DELETE /api/host-jobs/{name}", h(s.handleHostJobForget))
}

// handleHostJobForget is DELETE /api/host-jobs/{name}?host= (`muster job
// forget <host> <name>`): the caller's job and its runs are deleted — its
// pending schedule with it — and its runs' open Attention items withdrawn.
// For a host that is GONE: a host still declaring the job records it again on
// its next connection. ?host= is required (a name may be declared on several
// hosts). The caller's own rows only (row-level security): another user's job
// reads as missing, 404.
func (s *Server) handleHostJobForget(w http.ResponseWriter, r *http.Request) {
	name, host := r.PathValue("name"), r.URL.Query().Get("host")
	if !mirror.ValidJobName(name) || mirror.ValidHost(host) != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "forget names a job and its host: DELETE /api/host-jobs/{name}?host=<host>"})
		return
	}
	notices, err := s.hostJobs.Forget(r.Context(), host, name)
	switch {
	case errors.Is(err, hostjobs.ErrNotFound):
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "you have no job of that name on that host"})
		return
	case err != nil:
		s.logger.Printf("host jobs: forget %s on %s: %v", name, host, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the job could not be forgotten"})
		return
	}
	if s.ext.Attention != nil {
		for _, id := range notices {
			if _, err := s.cancelAsk(r.Context(), id); err != nil && !errors.Is(err, attention.ErrNotOpen) && !errors.Is(err, attention.ErrNotFound) {
				s.logger.Printf("host jobs: forget %s on %s: withdraw item %d: %v", name, host, id, err)
			}
		}
	}
	s.logger.Printf("host jobs: %s on %s forgotten (owner %d)", name, host, adminOwner(r))
	s.writeJSON(w, http.StatusOK, map[string]any{"forgot": name, "host": host})
}

// requireHostJobs answers 404 while jobs are off.
func (s *Server) requireHostJobs(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.hostJobs == nil {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "scheduled host jobs are off: they need a database and the mirror plugin (MUSTER_MIRROR=1)"})
			return
		}
		next(w, r)
	}
}

// hostJobHost is one host's part of GET /api/host-jobs.
type hostJobHost struct {
	Host      string         `json:"host"`
	Online    bool           `json:"online"`
	JobsError string         `json:"jobs_error,omitempty"`
	Jobs      []hostjobs.Job `json:"jobs"`
}

// handleHostJobs is GET /api/host-jobs: the caller's declared jobs, per host,
// as each host last reported them, with whether it is connected and — for a
// connected host whose jobs file could not be read — why.
func (s *Server) handleHostJobs(w http.ResponseWriter, r *http.Request) {
	owner := adminOwner(r)
	jobs, err := s.hostJobs.Jobs(r.Context())
	if err != nil {
		s.logger.Printf("host jobs: list: %v", err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the jobs could not be read"})
		return
	}
	mj, _ := s.ext.Mirror.(mirrorJobs)
	byHost := map[string]*hostJobHost{}
	var order []string
	add := func(host string) *hostJobHost {
		if h := byHost[host]; h != nil {
			return h
		}
		h := &hostJobHost{Host: host, Jobs: []hostjobs.Job{}}
		if mj != nil {
			h.Online, h.JobsError = mj.HostConnected(owner, host), mj.JobsError(owner, host)
		}
		if h.JobsError == "" {
			h.JobsError = s.hostJobReportError(owner, host)
		}
		byHost[host] = h
		order = append(order, host)
		return h
	}
	for _, j := range jobs {
		h := add(j.Host)
		h.Jobs = append(h.Jobs, j)
	}
	if s.ext.Mirror != nil && mj != nil {
		for _, v := range s.ext.Mirror.Hosts(r.Context()) {
			if byHost[v.Host] == nil && ((v.Online && mj.JobsError(owner, v.Host) != "") || s.hostJobReportError(owner, v.Host) != "") {
				add(v.Host)
			}
		}
	}
	out := make([]*hostJobHost, 0, len(order))
	for _, h := range order {
		out = append(out, byHost[h])
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"hosts": out})
}

// hostJobByName resolves {name} (and ?host=) to ONE of the caller's declared
// jobs, answering the request itself when it cannot (404 none, 409 several).
func (s *Server) hostJobByName(w http.ResponseWriter, r *http.Request) (hostjobs.Job, bool) {
	name, host := r.PathValue("name"), r.URL.Query().Get("host")
	if !mirror.ValidJobName(name) || (host != "" && mirror.ValidHost(host) != nil) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a job's name is lowercase letters, digits and '-'; ?host= is a host's name"})
		return hostjobs.Job{}, false
	}
	jobs, err := s.hostJobs.JobsNamed(r.Context(), name, host)
	if err != nil {
		s.logger.Printf("host jobs: read job %s: %v", name, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the job could not be read"})
		return hostjobs.Job{}, false
	}
	switch len(jobs) {
	case 0:
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "no declared job " + strconv.Quote(name) + hostSuffix(host)})
		return hostjobs.Job{}, false
	case 1:
		return jobs[0], true
	}
	hosts := make([]string, 0, len(jobs))
	for _, j := range jobs {
		hosts = append(hosts, j.Host)
	}
	s.writeJSON(w, http.StatusConflict, map[string]any{"error": "job " + strconv.Quote(name) + " is declared on several hosts (" + strings.Join(hosts, ", ") + "): name one with --host"})
	return hostjobs.Job{}, false
}

func hostSuffix(host string) string {
	if host == "" {
		return ""
	}
	return " on " + host
}

// handleHostJobRuns is GET /api/host-jobs/{name}/runs: the job's runs,
// newest first.
func (s *Server) handleHostJobRuns(w http.ResponseWriter, r *http.Request) {
	j, ok := s.hostJobByName(w, r)
	if !ok {
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > hostJobRunsMax {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "limit is 1.." + strconv.Itoa(hostJobRunsMax)})
			return
		}
		limit = n
	}
	runs, err := s.hostJobs.Runs(r.Context(), j.ID, limit)
	if err != nil {
		s.logger.Printf("host jobs: runs of %s: %v", j.Name, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the runs could not be read"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"host": j.Host, "job": j.Name, "runs": runs})
}

// handleHostJobRun is POST /api/host-jobs/{name}/run: run the job now. It
// answers the run as recorded — running (sent to the host; `job runs` shows
// how it ends), skipped (its previous run is still going), offline or
// refused (with its Attention item, as a fire's).
func (s *Server) handleHostJobRun(w http.ResponseWriter, r *http.Request) {
	j, ok := s.hostJobByName(w, r)
	if !ok {
		return
	}
	run, err := s.startHostJobRun(r.Context(), j, hostjobs.TriggerManual, s.hostJobClock())
	if err != nil {
		s.logger.Printf("host jobs: run %s on %s now: %v", j.Name, j.Host, err)
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the run could not be started"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"host": j.Host, "job": j.Name, "run": run})
}
