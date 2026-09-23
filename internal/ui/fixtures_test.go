package ui

import (
	"time"

	"github.com/ZacxDev/muster/internal/notes"
)

// Shared render fixtures for this package's tests.
//
// 🔴 OWN FILE, SAME REASON AS helpers_test.go: upstream kept these inside its
// largest test file, so every other test file silently depended on that file
// being in the package. fixedNow in particular is load-bearing — a fixture
// built on time.Now() renders differently on every run, and a relative
// timestamp assertion against it passes or fails by the clock.

// fixedNow anchors every time-bearing fixture so the rendered output is stable.
var fixedNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

func sampleTasksView() TasksView {
	agent := AgentBrief{ID: 41, Name: "worker-gamma", DisplayName: "worker-gamma", Status: "running"}
	return TasksView{
		Vocabulary: []notes.TagCount{{Tag: "backend", Count: 4}, {Tag: "ux", Count: 2}},
		Projects:   []notes.ProjectCount{{Name: "muster", Count: 6}, {Name: "vetr", Count: 1}},
		ActiveTags: []string{"backend"},
		Cards: []TaskCardView{
			{Note: notes.Note{
				ID: 101, Title: "Ship the drawer fix", Directory: "/work/muster",
				Body: "Body of the first task, long enough to snippet.", Status: string(notes.StatusInProgress),
				Tags: []string{"backend", "project:muster"}, Repo: "ZacxDev/muster", Model: "sonnet",
				CreatedAt: fixedNow.Add(-2 * time.Hour), UpdatedAt: fixedNow.Add(-30 * time.Minute),
				Comments: []notes.Comment{
					{ID: 1, NoteID: 101, Author: "zach", Body: "First comment body.", CreatedAt: fixedNow.Add(-90 * time.Minute)},
					// Retracted → exercises commentTombstone.
					{ID: 2, NoteID: 101, Author: "operator", Retracted: true, CreatedAt: fixedNow.Add(-80 * time.Minute)},
				},
				// Attachments → exercises summaryMeta's "N files" branch.
				Attachments: []notes.Attachment{{ID: 9, NoteID: 101, Filename: "trace-one.log", ContentType: "text/plain", SizeBytes: 2048}},
			}, Agent: &agent},
			{Note: notes.Note{
				ID: 202, Title: "Second, open task", Directory: "/work/vetr",
				Body: "A different body so the two cards are not identical.", Status: string(notes.StatusOpen),
				Tags: []string{"ux", "project:vetr"}, CreatedAt: fixedNow.Add(-5 * time.Hour), UpdatedAt: fixedNow.Add(-4 * time.Hour),
			}},
			{Note: notes.Note{
				ID: 303, Title: "Third, done task", Directory: "/work/gamma",
				Body: "Yet another distinct body.", Status: string(notes.StatusComplete),
				CreatedAt: fixedNow.Add(-30 * time.Hour), UpdatedAt: fixedNow.Add(-26 * time.Hour),
			}},
		},
	}
}

// sampleTaskDetailView is the /tasks/{id} page's fixture: the FIRST board card
// (already the richest one — comments including a retracted tombstone, an
// attachment, a linked agent) plus a session thread, in DETAIL shape.
//
// The session links are added here rather than to sampleTasksView because the
// board card renders only the session COUNT chip; the rows, their expander and
// its telemetry exist on this page alone, so this is the only fixture that can
// make the element-level sweeps see them.
func sampleTaskDetailView() TaskCardView {
	v := sampleTasksView().Cards[0]
	v.Detail = true
	v.Note.Sessions = []notes.SessionLink{
		{SessionID: "sess-aaaa1111", Role: notes.RoleCreated, Project: "muster", Cwd: "/work/muster",
			DetailAvailable: true, FirstSeenAt: fixedNow.Add(-2 * time.Hour), LastSeenAt: fixedNow.Add(-90 * time.Minute)},
		// DetailAvailable=false → the REAPED row (a session whose transcript the
		// 14-day retention sweep removed), which renders differently.
		{SessionID: "sess-bbbb2222", Role: notes.RoleWorked, Project: "muster",
			DetailAvailable: false, FirstSeenAt: fixedNow.Add(-40 * time.Hour), LastSeenAt: fixedNow.Add(-39 * time.Hour)},
	}
	return v
}

func samplePrivilegeRequests() []PrivilegeRequestView {
	return []PrivilegeRequestView{
		{ID: 11, AgentName: "worker-gamma", Profile: "kube-read", Reason: "needs to read pod logs", CreatedAt: fixedNow.Add(-6 * time.Minute)},
		{ID: 12, AgentName: "worker-delta", Profile: "s3-write", Reason: "needs to upload artifacts", CreatedAt: fixedNow.Add(-25 * time.Minute)},
	}
}

func sampleProfiles() []ProfileView {
	return []ProfileView{
		{ID: 21, Name: "kube-read", DisplayName: "Kube read", Description: "Read-only cluster access", Summary: "get/list/watch"},
		{ID: 22, Name: "s3-write", DisplayName: "S3 write", Description: "Artifact bucket writes", Summary: "PutObject on audit-artifacts"},
	}
}

func sampleRunbooks() []RunbookView {
	return []RunbookView{
		{ID: 31, Name: "deploy-muster", DisplayName: "Deploy muster", Description: "Build and roll out muster",
			Summary: "build → push → rollout", Repo: "ZacxDev/muster", Runs: 14, LastRunAt: fixedNow.Add(-20 * time.Hour),
			Params: []RunbookParam{{Name: "tag", Label: "Image tag", Description: "Tag to deploy", Required: true, Default: "latest"}}},
		{ID: 32, Name: "audit-vetr", DisplayName: "Audit vetr", Description: "Run the vetr UX audit",
			Summary: "crawl → axe → report", Repo: "ZacxDev/vetr", Runs: 2,
			Params: []RunbookParam{{Name: "depth", Label: "Crawl depth", Enum: []string{"1", "2", "3"}, Default: "2"}}},
	}
}

func sampleAgentCards() []AgentCardView {
	noteID := int64(101)
	return []AgentCardView{
		{ID: 41, Name: "worker-gamma", DisplayName: "worker-gamma", Repo: "ZacxDev/muster", Status: "running",
			KickedOff: true, Recent: []string{"gamma log line one", "gamma log line two"}, Model: "sonnet",
			NoteID: &noteID, CreatedAt: fixedNow.Add(-3 * time.Hour), UpdatedAt: fixedNow.Add(-4 * time.Minute)},
		{ID: 42, Name: "worker-delta", DisplayName: "worker-delta", Repo: "ZacxDev/vetr", Status: "error",
			Recent: []string{"delta log line one"}, CreatedAt: fixedNow.Add(-9 * time.Hour), UpdatedAt: fixedNow.Add(-70 * time.Minute)},
	}
}

func sampleRepos() []RepoView {
	return []RepoView{
		{FullName: "ZacxDev/muster", Private: true, HTMLURL: "https://github.com/ZacxDev/muster"},
		{FullName: "ZacxDev/vetr", HTMLURL: "https://github.com/ZacxDev/vetr"},
	}
}

func sampleNoteEditView() NoteEditView {
	return NoteEditView{
		Note:        sampleTasksView().Cards[0].Note,
		Directories: []string{"/work/muster", "/work/vetr"},
		Profiles:    []ProfileOption{{ID: 21, Name: "kube-read"}, {ID: 22, Name: "s3-write"}},
		Vocabulary:  []notes.TagCount{{Tag: "backend", Count: 4}, {Tag: "ux", Count: 2}},
	}
}
