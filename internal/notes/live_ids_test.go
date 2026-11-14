package notes

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestLiveIDsIsGetsAnswerInOneRead: through the status-logging decorator the
// server wires, LiveIDs names a live task, and not a dismissed one or an id
// that never existed — what Get answers for each.
func TestLiveIDsIsGetsAnswerInOneRead(t *testing.T) {
	store, _, ctx := softDeleteTestStore(t)
	live, err := store.Create(ctx, Note{Title: "live-ids live", Body: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	gone, err := store.Create(ctx, Note{Title: "live-ids dismissed", Body: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SoftDelete(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	never := gone.ID + 1_000_000
	got, err := LiveIDs(ctx, WithStatusLogging(store, nil), []int64{live.ID, gone.ID, never})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[int64]bool{live.ID: true}; !reflect.DeepEqual(got, want) {
		t.Errorf("LiveIDs = %v, want %v", got, want)
	}
}

// getOnlyStore is a Store with no batched read: only Get answers.
type getOnlyStore struct {
	Store
	live map[int64]bool
	gets int
}

func (s *getOnlyStore) Get(_ context.Context, id int64) (Note, error) {
	s.gets++
	if s.live[id] {
		return Note{ID: id}, nil
	}
	return Note{}, pgx.ErrNoRows
}

// TestLiveIDsFallsBackToGet: a store without the batched read gets the same
// answer from one Get per id.
func TestLiveIDsFallsBackToGet(t *testing.T) {
	s := &getOnlyStore{live: map[int64]bool{3: true}}
	got, err := LiveIDs(context.Background(), WithStatusLogging(s, nil), []int64{3, 4})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[int64]bool{3: true}; !reflect.DeepEqual(got, want) || s.gets != 2 {
		t.Errorf("LiveIDs = %v with %d gets, want %v with 2", got, s.gets, want)
	}
}
