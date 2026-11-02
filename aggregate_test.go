package main

import (
	"encoding/json"
	"errors"
	"testing"
)

var (
	alice = PlayerRef{Key: "key-alice", Name: "alice"}
	bob   = PlayerRef{Key: "key-bob", Name: "bob"}
)

func startedAggregate(t *testing.T) *GameAggregate {
	t.Helper()
	a := NewGameAggregate("g1")
	events, err := a.Start(alice, bob)
	if err != nil {
		t.Fatal(err)
	}
	applyAll(t, a, events)
	return a
}

func applyAll(t *testing.T, a *GameAggregate, events []Event) {
	t.Helper()
	for _, e := range events {
		if err := a.Apply(e); err != nil {
			t.Fatal(err)
		}
	}
}

// playMoves runs moves such as "e2e4" for whoever's turn it is and returns every event.
func playMoves(t *testing.T, a *GameAggregate, moves ...string) []Event {
	t.Helper()
	var all []Event
	for _, m := range moves {
		key := a.White.Key
		if a.Game.CurrentTurn == Black {
			key = a.Black.Key
		}
		events, err := a.MakeMove(key, sq(t, m[:2]), sq(t, m[2:]))
		if err != nil {
			t.Fatalf("move %s: %v", m, err)
		}
		applyAll(t, a, events)
		all = append(all, events...)
	}
	return all
}

