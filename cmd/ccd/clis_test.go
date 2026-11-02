package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/ZacxDev/muster/internal/agents"
)

// writeProc writes a fake /proc/<pid>/stat with state and start time.
func writeProc(t *testing.T, root string, pid int, state, start string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Fields after the comm: state ppid pgrp session tty ... starttime is #22.
	stat := strconv.Itoa(pid) + " (claude) " + state + " 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 " + start + " 0 0"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeSession writes Claude Code's sessions file for pid.
func writeSession(t *testing.T, cfg string, pid int, sid, status, procStart string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cfg, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": sid, "status": status, "procStart": procStart})
	if err := os.WriteFile(filepath.Join(cfg, "sessions", strconv.Itoa(pid)+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTheCLIsAreReadFromTheirOwnSessionsFiles: every LIVE CLI's own status is
// listed — the primary, a lane, and one ccd cannot name (a job's process, a
// background job) — with the lane each belongs to; a dead pid's file, a
// zombie's and a reused pid's (start time differs) are left out. Statuses are
// the CLI's own words, distinct per CLI.
func TestTheCLIsAreReadFromTheirOwnSessionsFiles(t *testing.T) {
	cfg, root := t.TempDir(), t.TempDir()
	const prim, lane, stray = "aaaaaaaa-0000-4000-8000-000000000001", "aaaaaaaa-0000-4000-8000-000000000002", "aaaaaaaa-0000-4000-8000-000000000003"
	writeSession(t, cfg, 101, prim, "idle", "500")
	writeProc(t, root, 101, "S", "500")
	writeSession(t, cfg, 102, lane, "busy", "")
	writeProc(t, root, 102, "S", "501")
	writeSession(t, cfg, 103, stray, "shell", "502")
	writeProc(t, root, 103, "R", "502")
	writeSession(t, cfg, 104, "aaaaaaaa-0000-4000-8000-000000000004", "busy", "") // no process
	writeSession(t, cfg, 105, "aaaaaaaa-0000-4000-8000-000000000005", "busy", "")
	writeProc(t, root, 105, "Z", "503") // a zombie
	writeSession(t, cfg, 106, "aaaaaaaa-0000-4000-8000-000000000006", "busy", "600")
	writeProc(t, root, 106, "S", "601") // the pid was reused
	known := map[string]cliEntry{prim: {Primary: true, Known: true}, lane: {Lane: "agent:chief:webchat:7", Known: true}}
	got := liveCLIs(cfg, root, known)
	want := []cliEntry{
		{Primary: true, Known: true, SessionID: prim, Status: "idle"},
		{Lane: "agent:chief:webchat:7", Known: true, SessionID: lane, Status: "busy"},
		{SessionID: stray, Status: "shell"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("live CLIs\n got %+v\nwant %+v", got, want)
	}
}

// TestKnownCLIsNameThePrimaryAndEachLane: the session id each CLI reported at
// SessionStart maps to the primary or to its lane's session key; a lane whose
// CLI has not started yet maps nothing.
func TestKnownCLIsNameThePrimaryAndEachLane(t *testing.T) {
	s := &server{sessionID: "sid-primary"}
	s.lanes = &lanes{set: map[string]*lane{
		"l1": {key: "agent:chief:webchat:one", srv: &server{sessionID: "sid-one"}},
		"l2": {key: "agent:chief:webchat:two", srv: &server{sessionID: "sid-two"}},
		"l3": {key: "agent:chief:webchat:starting", srv: &server{}},
	}}
	want := map[string]cliEntry{
		"sid-primary": {Primary: true, Known: true},
		"sid-one":     {Lane: "agent:chief:webchat:one", Known: true},
		"sid-two":     {Lane: "agent:chief:webchat:two", Known: true},
	}
	if got := s.knownCLIs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("known CLIs\n got %+v\nwant %+v", got, want)
	}
}

// TestTheCLIsAnswerIsMusters: muster's own client (agentgateway CLIActivity)
// reads ccd's answer, with ccd's version, through the bearer; a wrong token is
// refused.
func TestTheCLIsAnswerIsMusters(t *testing.T) {
	if cliActivityVersion != agents.CLIActivityVersion {
		t.Fatalf("ccd answers version %d, muster reads %d", cliActivityVersion, agents.CLIActivityVersion)
	}
	srv, _ := newScripted(t, cliScript{})
	cfg := srv.cfg.ConfigDir
	pid := os.Getpid()
	writeSession(t, cfg, pid, fixtureSessionID, "busy", "")
	gw := mustersGateway(t, gatewayServer(t, srv).URL)
	got, err := gw.CLIActivity(context.Background(), contractAgent)
	if err != nil {
		t.Fatal(err)
	}
	want := agents.CLIActivity{Version: 1, CLIs: []agents.CLIState{{Primary: true, Known: true, SessionID: fixtureSessionID, Status: "busy"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("muster read %+v, want %+v", got, want)
	}
	other := contractAgent
	other.HooksToken = "some-other-agents-token"
	if _, err := gw.CLIActivity(context.Background(), other); err == nil {
		t.Fatal("a wrong bearer read the pod's CLIs")
	}
}
