package main

import (
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/ZacxDev/muster/internal/mirrorhost"
)

// loadJobs reads -jobs-file (mirrorhost.LoadJobs): its jobs, or — when it is
// set and invalid — none and the reason, logged here and reported to muster
// (mirrorhost.Config.JobsError). Unset declares none, with no reason.
func loadJobs(path string, logger *log.Logger) ([]mirrorhost.JobSpec, string) {
	if path == "" {
		return nil, ""
	}
	jobs, err := mirrorhost.LoadJobs(expandHome(path))
	if err != nil {
		logger.Printf("muster-mirror: the jobs file declares NO job (mirroring continues): %v", err)
		return nil, err.Error()
	}
	logger.Printf("muster-mirror: %d scheduled job(s) declared in %s", len(jobs), path)
	return jobs, ""
}

// printJobs is `-list -jobs`: the jobs file validated, one line per job.
func printJobs(path string, stdout, stderr io.Writer) int {
	if path == "" {
		fmt.Fprintf(stderr, "muster-mirror: no jobs file (-jobs-file / %s)\n", envJobsFile)
		return 2
	}
	jobs, err := mirrorhost.LoadJobs(expandHome(path))
	if err != nil {
		fmt.Fprintf(stderr, "muster-mirror: %v\n", err)
		return 1
	}
	for _, j := range jobs {
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\n", j.Name, j.Cron, j.Timeout(), j.OutputFormat(), strings.Join(j.Command, " "))
	}
	return 0
}
