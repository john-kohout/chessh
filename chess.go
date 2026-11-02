package main

import (
	"fmt"
	"strings"
)

type Color int

const (
	White Color = iota
	Black
)

func (c Color) String() string {
	if c == White {
		return "White"
	}
	return "Black"
}

type PieceType int

const (
	Empty PieceType = iota
	Pawn
	Rook
	Knight
	Bishop
	Queen
	King
)

type Piece struct {
	Type  PieceType
	Color Color
}

func (p Piece) String() string {
	if p.Type == Empty {
		return " "
	}

	symbols := map[PieceType]string{
		Pawn:   "♟",
		Rook:   "♜",
		Knight: "♞",
		Bishop: "♝",
		Queen:  "♛",
		King:   "♚",
	}

	if p.Color == White {
		whiteSymbols := map[PieceType]string{
			Pawn:   "♙",
			Rook:   "♖",
			Knight: "♘",
			Bishop: "♗",
			Queen:  "♕",
			King:   "♔",
		}
		return whiteSymbols[p.Type]
	}

	return symbols[p.Type]
}

type Position struct {
	Row, Col int
}

func (p Position) Valid() bool {
	return p.Row >= 0 && p.Row < 8 && p.Col >= 0 && p.Col < 8
}

func (p Position) String() string {
	if !p.Valid() {
		return "invalid"
	}
	return fmt.Sprintf("%c%d", 'a'+p.Col, p.Row+1)
}

type Move struct {
	From, To Position
	Piece    Piece
}

type Board [8][8]Piece

func NewBoard() Board {
	var board Board

	board[0] = [8]Piece{
		{Rook, White}, {Knight, White}, {Bishop, White}, {Queen, White},
		{King, White}, {Bishop, White}, {Knight, White}, {Rook, White},
	}
	board[1] = [8]Piece{
		{Pawn, White}, {Pawn, White}, {Pawn, White}, {Pawn, White},
		{Pawn, White}, {Pawn, White}, {Pawn, White}, {Pawn, White},
	}

	for col := range 8 {
		for row := 2; row < 6; row++ {
			board[row][col] = Piece{Empty, White}
		}
	}

	board[6] = [8]Piece{
		{Pawn, Black}, {Pawn, Black}, {Pawn, Black}, {Pawn, Black},
		{Pawn, Black}, {Pawn, Black}, {Pawn, Black}, {Pawn, Black},
	}
	board[7] = [8]Piece{
		{Rook, Black}, {Knight, Black}, {Bishop, Black}, {Queen, Black},
		{King, Black}, {Bishop, Black}, {Knight, Black}, {Rook, Black},
	}

	return board
}

func (b *Board) At(pos Position) Piece {
	if !pos.Valid() {
		return Piece{Empty, White}
	}
	return b[pos.Row][pos.Col]
}

func (b *Board) Set(pos Position, piece Piece) {
	if pos.Valid() {
		b[pos.Row][pos.Col] = piece
	}
}

func (b *Board) Move(from, to Position) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}

	piece := b.At(from)
	if piece.Type == Empty {
		return false
	}

	b.Set(to, piece)
	b.Set(from, Piece{Empty, White})
	return true
}

type Game struct {
	Board           Board
	CurrentTurn     Color
	MoveHistory     []Move
	KingMoved       [2]bool
	RookMoved       [2][2]bool
	EnPassantTarget *Position
}

func NewGame() *Game {
	return &Game{
		Board:           NewBoard(),
		CurrentTurn:     White,
		MoveHistory:     make([]Move, 0),
		KingMoved:       [2]bool{false, false},
		RookMoved:       [2][2]bool{{false, false}, {false, false}},
		EnPassantTarget: nil,
	}
}

func (g *Game) IsValidMove(from, to Position) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}

	piece := g.Board.At(from)
	if piece.Type == Empty || piece.Color != g.CurrentTurn {
		return false
	}

	target := g.Board.At(to)
	if target.Type != Empty && target.Color == piece.Color {
		return false
	}

	return g.isValidPieceMove(piece, from, to)
}

