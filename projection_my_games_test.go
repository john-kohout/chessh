package main

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
)

// gameEvents plays moves in a new game and returns all its events, including GameStarted.
func gameEvents(t *testing.T, id string, white, black PlayerRef, moves ...string) []Event {
	t.Helper()
	a := NewGameAggregate(id)
	events, err := a.Start(white, black)
	if err != nil {
		t.Fatal(err)
	}
	applyAll(t, a, events)
	return append(events, playMoves(t, a, moves...)...)
}

var foolsMate = []string{"f2f3", "e7e5", "g2g4", "d8h4"}

func myGamesSetup(t *testing.T, games ...[]Event) (*sql.DB, *Projector) {
	t.Helper()
	db := testDB(t)
	store := NewEventStore(db)
	for _, events := range games {
		if err := store.Append(context.Background(), events); err != nil {
			t.Fatal(err)
		}
	}
	projector := NewProjector(db, store, nil, MyGames{})
	if err := projector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db, projector
}

func TestMyGamesTwoRowsPerFinishedGame(t *testing.T) {
	db, _ := myGamesSetup(t, gameEvents(t, "g1", alice, bob, foolsMate...))
	ctx := context.Background()

	lost, err := ListMyGames(ctx, db, alice.Key)
	if err != nil || len(lost) != 1 {
		t.Fatalf("alice: %v %v", lost, err)
	}
	g := lost[0]
	if g.GameID != "g1" || g.PlayerKey != alice.Key || g.OpponentName != "bob" || g.Color != "white" ||
		g.Result != "lost" || g.Reason != OutcomeCheckmate || g.MoveCount != 4 {
		t.Fatalf("alice's row = %+v", g)
	}
	if g.StartedAt.IsZero() || g.EndedAt.Before(g.StartedAt) {
		t.Fatalf("times = %v %v", g.StartedAt, g.EndedAt)
	}

	won, err := ListMyGames(ctx, db, bob.Key)
	if err != nil || len(won) != 1 {
		t.Fatalf("bob: %v %v", won, err)
	}
	g = won[0]
	if g.OpponentName != "alice" || g.Color != "black" || g.Result != "won" || g.Reason != OutcomeCheckmate || g.MoveCount != 4 {
		t.Fatalf("bob's row = %+v", g)
	}
}

func TestMyGamesExcludesActiveGames(t *testing.T) {
	db, _ := myGamesSetup(t, gameEvents(t, "g1", alice, bob, "e2e4"))
	for _, p := range []PlayerRef{alice, bob} {
		games, err := ListMyGames(context.Background(), db, p.Key)
		if err != nil || len(games) != 0 {
			t.Fatalf("%s: %v %v", p.Name, games, err)
		}
	}
}

func TestMyGamesNewestFirst(t *testing.T) {
	first := gameEvents(t, "g1", alice, bob, foolsMate...)
	second := gameEvents(t, "g2", alice, bob, "e2e4")
	second = append(second, rawEvent("g2", 3, EventGameEnded, `{"outcome":"resignation","winner":"black"}`))
	db, _ := myGamesSetup(t, first, second)

	games, err := ListMyGames(context.Background(), db, alice.Key)
	if err != nil || len(games) != 2 {
		t.Fatalf("%v %v", games, err)
	}
	if games[0].GameID != "g2" || games[1].GameID != "g1" || games[0].EndedAt.Before(games[1].EndedAt) {
		t.Fatalf("order = %s, %s", games[0].GameID, games[1].GameID)
	}
}

func TestMyGamesScopedToPlayer(t *testing.T) {
	db, _ := myGamesSetup(t, gameEvents(t, "g1", alice, bob, foolsMate...))
	games, err := ListMyGames(context.Background(), db, "key-carol")
	if err != nil || len(games) != 0 {
		t.Fatalf("%v %v", games, err)
	}
}

func TestResultLabels(t *testing.T) {
	for _, c := range []struct{ result, reason, want string }{
		{"won", OutcomeCheckmate, "Won"},
		{"lost", OutcomeCheckmate, "Lost"},
		{"draw", OutcomeStalemate, "Draw"},
		{"won", OutcomeResignation, "Opp. left"},
		{"lost", OutcomeResignation, "Left"},
	} {
		if got := (MyGame{Result: c.result, Reason: c.reason}).ResultLabel(); got != c.want {
			t.Errorf("%s/%s = %q, want %q", c.result, c.reason, got, c.want)
		}
	}
}

func TestMyGamesRebuildIsIdentical(t *testing.T) {
	db, projector := myGamesSetup(t,
		gameEvents(t, "g1", alice, bob, foolsMate...),
		append(gameEvents(t, "g2", bob, alice), rawEvent("g2", 2, EventGameEnded, `{"outcome":"resignation","winner":"white"}`)))
	ctx := context.Background()

	before, err := ListMyGames(ctx, db, alice.Key)
	if err != nil || len(before) != 2 {
		t.Fatalf("%v %v", before, err)
	}
	if _, err := db.Exec(`DROP TABLE my_games`); err != nil {
		t.Fatal(err)
	}
	if err := projector.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := ListMyGames(ctx, db, alice.Key)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("before %+v\nafter  %+v\nerr %v", before, after, err)
	}
}

func TestMyGamesQueryUnavailableAfterDrop(t *testing.T) {
	db, _ := myGamesSetup(t)
	if _, err := db.Exec(`DROP TABLE my_games`); err != nil {
		t.Fatal(err)
	}
	if _, err := ListMyGames(context.Background(), db, alice.Key); !errors.Is(err, ErrProjectionUnavailable) {
		t.Fatalf("err = %v", err)
	}
}