func decode[T any](t *testing.T, e Event) T {
	t.Helper()
	var d T
	if err := json.Unmarshal(e.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestStartEmitsGameStarted(t *testing.T) {
	a := NewGameAggregate("g1")
	events, err := a.Start(alice, bob)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != EventGameStarted || events[0].Version != 1 || events[0].GameID != "g1" {
		t.Fatalf("unexpected events %+v", events)
	}
	d := decode[GameStartedData](t, events[0])
	if d.White != alice || d.Black != bob {
		t.Fatalf("unexpected data %+v", d)
	}
	applyAll(t, a, events)
	if _, err := a.Start(alice, bob); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("got %v, want ErrAlreadyStarted", err)
	}
}

func TestMakeMoveEmitsMoveMade(t *testing.T) {
	a := startedAggregate(t)
	events, err := a.MakeMove(alice.Key, sq(t, "e2"), sq(t, "e4"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != EventMoveMade || events[0].Version != 2 {
		t.Fatalf("unexpected events %+v", events)
	}
	want := MoveMadeData{Color: "white", Piece: "pawn", From: "e2", To: "e4"}
	if got := decode[MoveMadeData](t, events[0]); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestMakeMoveDoesNotChangeStateUntilApplied(t *testing.T) {
	a := startedAggregate(t)
	before := a.Game.Board.Placement()
	events, err := a.MakeMove(alice.Key, sq(t, "e2"), sq(t, "e4"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != 1 || a.Game.Board.Placement() != before || a.Game.CurrentTurn != White {
		t.Fatal("MakeMove changed the aggregate")
	}
	applyAll(t, a, events)
	if a.Version != 2 || a.Game.Board.Placement() == before {
		t.Fatal("Apply did not change the aggregate")
	}
}

func TestRejections(t *testing.T) {
	ended := startedAggregate(t)
	events, _ := ended.Resign(alice.Key)
	applyAll(t, ended, events)

	tests := []struct {
		name string
		a    *GameAggregate
		key  string
		from string
		to   string
		want error
	}{
		{"wrong turn", startedAggregate(t), bob.Key, "e7", "e5", ErrNotYourTurn},
		{"stranger", startedAggregate(t), "key-eve", "e2", "e4", ErrNotInGame},
		{"illegal", startedAggregate(t), alice.Key, "e2", "e5", ErrIllegalMove},
		{"before start", NewGameAggregate("g2"), alice.Key, "e2", "e4", ErrGameNotStarted},
		{"after end", ended, alice.Key, "e2", "e4", ErrGameOver},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, err := tt.a.MakeMove(tt.key, sq(t, tt.from), sq(t, tt.to))
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
			if len(events) != 0 {
				t.Fatalf("got events %+v, want none", events)
			}
		})
	}
}

func TestSameKeyCannotBeBothPlayers(t *testing.T) {
	if _, err := NewGameAggregate("g1").Start(alice, alice); err == nil {
		t.Fatal("expected an error")
	}
}

func TestFoolsMateEndsTheGame(t *testing.T) {
	a := startedAggregate(t)
	playMoves(t, a, "f2f3", "e7e5", "g2g4")
	events := playMoves(t, a, "d8h4")
	if len(events) != 2 || events[0].Type != EventMoveMade || events[1].Type != EventGameEnded {
		t.Fatalf("unexpected events %+v", events)
	}
	want := GameEndedData{Outcome: OutcomeCheckmate, Winner: "black"}
	if got := decode[GameEndedData](t, events[1]); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if !a.Ended {
		t.Fatal("aggregate should be ended")
	}
}

func TestStalemateEndsTheGame(t *testing.T) {
	a := startedAggregate(t)
	playMoves(t, a, "e2e3", "a7a5", "d1h5", "a8a6", "h5a5", "h7h5", "h2h4", "a6h6",
		"a5c7", "f7f6", "c7d7", "e8f7", "d7b7", "d8d3", "b7b8", "d3h7", "b8c8", "f7g6")
	events := playMoves(t, a, "c8e6")
	if len(events) != 2 || events[1].Type != EventGameEnded {
		t.Fatalf("unexpected events %+v", events)
	}
	want := GameEndedData{Outcome: OutcomeStalemate}
	if got := decode[GameEndedData](t, events[1]); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestPromotionRecordedInEvent(t *testing.T) {
	a := startedAggregate(t)
	a.Game = gameFrom(White, map[string]Piece{
		"a7": {Pawn, White}, "e1": {King, White}, "h6": {King, Black},
	})
	events := playMoves(t, a, "a7a8")
	if got := decode[MoveMadeData](t, events[0]); got.Promotion != "queen" {
		t.Fatalf("got promotion %q, want queen", got.Promotion)
	}
	if piece := a.Game.Board.At(sq(t, "a8")); piece != (Piece{Queen, White}) {
		t.Fatalf("got %+v on a8, want a white queen", piece)
	}
}

func TestResignNamesOpponentWinner(t *testing.T) {
	a := startedAggregate(t)
	events, err := a.Resign(alice.Key)
	if err != nil {
		t.Fatal(err)
	}
	want := GameEndedData{Outcome: OutcomeResignation, Winner: "black"}
	if len(events) != 1 || events[0].Type != EventGameEnded || decode[GameEndedData](t, events[0]) != want {
		t.Fatalf("unexpected events %+v", events)
	}
	applyAll(t, a, events)
	if _, err := a.Resign(bob.Key); !errors.Is(err, ErrGameOver) {
		t.Fatalf("got %v, want ErrGameOver", err)
	}
}

func TestLoadRebuildsSameState(t *testing.T) {
	a := startedAggregate(t)
	started, _ := NewGameAggregate("g1").Start(alice, bob)
	all := append(started, playMoves(t, a, "e2e4", "e7e5", "g1f3")...)
	events, _ := a.Resign(bob.Key)
	all = append(all, events...)
	applyAll(t, a, events)

	loaded, err := LoadGameAggregate("g1", all)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Game.Board.Placement() != a.Game.Board.Placement() ||
		loaded.Version != a.Version || loaded.Ended != a.Ended ||
		loaded.Game.CurrentTurn != a.Game.CurrentTurn ||
		loaded.White != alice || loaded.Black != bob {
		t.Fatalf("loaded %+v differs from played %+v", loaded, a)
	}
}

func TestApplyDoesNotValidate(t *testing.T) {
	a := NewGameAggregate("g1")
	// A rook jump from a1 to a5 over its own pawn is illegal, but Apply just records it.
	e := Event{GameID: "g1", Version: 1, Type: EventMoveMade,
		Data: json.RawMessage(`{"color":"white","piece":"rook","from":"a1","to":"a5"}`)}
	if err := a.Apply(e); err != nil {
		t.Fatal(err)
	}
	if a.Game.Board.At(sq(t, "a5")).Type != Rook {
		t.Fatal("board did not change")
	}
}