func (g *Game) isValidPieceMove(piece Piece, from, to Position) bool {
	dx := to.Col - from.Col
	dy := to.Row - from.Row

	switch piece.Type {
	case Pawn:
		return g.isValidPawnMove(piece, from, to, dx, dy)
	case Rook:
		return g.isValidRookMove(from, to, dx, dy)
	case Knight:
		return g.isValidKnightMove(dx, dy)
	case Bishop:
		return g.isValidBishopMove(from, to, dx, dy)
	case Queen:
		return g.isValidQueenMove(from, to, dx, dy)
	case King:
		return g.isValidKingMove(piece, from, dx, dy)
	}
	return false
}

func (g *Game) isValidPawnMove(piece Piece, from, to Position, dx, dy int) bool {
	direction := 1
	startRow := 1
	if piece.Color == Black {
		direction = -1
		startRow = 6
	}

	if dx == 0 && g.Board.At(to).Type == Empty {
		if dy == direction {
			return true
		}
		between := Position{from.Row + direction, from.Col}
		if from.Row == startRow && dy == 2*direction && g.Board.At(between).Type == Empty {
			return true
		}
	}

	if abs(dx) == 1 && dy == direction {
		target := g.Board.At(to)
		if target.Type != Empty && target.Color != piece.Color {
			return true
		}

		if g.EnPassantTarget != nil && *g.EnPassantTarget == to {
			return true
		}
	}

	return false
}

func (g *Game) isValidRookMove(from, to Position, dx, dy int) bool {
	if dx != 0 && dy != 0 {
		return false
	}
	return g.isPathClear(from, to)
}

func (g *Game) isValidKnightMove(dx, dy int) bool {
	return (abs(dx) == 2 && abs(dy) == 1) || (abs(dx) == 1 && abs(dy) == 2)
}

func (g *Game) isValidBishopMove(from, to Position, dx, dy int) bool {
	if abs(dx) != abs(dy) {
		return false
	}
	return g.isPathClear(from, to)
}

func (g *Game) isValidQueenMove(from, to Position, dx, dy int) bool {
	return g.isValidRookMove(from, to, dx, dy) || g.isValidBishopMove(from, to, dx, dy)
}

func (g *Game) isValidKingMove(piece Piece, from Position, dx, dy int) bool {
	if abs(dx) <= 1 && abs(dy) <= 1 {
		return true
	}

	if abs(dx) == 2 && dy == 0 && from.Col == 4 && !g.KingMoved[piece.Color] {
		return g.canCastle(piece.Color, dx > 0)
	}

	return false
}

func (g *Game) canCastle(color Color, kingSide bool) bool {
	row := 0
	if color == Black {
		row = 7
	}
	if from := (Position{row, 4}); g.Board.At(from) != (Piece{King, color}) {
		return false
	}

	rookCol := 0
	if kingSide {
		rookCol = 7
	}
	if g.Board.At(Position{row, rookCol}) != (Piece{Rook, color}) {
		return false
	}
	if g.RookMoved[color][rookCol/7] {
		return false
	}

	if g.IsInCheck(color) {
		return false
	}

	// Squares that must be empty, and squares the king crosses (which must be safe).
	emptyFrom, emptyTo := 1, 3
	crossed := []int{3, 2}
	if kingSide {
		emptyFrom, emptyTo = 5, 6
		crossed = []int{5, 6}
	}

	for col := emptyFrom; col <= emptyTo; col++ {
		if g.Board.At(Position{row, col}).Type != Empty {
			return false
		}
	}

	for _, col := range crossed {
		test := g.clone()
		test.Board.Move(Position{row, 4}, Position{row, col})
		if test.IsInCheck(color) {
			return false
		}
	}

	return true
}

func (g *Game) isPathClear(from, to Position) bool {
	dx := sign(to.Col - from.Col)
	dy := sign(to.Row - from.Row)

	current := Position{from.Row + dy, from.Col + dx}

	for current != to {
		if g.Board.At(current).Type != Empty {
			return false
		}
		current.Row += dy
		current.Col += dx
	}

	return true
}

// clone copies everything the rules need. MoveHistory is not copied.
func (g *Game) clone() *Game {
	c := *g
	c.MoveHistory = nil
	if g.EnPassantTarget != nil {
		target := *g.EnPassantTarget
		c.EnPassantTarget = &target
	}
	return &c
}

