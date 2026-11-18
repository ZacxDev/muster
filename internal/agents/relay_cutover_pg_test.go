package agents

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTheRelayCutoverNamesTheLinkMigration: relayLinkMigrationVersion is the
// version of the migration that added chat_messages.plugin_message_id — the
// moment WrittenBeforeRelayLink is measured against. A renumbering at merge
// (this migration has been renumbered twice already) that left the constant
// behind would measure against ANOTHER migration's applied_at.
func TestTheRelayCutoverNamesTheLinkMigration(t *testing.T) {
	name := "00" + relayLinkMigrationVersion + "_chat_plugin_message.sql"
	if _, err := os.Stat(filepath.Join("..", "db", "migrations", name)); err != nil {
		t.Fatalf("relayLinkMigrationVersion %s names no %s: %v", relayLinkMigrationVersion, name, err)
	}
}

// TestOnlyARowWrittenBeforeTheLinkMigrationIsMarkedSo: every chat read marks a
// row WrittenBeforeRelayLink exactly when its created_at precedes the link
// migration's applied_at — a row written now is not, the same row backdated
// before the migration is — on the page read, the full read and the one-row
// read, which share one column list.
func TestOnlyARowWrittenBeforeTheLinkMigrationIsMarkedSo(t *testing.T) {
	s, pool, ctx := searchStore(t)
	a := searchAgent(t, s, pool, ctx, "cutover")
	sess, err := s.CreateSession(ctx, a.ID, a.Name)
	if err != nil {
		t.Fatal(err)
	}
	now, err := s.AddChatMessage(ctx, ChatMessage{AgentID: a.ID, SessionID: sess.ID, Role: "user", Content: "written now"})
	if err != nil {
		t.Fatal(err)
	}
	if now.WrittenBeforeRelayLink {
		t.Fatal("a row written now (AddChatMessage's RETURNING) is marked as written before the link migration")
	}
	old, err := s.AddChatMessage(ctx, ChatMessage{AgentID: a.ID, SessionID: sess.ID, Role: "user", Content: "written long ago"})
	if err != nil {
		t.Fatal(err)
	}
	var applied time.Time
	if err := pool.QueryRow(ctx, `SELECT applied_at FROM schema_migrations WHERE version = `+relayLinkMigrationVersion).Scan(&applied); err != nil {
		t.Fatalf("the link migration's applied_at: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE chat_messages SET created_at = $1 WHERE id = $2`, applied.Add(-time.Hour), old.ID); err != nil {
		t.Fatal(err)
	}
	want := map[int64]bool{now.ID: false, old.ID: true}
	for name, read := range map[string]func() ([]ChatMessage, error){
		"ListChatMessages":       func() ([]ChatMessage, error) { return s.ListChatMessages(ctx, sess.ID) },
		"ListRecentChatMessages": func() ([]ChatMessage, error) { return s.ListRecentChatMessages(ctx, sess.ID, 10) },
	} {
		msgs, err := read()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(msgs) != 2 {
			t.Fatalf("%s: %d rows", name, len(msgs))
		}
		for _, m := range msgs {
			if m.WrittenBeforeRelayLink != want[m.ID] {
				t.Errorf("%s: row %d (%q) WrittenBeforeRelayLink %v, want %v", name, m.ID, m.Content, m.WrittenBeforeRelayLink, want[m.ID])
			}
		}
	}
	one, err := s.ChatMessage(ctx, old.ID)
	if err != nil || !one.WrittenBeforeRelayLink {
		t.Errorf("ChatMessage(%d) = %+v, %v; want marked", old.ID, one.WrittenBeforeRelayLink, err)
	}
}
