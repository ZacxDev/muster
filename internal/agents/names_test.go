package agents

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestGenerateNameFormat(t *testing.T) {
	adj := map[string]bool{}
	for _, a := range adjectives {
		adj[a] = true
	}
	noun := map[string]bool{}
	for _, n := range nouns {
		noun[n] = true
	}
	// Generate many to exercise the random picker and assert the shape holds.
	for i := 0; i < 200; i++ {
		name := generateName()
		parts := strings.Split(name, "-")
		if len(parts) != 2 {
			t.Fatalf("generateName()=%q, want adjective-noun (two parts)", name)
		}
		if !adj[parts[0]] {
			t.Errorf("generateName()=%q: %q not in adjectives", name, parts[0])
		}
		if !noun[parts[1]] {
			t.Errorf("generateName()=%q: %q not in nouns", name, parts[1])
		}
	}
}

func TestBuildUniqueAgentNameFirstTry(t *testing.T) {
	f := newFakeStore()
	// No name is taken → first candidate is returned.
	name, err := BuildUniqueAgentName(context.Background(), f)
	if err != nil {
		t.Fatalf("BuildUniqueAgentName: %v", err)
	}
	if name == "" {
		t.Fatal("BuildUniqueAgentName returned an empty name")
	}
}

func TestBuildUniqueAgentNameRetriesOnCollision(t *testing.T) {
	var calls int
	f := newFakeStore()
	// First two candidates are "taken", the third is free — must return the third.
	f.nameExistsFn = func(name string) (bool, error) {
		calls++
		return calls <= 2, nil
	}
	name, err := BuildUniqueAgentName(context.Background(), f)
	if err != nil {
		t.Fatalf("BuildUniqueAgentName: %v", err)
	}
	if name == "" {
		t.Fatal("expected a non-empty name after collisions")
	}
	if calls != 3 {
		t.Errorf("NameExists called %d times, want 3 (two collisions then a free name)", calls)
	}
}

func TestBuildUniqueAgentNameExhausted(t *testing.T) {
	var calls int
	f := newFakeStore()
	// Every candidate is taken → after 10 attempts it gives up with an error.
	f.nameExistsFn = func(name string) (bool, error) {
		calls++
		return true, nil
	}
	_, err := BuildUniqueAgentName(context.Background(), f)
	if err == nil {
		t.Fatal("BuildUniqueAgentName with all names taken = nil error, want exhaustion error")
	}
	if calls != 10 {
		t.Errorf("NameExists called %d times, want 10 attempts before giving up", calls)
	}
}

func TestBuildUniqueAgentNameStoreError(t *testing.T) {
	sentinel := errors.New("db down")
	f := newFakeStore()
	f.nameExistsFn = func(name string) (bool, error) { return false, sentinel }
	_, err := BuildUniqueAgentName(context.Background(), f)
	if !errors.Is(err, sentinel) {
		t.Errorf("BuildUniqueAgentName error = %v, want the store error propagated", err)
	}
}
