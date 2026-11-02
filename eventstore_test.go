package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func countEvents(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func rawEvent(gameID string, version int, eventType, data string) Event {
	return Event{GameID: gameID, Version: version, Type: eventType, Data: []byte(data)}
}

func TestAppendAndLoadPreservesEvents(t *testing.T) {
	db := testDB(t)
	store := NewEventStore(db)
	ctx := context.Background()
	moveJSON := `{"color":"white","piece":"pawn","from":"e2","to":"e4"}`
	if err := store.Append(ctx, []Event{
		rawEvent("g1", 1, EventGameStarted, `{"white":{"key":"a","name":"alice"},"black":{"key":"b","name":"bob"}}`),
		rawEvent("g1", 2, EventMoveMade, moveJSON),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(ctx, []Event{rawEvent("g2", 1, EventGameStarted, `{}`)}); err != nil {
		t.Fatal(err)
	}

	got, err := store.Load(ctx, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Version != 1 || got[1].Version != 2 || got[1].Type != EventMoveMade {
		t.Fatalf("unexpected events %+v", got)
	}
	if string(got[1].Data) != moveJSON {
		t.Fatalf("data changed: %s", got[1].Data)
	}
	for _, e := range got {
		if e.Sequence == 0 || e.CreatedAt.IsZero() || e.GameID != "g1" {
			t.Fatalf("store fields not populated: %+v", e)
		}
	}
}

func TestAppendConflict(t *testing.T) {
	db := testDB(t)
	store := NewEventStore(db)
	ctx := context.Background()
	if err := store.Append(ctx, []Event{rawEvent("g1", 1, EventGameStarted, `{}`)}); err != nil {
		t.Fatal(err)
	}
	err := store.Append(ctx, []Event{rawEvent("g1", 1, EventGameStarted, `{}`)})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("got %v, want ErrConflict", err)
	}
	if n := countEvents(t, db); n != 1 {
		t.Fatalf("got %d rows, want 1", n)
	}
}

func TestAppendIsAtomic(t *testing.T) {
	db := testDB(t)
	store := NewEventStore(db)
	ctx := context.Background()
	if err := store.Append(ctx, []Event{rawEvent("g1", 1, EventGameStarted, `{}`)}); err != nil {
		t.Fatal(err)
	}
	err := store.Append(ctx, []Event{
		rawEvent("g1", 2, EventMoveMade, `{}`),
		rawEvent("g1", 1, EventMoveMade, `{}`), // conflicts
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("got %v, want ErrConflict", err)
	}
	if n := countEvents(t, db); n != 1 {
		t.Fatalf("got %d rows, want 1: the first event of the batch was stored", n)
	}
}

func TestAfterReturnsLaterEventsInOrder(t *testing.T) {
	db := testDB(t)
	store := NewEventStore(db)
	ctx := context.Background()
	for _, batch := range [][]Event{
		{rawEvent("g1", 1, EventGameStarted, `{}`)},
		{rawEvent("g2", 1, EventGameStarted, `{}`)},
		{rawEvent("g1", 2, EventMoveMade, `{}`), rawEvent("g1", 3, EventGameEnded, `{}`)},
	} {
		if err := store.Append(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	all, err := store.After(ctx, tx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("got %d events, want 4", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].Sequence <= all[i-1].Sequence {
			t.Fatalf("not in sequence order: %+v", all)
		}
	}
	later, err := store.After(ctx, tx, all[1].Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if len(later) != 2 || later[0].Sequence != all[2].Sequence || later[1].Type != EventGameEnded {
		t.Fatalf("unexpected events after sequence %d: %+v", all[1].Sequence, later)
	}
}

func TestDataIsReadableJSON(t *testing.T) {
	db := testDB(t)
	commands := NewCommands(NewEventStore(db), nil)
	ctx := context.Background()
	if err := commands.StartGame(ctx, "g1", alice, bob); err != nil {
		t.Fatal(err)
	}
	if err := commands.MakeMove(ctx, "g1", alice.Key, sq(t, "e2"), sq(t, "e4")); err != nil {
		t.Fatal(err)
	}
	var data string
	if err := db.QueryRow(`SELECT data FROM events WHERE type = ?`, EventMoveMade).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data, `"from":"e2"`) {
		t.Fatalf("data is not readable JSON: %s", data)
	}
}

func TestOpenDBCreatesParentDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not", "yet", "there", "chessh.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if n := countEvents(t, db); n != 0 {
		t.Fatalf("got %d rows, want 0", n)
	}
}
