package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// activeGamesSetup plays moves in game "g1" between alice and bob and projects them.
func activeGamesSetup(t *testing.T, moves ...string) (*GameAggregate, func() (ActiveGame, bool, error)) {
	t.Helper()
	db := testDB(t)
	store := NewEventStore(db)
	ctx := context.Background()
	a := startedAggregate(t)
	events := []Event{a.newEvent(1, EventGameStarted, GameStartedData{White: alice, Black: bob})}
	events = append(events, playMoves(t, a, moves...)...)
	if err := store.Append(ctx, events); err != nil {
		t.Fatal(err)
	}
	if err := NewProjector(db, store, nil, ActiveGames{}).CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	return a, func() (ActiveGame, bool, error) { return GetActiveGame(ctx, db, "g1") }
}

func TestActiveGameCreatedOnStart(t *testing.T) {
	_, get := activeGamesSetup(t)
	g, found, err := get()
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if g.White != alice || g.Black != bob {
		t.Fatalf("players = %v %v", g.White, g.Black)
	}
	if got := g.Game.Board.Placement(); got != "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR" {
		t.Fatalf("board = %s", got)
	}
	if g.Game.CurrentTurn != White || len(g.LegalMoves) != 20 || g.InCheck || g.Version != 1 {
		t.Fatalf("turn=%v moves=%d check=%v version=%d", g.Game.CurrentTurn, len(g.LegalMoves), g.InCheck, g.Version)
	}
}

func TestActiveGameUpdatedByMove(t *testing.T) {
	_, get := activeGamesSetup(t, "e2e4")
	g, found, err := get()
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if got := g.Game.Board.Placement(); got != "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR" {
		t.Fatalf("board = %s", got)
	}
	if g.Game.CurrentTurn != Black || g.Version != 2 {
		t.Fatalf("turn=%v version=%d", g.Game.CurrentTurn, g.Version)
	}
	if len(g.Game.MoveHistory) != 1 {
		t.Fatalf("history = %d moves, want 1", len(g.Game.MoveHistory))
	}
}

func TestActiveGameInCheckFlag(t *testing.T) {
	_, get := activeGamesSetup(t, "e2e4", "f7f6", "d1h5")
	g, _, err := get()
	if err != nil {
		t.Fatal(err)
	}
	if !g.InCheck {
		t.Fatal("InCheck = false, want true")
	}
	// The queen on h5 also covers f7, so the only escape is blocking with g6.
	if len(g.LegalMoves) != 1 || g.LegalMoves[0].From.String() != "g7" || g.LegalMoves[0].To.String() != "g6" {
		t.Fatalf("legal moves = %v, want only g7-g6", g.LegalMoves)
	}
}

func TestActiveGameRemovedOnEnd(t *testing.T) {
	_, get := activeGamesSetup(t, "f2f3", "e7e5", "g2g4", "d8h4")
	_, found, err := get()
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("finished game is still active")
	}
}

func TestActiveGameForPlayerFindsEitherColor(t *testing.T) {
	db := testDB(t)
	store := NewEventStore(db)
	ctx := context.Background()
	a := NewGameAggregate("g1")
	events, _ := a.Start(alice, bob)
	if err := store.Append(ctx, events); err != nil {
		t.Fatal(err)
	}
	if err := NewProjector(db, store, nil, ActiveGames{}).CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{alice.Key, bob.Key} {
		g, found, err := GetActiveGameForPlayer(ctx, db, key)
		if err != nil || !found || g.GameID != "g1" {
			t.Fatalf("%s: found=%v err=%v game=%s", key, found, err, g.GameID)
		}
	}
	if _, found, _ := GetActiveGameForPlayer(ctx, db, "stranger"); found {
		t.Fatal("stranger found a game")
	}
	list, err := ListActiveGames(ctx, db)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d err=%v", len(list), err)
	}
}

