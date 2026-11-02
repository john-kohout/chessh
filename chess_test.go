package main

import "testing"

func sq(t *testing.T, s string) Position {
	t.Helper()
	p, err := ParsePosition(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// gameFrom builds a position with all castling rights intact.
func gameFrom(turn Color, pieces map[string]Piece) *Game {
	g := NewGame()
	g.Board = Board{}
	g.CurrentTurn = turn
	for name, piece := range pieces {
		p, err := ParsePosition(name)
		if err != nil {
			panic(err)
		}
		g.Board.Set(p, piece)
	}
	return g
}

func play(t *testing.T, g *Game, moves ...string) {
	t.Helper()
	for i := 0; i+1 < len(moves); i += 2 {
		if !g.MakeMove(sq(t, moves[i]), sq(t, moves[i+1])) {
			t.Fatalf("illegal move %s%s", moves[i], moves[i+1])
		}
	}
}

func TestStalemate(t *testing.T) {
	g := gameFrom(Black, map[string]Piece{
		"a8": {King, Black}, "b6": {Queen, White}, "h1": {King, White},
	})
	if !g.IsStalemate(Black) {
		t.Error("want stalemate")
	}
	if g.IsCheckmate(Black) {
		t.Error("stalemate is not checkmate")
	}
	if n := len(g.LegalMoves()); n != 0 {
		t.Errorf("want no legal moves, got %d", n)
	}
}

func TestFoolsMateIsCheckmate(t *testing.T) {
	g := NewGame()
	play(t, g, "f2", "f3", "e7", "e5", "g2", "g4", "d8", "h4")
	if !g.IsCheckmate(White) {
		t.Error("want checkmate")
	}
	if g.IsStalemate(White) {
		t.Error("checkmate is not stalemate")
	}
}

func TestCheckmateEscapeByEnPassant(t *testing.T) {
	// Black just played c7c5, checking the king on d4. The king is boxed in
	// and c5 is protected, so only d5xc6 en passant removes the checker.
	g := gameFrom(White, map[string]Piece{
		"d4": {King, White}, "c5": {Pawn, Black}, "d5": {Pawn, White},
		"c3": {Pawn, White}, "d3": {Pawn, White}, "e3": {Pawn, White},
		"c4": {Pawn, White}, "e4": {Pawn, White}, "e5": {Pawn, White},
		"c8": {Rook, Black}, "h8": {King, Black},
	})
	g.EnPassantTarget = &Position{5, 2}
	if !g.IsInCheck(White) {
		t.Fatal("setup: white should be in check")
	}
	if g.IsCheckmate(White) {
		t.Error("en passant escapes the check")
	}
	if !g.IsLegalMove(sq(t, "d5"), sq(t, "c6")) {
		t.Error("d5xc6 en passant should be legal")
	}
}

func TestPromotionWhiteAndBlack(t *testing.T) {
	g := gameFrom(White, map[string]Piece{
		"a7": {Pawn, White}, "h2": {Pawn, Black}, "b8": {Rook, Black},
		"e1": {King, White}, "e8": {King, Black},
	})
	if !g.IsPromotion(sq(t, "a7"), sq(t, "a8")) {
		t.Error("a7a8 is a promotion")
	}
	play(t, g, "a7", "a8")
	if got := g.Board.At(sq(t, "a8")); got != (Piece{Queen, White}) {
		t.Errorf("a8 = %v", got)
	}
	if !g.IsPromotion(sq(t, "h2"), sq(t, "h1")) {
		t.Error("h2h1 is a promotion")
	}
	play(t, g, "h2", "h1")
	if got := g.Board.At(sq(t, "h1")); got != (Piece{Queen, Black}) {
		t.Errorf("h1 = %v", got)
	}

	// Promotion by capture.
	g = gameFrom(White, map[string]Piece{
		"a7": {Pawn, White}, "b8": {Rook, Black},
		"e1": {King, White}, "e8": {King, Black},
	})
	if !g.IsPromotion(sq(t, "a7"), sq(t, "b8")) {
		t.Error("a7xb8 is a promotion")
	}
	play(t, g, "a7", "b8")
	if got := g.Board.At(sq(t, "b8")); got != (Piece{Queen, White}) {
		t.Errorf("b8 = %v", got)
	}
}

func TestPawnCannotJumpOnDoubleMove(t *testing.T) {
	g := gameFrom(White, map[string]Piece{
		"e2": {Pawn, White}, "e3": {Knight, White},
		"e1": {King, White}, "e8": {King, Black},
	})
	if g.IsLegalMove(sq(t, "e2"), sq(t, "e4")) {
		t.Error("pawn cannot jump over e3")
	}
}

func TestCannotCastleWithCapturedRook(t *testing.T) {
	g := gameFrom(White, map[string]Piece{"e1": {King, White}, "e8": {King, Black}})
	if g.IsLegalMove(sq(t, "e1"), sq(t, "g1")) {
		t.Error("cannot castle without a rook")
	}
}

func TestQueensideCastleAllowedWhenBFileAttacked(t *testing.T) {
	g := gameFrom(White, map[string]Piece{
		"e1": {King, White}, "a1": {Rook, White}, "b8": {Rook, Black}, "h8": {King, Black},
	})
	if !g.IsLegalMove(sq(t, "e1"), sq(t, "c1")) {
		t.Error("queenside castling is allowed when only b1 is attacked")
	}
}

func TestCastleBlockedThroughCheck(t *testing.T) {
	g := gameFrom(White, map[string]Piece{
		"e1": {King, White}, "h1": {Rook, White}, "f8": {Rook, Black}, "h8": {King, Black},
	})
	if g.IsLegalMove(sq(t, "e1"), sq(t, "g1")) {
		t.Error("cannot castle through an attacked f1")
	}
}

func TestApplyMoveCastleMovesRook(t *testing.T) {
	g := gameFrom(White, map[string]Piece{
		"e1": {King, White}, "h1": {Rook, White}, "e8": {King, Black},
	})
	g.ApplyMove(sq(t, "e1"), sq(t, "g1"))
	if got := g.Board.At(sq(t, "f1")); got != (Piece{Rook, White}) {
		t.Errorf("f1 = %v", got)
	}
	if got := g.Board.At(sq(t, "h1")); got.Type != Empty {
		t.Errorf("h1 = %v", got)
	}
	if g.CurrentTurn != Black {
		t.Error("turn should pass to black")
	}
}

func TestApplyMoveEnPassantRemovesPawn(t *testing.T) {
	g := gameFrom(White, map[string]Piece{
		"e5": {Pawn, White}, "d7": {Pawn, Black},
		"e1": {King, White}, "e8": {King, Black},
	})
	g.CurrentTurn = Black
	play(t, g, "d7", "d5")
	play(t, g, "e5", "d6")
	if got := g.Board.At(sq(t, "d5")); got.Type != Empty {
		t.Errorf("d5 = %v, want empty", got)
	}
	if got := g.Board.At(sq(t, "d6")); got != (Piece{Pawn, White}) {
		t.Errorf("d6 = %v", got)
	}
}

func TestIsLegalMoveLeavesBoardUnchanged(t *testing.T) {
	g := gameFrom(White, map[string]Piece{
		"e1": {King, White}, "h1": {Rook, White}, "e2": {Pawn, White},
		"e8": {King, Black}, "e7": {Rook, Black},
	})
	before := g.Board.Placement()
	for _, m := range [][2]string{{"e1", "g1"}, {"e2", "e5"}, {"e2", "e4"}, {"e1", "d1"}} {
		g.IsLegalMove(sq(t, m[0]), sq(t, m[1]))
		if g.Board.Placement() != before || g.CurrentTurn != White {
			t.Fatalf("board or turn changed after checking %v", m)
		}
	}
}

func TestParsePositionRoundTrip(t *testing.T) {
	for _, s := range []string{"a1", "h8", "e4"} {
		p, err := ParsePosition(s)
		if err != nil || p.String() != s {
			t.Errorf("%q: got %v, %v", s, p, err)
		}
	}
	for _, s := range []string{"i1", "a9", "", "a", "a10"} {
		if _, err := ParsePosition(s); err == nil {
			t.Errorf("%q should fail", s)
		}
	}
}

func TestPlacementStartingPosition(t *testing.T) {
	want := "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR"
	if got := NewBoard().Placement(); got != want {
		t.Errorf("got %q", got)
	}
}