// IsLegalMove is IsValidMove plus: the mover must not be left in check.
func (g *Game) IsLegalMove(from, to Position) bool {
	if !g.IsValidMove(from, to) {
		return false
	}

	test := g.clone()
	test.executeMove(from, to, g.Board.At(from))
	return !test.IsInCheck(g.CurrentTurn)
}

// IsPromotion reports whether the move takes a pawn to the last rank.
func (g *Game) IsPromotion(from, to Position) bool {
	piece := g.Board.At(from)
	if piece.Type != Pawn || !to.Valid() {
		return false
	}
	lastRow := 7
	if piece.Color == Black {
		lastRow = 0
	}
	return to.Row == lastRow
}

// ApplyMove changes the state unconditionally. It never checks the rules.
func (g *Game) ApplyMove(from, to Position) {
	piece := g.Board.At(from)
	promotion := g.IsPromotion(from, to)

	g.updateRookRights(to)
	g.executeMove(from, to, piece)
	if promotion {
		g.Board.Set(to, Piece{Queen, piece.Color})
	}
	g.updateGameState(from, to, piece)
	g.MoveHistory = append(g.MoveHistory, Move{from, to, piece})
	g.CurrentTurn = 1 - g.CurrentTurn
}

func (g *Game) MakeMove(from, to Position) bool {
	if !g.IsLegalMove(from, to) {
		return false
	}
	g.ApplyMove(from, to)
	return true
}

// updateRookRights records a rook captured on its corner, before it is gone.
func (g *Game) updateRookRights(to Position) {
	captured := g.Board.At(to)
	if captured.Type == Rook && (to.Col == 0 || to.Col == 7) {
		g.RookMoved[captured.Color][to.Col/7] = true
	}
}

func (g *Game) executeMove(from, to Position, piece Piece) {
	if piece.Type == King && abs(to.Col-from.Col) == 2 {
		g.executeCastle(from, to)
	} else if piece.Type == Pawn && abs(to.Col-from.Col) == 1 && g.EnPassantTarget != nil && *g.EnPassantTarget == to {
		g.executeEnPassant(from, to)
	} else {
		g.Board.Move(from, to)
	}
}

func (g *Game) executeCastle(from, to Position) {
	g.Board.Move(from, to)

	rookFromCol := 0
	rookToCol := 3
	if to.Col > from.Col {
		rookFromCol = 7
		rookToCol = 5
	}

	rookFrom := Position{from.Row, rookFromCol}
	rookTo := Position{from.Row, rookToCol}
	g.Board.Move(rookFrom, rookTo)
}

func (g *Game) executeEnPassant(from, to Position) {
	g.Board.Move(from, to)
	capturedPawnRow := from.Row
	g.Board.Set(Position{capturedPawnRow, to.Col}, Piece{Empty, White})
}

func (g *Game) updateGameState(from, to Position, piece Piece) {
	g.EnPassantTarget = nil

	if piece.Type == King {
		g.KingMoved[piece.Color] = true
	} else if piece.Type == Rook {
		switch from.Col {
		case 0:
			g.RookMoved[piece.Color][0] = true
		case 7:
			g.RookMoved[piece.Color][1] = true
		}
	} else if piece.Type == Pawn && abs(to.Row-from.Row) == 2 {
		enPassantRow := (from.Row + to.Row) / 2
		g.EnPassantTarget = &Position{enPassantRow, from.Col}
	}
}

func (g *Game) FindKing(color Color) Position {
	for row := range 8 {
		for col := range 8 {
			pos := Position{row, col}
			piece := g.Board.At(pos)
			if piece.Type == King && piece.Color == color {
				return pos
			}
		}
	}
	return Position{-1, -1}
}

func (g *Game) IsInCheck(color Color) bool {
	kingPos := g.FindKing(color)
	if !kingPos.Valid() {
		return false
	}

	enemyColor := 1 - color

	for row := range 8 {
		for col := range 8 {
			pos := Position{row, col}
			piece := g.Board.At(pos)
			if piece.Type != Empty && piece.Color == enemyColor {
				if g.canPieceAttack(piece, pos, kingPos) {
					return true
				}
			}
		}
	}

	return false
}