func TestActiveGameMatchesAggregate(t *testing.T) {
	// Castling (white O-O), en passant (e5xd6), and promotion (b7xa8=Q).
	a, get := activeGamesSetup(t,
		"e2e4", "a7a6", "g1f3", "a6a5", "f1c4", "h7h6", "e1g1", "h6h5", // castling
		"e4e5", "d7d5", "e5d6", // en passant
		"g8f6", "d6c7", "c8d7", "c7b8", "d7c6") // capture-promotion
	g, found, err := get()
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	want := a.Game
	if g.Game.Board.Placement() != want.Board.Placement() || g.Game.CurrentTurn != want.CurrentTurn {
		t.Fatalf("board/turn differ: %s %v vs %s %v", g.Game.Board.Placement(), g.Game.CurrentTurn, want.Board.Placement(), want.CurrentTurn)
	}
	if !reflect.DeepEqual(g.LegalMoves, want.LegalMoves()) {
		t.Fatal("legal moves differ from the aggregate's")
	}
	if !reflect.DeepEqual(g.Game, want) {
		t.Fatalf("game state differs:\n%+v\n%+v", g.Game, want)
	}
	if g.Version != a.Version {
		t.Fatalf("version %d vs %d", g.Version, a.Version)
	}
}

func TestQueriesReportUnavailableAfterDrop(t *testing.T) {
	db := testDB(t)
	store := NewEventStore(db)
	ctx := context.Background()
	if err := NewProjector(db, store, nil, ActiveGames{}).CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE active_games`); err != nil {
		t.Fatal(err)
	}
	if _, found, err := GetActiveGame(ctx, db, "g1"); !errors.Is(err, ErrProjectionUnavailable) || found {
		t.Fatalf("GetActiveGame: found=%v err=%v", found, err)
	}
	if _, found, err := GetActiveGameForPlayer(ctx, db, "k"); !errors.Is(err, ErrProjectionUnavailable) || found {
		t.Fatalf("GetActiveGameForPlayer: found=%v err=%v", found, err)
	}
	if _, err := ListActiveGames(ctx, db); !errors.Is(err, ErrProjectionUnavailable) {
		t.Fatalf("ListActiveGames: err=%v", err)
	}
}

func TestActiveGameRestoredAfterDeleteFrom(t *testing.T) {
	for _, tt := range []struct {
		name  string
		moves []string // after the delete, White first, all in one catch-up
	}{
		{"one move", []string{"g1f3"}},
		{"several moves", []string{"g1f3", "b8c6", "f1c4"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := testDB(t)
			store := NewEventStore(db)
			commands := NewCommands(store, nil)
			projector := NewProjector(db, store, nil, ActiveGames{})
			ctx := context.Background()
			move := func(key, m string) {
				t.Helper()
				if err := commands.MakeMove(ctx, "g1", key, sq(t, m[:2]), sq(t, m[2:])); err != nil {
					t.Fatalf("move %s: %v", m, err)
				}
			}

			if err := commands.StartGame(ctx, "g1", alice, bob); err != nil {
				t.Fatal(err)
			}
			move(alice.Key, "e2e4")
			move(bob.Key, "e7e5")
			if err := projector.CatchUp(ctx); err != nil {
				t.Fatal(err)
			}

			if _, err := db.Exec(`DELETE FROM active_games`); err != nil {
				t.Fatal(err)
			}
			for i, m := range tt.moves {
				key := alice.Key
				if i%2 == 1 {
					key = bob.Key
				}
				move(key, m)
			}
			if err := projector.CatchUp(ctx); err != nil {
				t.Fatal(err)
			}

			events, err := store.Load(ctx, "g1")
			if err != nil {
				t.Fatal(err)
			}
			want, err := LoadGameAggregate("g1", events)
			if err != nil {
				t.Fatal(err)
			}
			got, found, err := GetActiveGame(ctx, db, "g1")
			if err != nil || !found {
				t.Fatalf("found=%v err=%v", found, err)
			}
			if got.White != alice || got.Black != bob || got.Version != want.Version {
				t.Fatalf("white=%v black=%v version=%d, want version %d", got.White, got.Black, got.Version, want.Version)
			}
			if !reflect.DeepEqual(got.Game, want.Game) {
				t.Fatalf("game state differs:\n%+v\n%+v", got.Game, want.Game)
			}
			if !reflect.DeepEqual(got.LegalMoves, want.Game.LegalMoves()) {
				t.Fatal("legal moves differ from the aggregate's")
			}

			if err := commands.StartGame(ctx, "g2", carol, dave); err != nil {
				t.Fatal(err)
			}
			if err := projector.CatchUp(ctx); err != nil {
				t.Fatal(err)
			}
			if _, found, err := GetActiveGame(ctx, db, "g2"); err != nil || !found {
				t.Fatalf("new game: found=%v err=%v", found, err)
			}
		})
	}
}
