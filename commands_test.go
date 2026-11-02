package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCommandsPersistAndNotify(t *testing.T) {
	store := NewEventStore(testDB(t))
	var notified atomic.Int32
	commands := NewCommands(store, func() { notified.Add(1) })
	ctx := context.Background()

	if err := commands.StartGame(ctx, "g1", alice, bob); err != nil {
		t.Fatal(err)
	}
	if err := commands.MakeMove(ctx, "g1", alice.Key, sq(t, "e2"), sq(t, "e4")); err != nil {
		t.Fatal(err)
	}
	events, err := store.Load(ctx, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != EventGameStarted || events[1].Type != EventMoveMade {
		t.Fatalf("unexpected events %+v", events)
	}
	if n := notified.Load(); n != 2 {
		t.Fatalf("onAppended called %d times, want 2", n)
	}
}

func TestCommandRejectionAppendsNothing(t *testing.T) {
	db := testDB(t)
	var notified atomic.Int32
	commands := NewCommands(NewEventStore(db), func() { notified.Add(1) })
	ctx := context.Background()
	if err := commands.StartGame(ctx, "g1", alice, bob); err != nil {
		t.Fatal(err)
	}
	before, calls := countEvents(t, db), notified.Load()

	err := commands.MakeMove(ctx, "g1", alice.Key, sq(t, "e2"), sq(t, "e5"))
	if !errors.Is(err, ErrIllegalMove) {
		t.Fatalf("got %v, want ErrIllegalMove", err)
	}
	if after := countEvents(t, db); after != before {
		t.Fatalf("event count changed from %d to %d", before, after)
	}
	if notified.Load() != calls {
		t.Fatal("onAppended called for a rejected command")
	}
}

func TestConcurrentResignAndMoveWritesOneEnding(t *testing.T) {
	db := testDB(t)
	store := NewEventStore(db)
	commands := NewCommands(store, nil)
	ctx := context.Background()

	for i := 0; i < 50; i++ {
		id := NewGameID()
		if err := commands.StartGame(ctx, id, alice, bob); err != nil {
			t.Fatal(err)
		}
		for j, m := range []string{"f2f3", "e7e5", "g2g4"} {
			key := alice.Key
			if j == 1 {
				key = bob.Key
			}
			if err := commands.MakeMove(ctx, id, key, sq(t, m[:2]), sq(t, m[2:])); err != nil {
				t.Fatal(err)
			}
		}

		// Black can mate with Qh4 while White resigns at the same moment.
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs[0] = commands.MakeMove(ctx, id, bob.Key, sq(t, "d8"), sq(t, "h4"))
		}()
		go func() {
			defer wg.Done()
			errs[1] = commands.Resign(ctx, id, alice.Key)
		}()
		wg.Wait()

		for _, err := range errs {
			if err != nil && !errors.Is(err, ErrGameOver) {
				t.Fatalf("game %d: unexpected error %v", i, err)
			}
		}
		events, err := store.Load(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		endings := 0
		for _, e := range events {
			if e.Type == EventGameEnded {
				endings++
			}
		}
		if endings != 1 {
			t.Fatalf("game %d: got %d GameEnded events, want 1", i, endings)
		}
	}
}

func TestConcurrentCommandsRetryOnConflict(t *testing.T) {
	commands := NewCommands(NewEventStore(testDB(t)), nil)
	ctx := context.Background()
	if err := commands.StartGame(ctx, "g1", alice, bob); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = commands.Resign(ctx, "g1", alice.Key) }()
	go func() { defer wg.Done(); errs[1] = commands.Resign(ctx, "g1", bob.Key) }()
	wg.Wait()

	for _, err := range errs {
		if errors.Is(err, ErrConflict) {
			t.Fatal("ErrConflict leaked out of Commands")
		}
	}
	nils, overs := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			nils++
		case errors.Is(err, ErrGameOver):
			overs++
		}
	}
	if nils != 1 || overs != 1 {
		t.Fatalf("got %v, want one nil and one ErrGameOver", errs)
	}
}