func (g *Game) canPieceAttack(piece Piece, from, to Position) bool {
	dx := to.Col - from.Col
	dy := to.Row - from.Row

	switch piece.Type {
	case Pawn:
		direction := 1
		if piece.Color == Black {
			direction = -1
		}
		return abs(dx) == 1 && dy == direction
	case Rook:
		return (dx == 0 || dy == 0) && g.isPathClear(from, to)
	case Knight:
		return (abs(dx) == 2 && abs(dy) == 1) || (abs(dx) == 1 && abs(dy) == 2)
	case Bishop:
		return abs(dx) == abs(dy) && g.isPathClear(from, to)
	case Queen:
		return ((dx == 0 || dy == 0) || (abs(dx) == abs(dy))) && g.isPathClear(from, to)
	case King:
		return abs(dx) <= 1 && abs(dy) <= 1
	}
	return false
}

// LegalMoves lists every legal move for the player to move.
func (g *Game) LegalMoves() []Move {
	var moves []Move
	for row := range 8 {
		for col := range 8 {
			from := Position{row, col}
			piece := g.Board.At(from)
			if piece.Type == Empty || piece.Color != g.CurrentTurn {
				continue
			}
			for toRow := range 8 {
				for toCol := range 8 {
					to := Position{toRow, toCol}
					if g.IsLegalMove(from, to) {
						moves = append(moves, Move{from, to, piece})
					}
				}
			}
		}
	}
	return moves
}

func (g *Game) hasNoLegalMoves(color Color) bool {
	test := g.clone()
	test.CurrentTurn = color
	return len(test.LegalMoves()) == 0
}

func (g *Game) IsCheckmate(color Color) bool {
	return g.IsInCheck(color) && g.hasNoLegalMoves(color)
}

func (g *Game) IsStalemate(color Color) bool {
	return !g.IsInCheck(color) && g.hasNoLegalMoves(color)
}

func (g *Game) GameStatus() string {
	if g.IsCheckmate(g.CurrentTurn) {
		winner := "White"
		if g.CurrentTurn == White {
			winner = "Black"
		}
		return fmt.Sprintf("Checkmate! %s wins!", winner)
	}

	if g.IsInCheck(g.CurrentTurn) {
		return fmt.Sprintf("%s is in check!", g.CurrentTurn)
	}

	return ""
}

func ParsePosition(s string) (Position, error) {
	if len(s) != 2 || s[0] < 'a' || s[0] > 'h' || s[1] < '1' || s[1] > '8' {
		return Position{}, fmt.Errorf("invalid square %q", s)
	}
	return Position{Row: int(s[1] - '1'), Col: int(s[0] - 'a')}, nil
}

// Placement is the piece-placement field of a FEN string, rank 8 first.
func (b Board) Placement() string {
	letters := map[PieceType]string{Pawn: "p", Rook: "r", Knight: "n", Bishop: "b", Queen: "q", King: "k"}
	ranks := make([]string, 0, 8)
	for row := 7; row >= 0; row-- {
		var rank strings.Builder
		empty := 0
		for col := range 8 {
			piece := b[row][col]
			if piece.Type == Empty {
				empty++
				continue
			}
			if empty > 0 {
				fmt.Fprint(&rank, empty)
				empty = 0
			}
			letter := letters[piece.Type]
			if piece.Color == White {
				letter = strings.ToUpper(letter)
			}
			rank.WriteString(letter)
		}
		if empty > 0 {
			fmt.Fprint(&rank, empty)
		}
		ranks = append(ranks, rank.String())
	}
	return strings.Join(ranks, "/")
}

func (c Color) Name() string {
	if c == White {
		return "white"
	}
	return "black"
}

func (t PieceType) Name() string {
	names := map[PieceType]string{Pawn: "pawn", Rook: "rook", Knight: "knight", Bishop: "bishop", Queen: "queen", King: "king"}
	return names[t]
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func sign(x int) int {
	if x > 0 {
		return 1
	}
	if x < 0 {
		return -1
	}
	return 0
}
