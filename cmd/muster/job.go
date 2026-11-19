package main

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"
)

// `job …` — scheduled host jobs (docs/host-jobs.md): the jobs your hosts
// declare in their muster-mirror jobs file, their runs, and running one now.
//
// 🔴 NO VERB HERE DECLARES OR CHANGES A JOB. A job is declared on its host
// (muster-mirror -jobs-file) and nowhere else; muster can only trigger it by
// name, record its runs and raise an Attention item when one fails.

const (
	hostJobsRoute    = "/api/host-jobs"
	hostJobRunsRoute = "/api/host-jobs/{name}/runs"
	hostJobRunRoute  = "/api/host-jobs/{name}/run"
	hostJobRoute     = "/api/host-jobs/{name}"
)

func newJobCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "job",
		Short: "Scheduled host jobs: the fixed commands your hosts run on a cron and post to a page",
		Long: `A scheduled host job is a fixed command a mirrored host runs on a cron (UTC),
whose stdout becomes a new version of a muster page when it changed. A job is
declared ON ITS HOST, in muster-mirror's jobs file (-jobs-file); muster fires
it, records each run and raises an Attention item when one fails (a non-zero
exit, a timeout, an output that is not a valid page, the host offline at fire
time). Nothing here can add, change or parameterise a job.`,
	}
	cmd.AddCommand(newJobListCmd(a), newJobRunsCmd(a), newJobRunCmd(a), newJobForgetCmd(a))
	return cmd
}

func newJobListCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "GET " + hostJobsRoute + " — your hosts' declared jobs, per host, as each host reported them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.jobCall(cmd, http.MethodGet, hostJobsRoute, nil, nil)
		},
	}
}

func newJobRunsCmd(a *app) *cobra.Command {
	var host string
	var limit int
	cmd := &cobra.Command{
		Use:   "runs <name>",
		Short: "GET " + hostJobRunsRoute + " — a job's runs, newest first: status, start, end, exit code, duration, version written, stderr tail",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if host != "" {
				q.Set("host", host)
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			return a.jobCall(cmd, http.MethodGet, hostJobRunsRoute, map[string]string{"name": args[0]}, q)
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "the host, when the name is declared on several")
	cmd.Flags().IntVar(&limit, "limit", 0, "at most this many runs (default 50, at most 200)")
	return cmd
}

func newJobRunCmd(a *app) *cobra.Command {
	var host string
	cmd := &cobra.Command{
		Use:   "run <name>",
		Short: "POST " + hostJobRunRoute + " — run a job now (skipped while its previous run is still going)",
		Long: `Run a declared job now, as its cron would. The answer is the run as muster
recorded it: running (sent to the host; ` + "`job runs`" + ` shows how it ends), skipped
(its previous run is still going), offline or refused (each with an Attention
item, as a fire's).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := url.Values{}
			if host != "" {
				q.Set("host", host)
			}
			return a.jobCall(cmd, http.MethodPost, hostJobRunRoute, map[string]string{"name": args[0]}, q)
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "the host, when the name is declared on several")
	return cmd
}

func newJobForgetCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "forget <host> <name>",
		Short: "DELETE " + hostJobRoute + "?host= — forget a job of a host that is gone: its row, its runs and its schedule",
		Long: `Forget one of your jobs for good: its record, its run history and its
pending schedule are deleted, and its runs' open Attention items withdrawn, so
a host you decommissioned stops raising "did not run" items. For a host that
is GONE: a host that still declares the job records it again on its next
connection (remove it from that host's jobs file instead). Your own jobs only.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.jobCall(cmd, http.MethodDelete, hostJobRoute, map[string]string{"name": args[1]}, url.Values{"host": {args[0]}})
		},
	}
}

// jobCall makes one host-jobs API call as the configured credential and
// prints its JSON.
func (a *app) jobCall(cmd *cobra.Command, method, tmpl string, params map[string]string, query url.Values) error {
	path, err := expandPath(tmpl, params)
	if err != nil {
		return err
	}
	raw, err := a.call(cmd.Context(), request{method: method, path: path, query: query, cred: credHook})
	if err != nil {
		return err
	}
	return a.emit(raw)
}
