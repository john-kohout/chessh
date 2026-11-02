package main

import "testing"

func TestReplayPositions(t *testing.T) {
	events := gameEvents(t, "g1", alice, bob, foolsMate...)
	events = append(events, rawEvent("g1", 6, EventGameEnded, `{"outcome":"checkmate","winner":"black"}`))
	r, err := BuildReplay(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Positions) != 5 || len(r.Moves) != 4 || r.Moves[3] != "d8-h4" {
		t.Fatalf("positions=%d moves=%v", len(r.Positions), r.Moves)
	}
	if r.Positions[0] != NewBoard() {
		t.Fatal("Positions[0] should be the start position")
	}
	a, err := LoadGameAggregate("g1", events)
	if err != nil {
		t.Fatal(err)
	}
	if r.Positions[4] != a.Game.Board {
		t.Fatal("last position should equal the aggregate's board")
	}
	if r.White != alice || r.Black != bob || !r.Finished || r.Outcome != OutcomeCheckmate || r.Winner != "black" {
		t.Fatalf("replay = %+v", r)
	}
}

func TestReplayUnfinishedGame(t *testing.T) {
	r, err := BuildReplay(gameEvents(t, "g1", alice, bob, "e2e4"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Finished || r.Outcome != "" || !r.EndedAt.IsZero() {
		t.Fatalf("replay = %+v", r)
	}
}

func TestReplayShowsPromotion(t *testing.T) {
	r, err := BuildReplay(gameEvents(t, "g1", alice, bob,
		"h2h4", "a7a6", "h4h5", "a6a5", "h5h6", "a5a4", "h6g7", "a4a3", "g7h8"))
	if err != nil {
		t.Fatal(err)
	}
	h8 := sq(t, "h8")
	if len(r.Positions) != 10 {
		t.Fatalf("positions = %d", len(r.Positions))
	}
	if got := r.Positions[8].At(sq(t, "g7")); got != (Piece{Pawn, White}) {
		t.Fatalf("before promotion g7 = %+v", got)
	}
	if got := r.Positions[9].At(h8); got != (Piece{Queen, White}) {
		t.Fatalf("after promotion h8 = %+v", got)
	}
}

func TestReplayCastlingMovesRook(t *testing.T) {
	r, err := BuildReplay(gameEvents(t, "g1", alice, bob,
		"e2e4", "e7e5", "g1f3", "b8c6", "f1c4", "g8f6", "e1g1"))
	if err != nil {
		t.Fatal(err)
	}
	last := r.Positions[len(r.Positions)-1]
	if got := last.At(sq(t, "f1")); got != (Piece{Rook, White}) {
		t.Fatalf("f1 = %+v", got)
	}
	if got := last.At(sq(t, "g1")); got != (Piece{King, White}) {
		t.Fatalf("g1 = %+v", got)
	}
}

func TestReplayPositionsAreIndependent(t *testing.T) {
	r, err := BuildReplay(gameEvents(t, "g1", alice, bob, "e2e4", "e7e5"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Positions[1] == r.Positions[2] || r.Positions[0] == r.Positions[1] {
		t.Fatal("positions should differ from each other")
	}
	if r.Positions[1].At(sq(t, "e4")).Type != Pawn || r.Positions[1].At(sq(t, "e5")).Type != Empty {
		t.Fatal("Positions[1] was changed by a later move")
	}
}

func TestReplayOfEmptyEventsErrors(t *testing.T) {
	if _, err := BuildReplay(nil); err == nil {
		t.Fatal("expected an error")
	}
}
