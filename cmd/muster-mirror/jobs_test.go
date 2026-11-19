package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheJobsFileIsReadFromItsVariableAndCheckedByListJobs: `-list -jobs`
// reads MUSTER_MIRROR_JOBS_FILE (the managed unit's way in), prints one line
// per declared job and dials nothing; an invalid file is exit 1 naming why;
// no file is exit 2. And the daemon's loader declares nothing from an invalid
// file, keeping its reason for muster, while an unset variable declares
// nothing with no reason.
func TestTheJobsFileIsReadFromItsVariableAndCheckedByListJobs(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "jobs.json")
	if err := os.WriteFile(good, []byte(`{"jobs":[{"name":"smoke","command":["/usr/bin/env","date","-u"],"cron":"0 14 * * *","timeoutSeconds":60}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"jobs":[{"name":"smoke","command":["date"],"cron":"0 14 * * *"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(file string) func(string) string {
		return func(k string) string {
			if k == envJobsFile {
				return file
			}
			return ""
		}
	}
	var out, errb bytes.Buffer
	if rc := run([]string{"-list", "-jobs"}, env(good), &out, &errb); rc != 0 {
		t.Fatalf("a valid file: exit %d, stderr %q", rc, errb.String())
	}
	if want := "smoke\t0 14 * * *\t1m0s\tmarkdown\tpage=new\t/usr/bin/env date -u\n"; out.String() != want {
		t.Fatalf("stdout %q; want %q", out.String(), want)
	}
	out.Reset()
	errb.Reset()
	if rc := run([]string{"-list", "-jobs"}, env(bad), &out, &errb); rc != 1 || !strings.Contains(errb.String(), "not an absolute path") || out.Len() != 0 {
		t.Fatalf("an invalid file: exit %d, stdout %q, stderr %q; want 1 naming why", rc, out.String(), errb.String())
	}
	errb.Reset()
	if rc := run([]string{"-list", "-jobs"}, env(""), &out, &errb); rc != 2 {
		t.Fatalf("no file: exit %d; want 2", rc)
	}

	logger := log.New(io.Discard, "", 0)
	if jobs, why := loadJobs(bad, logger); jobs != nil || !strings.Contains(why, "not an absolute path") {
		t.Fatalf("an invalid file: %+v %q; want no job and its reason", jobs, why)
	}
	if jobs, why := loadJobs(good, logger); len(jobs) != 1 || why != "" {
		t.Fatalf("a valid file: %+v %q", jobs, why)
	}
	if jobs, why := loadJobs("", logger); jobs != nil || why != "" {
		t.Fatalf("no file: %+v %q; want nothing and no reason", jobs, why)
	}
}
