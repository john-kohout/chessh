package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrProjectionUnavailable means the table is missing, for example mid-rebuild.
var ErrProjectionUnavailable = errors.New("projection unavailable")

// ActiveGame is one row of active_games: a game in progress, ready to draw.
type ActiveGame struct {
	GameID     string
	White      PlayerRef
	Black      PlayerRef
	Game       *Game // includes turn and last move
	LegalMoves []Move
	InCheck    bool
	Version    int
}

// ActiveGames keeps one row per game in progress.
type ActiveGames struct{}

func (ActiveGames) Name() string { return "active_games" }

func (ActiveGames) Create(tx *sql.Tx) error {
	for _, stmt := range []string{
		`CREATE TABLE active_games (
		   game_id     TEXT PRIMARY KEY,
		   white_key   TEXT NOT NULL,
		   white_name  TEXT NOT NULL,
		   black_key   TEXT NOT NULL,
		   black_name  TEXT NOT NULL,
		   board       TEXT NOT NULL,
		   turn        TEXT NOT NULL,
		   state       TEXT NOT NULL,   -- JSON of Game
		   legal_moves TEXT NOT NULL,   -- JSON
		   in_check    INTEGER NOT NULL,
		   version     INTEGER NOT NULL
		 )`,
		`CREATE INDEX active_games_white ON active_games (white_key)`,
		`CREATE INDEX active_games_black ON active_games (black_key)`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func (ActiveGames) Apply(tx *sql.Tx, e Event) error {
	switch e.Type {
	case EventGameStarted:
		var d GameStartedData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return err
		}
		game := NewGame()
		state, moves, err := encodeGame(game)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO active_games
			(game_id, white_key, white_name, black_key, black_name, board, turn, state, legal_moves, in_check, version)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.GameID, d.White.Key, d.White.Name, d.Black.Key, d.Black.Name,
			game.Board.Placement(), game.CurrentTurn.Name(), state, moves, game.IsInCheck(game.CurrentTurn), e.Version)
		return err

	case EventMoveMade:
		var d MoveMadeData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return err
		}
		from, err := ParsePosition(d.From)
		if err != nil {
			return err
		}
		to, err := ParsePosition(d.To)
		if err != nil {
			return err
		}
		var state string
		if err := tx.QueryRow(`SELECT state FROM active_games WHERE game_id = ?`, e.GameID).Scan(&state); err != nil {
			return fmt.Errorf("move for game %s: %w", e.GameID, err)
		}
		var game Game
		if err := json.Unmarshal([]byte(state), &game); err != nil {
			return err
		}
		game.ApplyMove(from, to)
		newState, moves, err := encodeGame(&game)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE active_games
			SET board = ?, turn = ?, state = ?, legal_moves = ?, in_check = ?, version = ?
			WHERE game_id = ?`,
			game.Board.Placement(), game.CurrentTurn.Name(), newState, moves, game.IsInCheck(game.CurrentTurn), e.Version, e.GameID)
		return err

	case EventGameEnded:
		_, err := tx.Exec(`DELETE FROM active_games WHERE game_id = ?`, e.GameID)
		return err
	}
	return nil
}

func encodeGame(g *Game) (state, legalMoves []byte, err error) {
	if state, err = json.Marshal(g); err != nil {
		return nil, nil, err
	}
	legalMoves, err = json.Marshal(g.LegalMoves())
	return state, legalMoves, err
}

const activeGameColumns = `SELECT game_id, white_key, white_name, black_key, black_name, state, legal_moves, in_check, version FROM active_games`

func GetActiveGame(ctx context.Context, db *sql.DB, gameID string) (ActiveGame, bool, error) {
	return queryOneActiveGame(ctx, db, activeGameColumns+` WHERE game_id = ?`, gameID)
}

func GetActiveGameForPlayer(ctx context.Context, db *sql.DB, playerKey string) (ActiveGame, bool, error) {
	return queryOneActiveGame(ctx, db, activeGameColumns+` WHERE white_key = ? OR black_key = ? LIMIT 1`, playerKey, playerKey)
}

func ListActiveGames(ctx context.Context, db *sql.DB) ([]ActiveGame, error) {
	rows, err := db.QueryContext(ctx, activeGameColumns+` ORDER BY game_id`)
	if err != nil {
		return nil, unavailableIfMissing(err)
	}
	defer rows.Close()
	var games []ActiveGame
	for rows.Next() {
		g, err := scanActiveGame(rows)
		if err != nil {
			return nil, err
		}
		games = append(games, g)
	}
	return games, rows.Err()
}

func queryOneActiveGame(ctx context.Context, db *sql.DB, query string, args ...any) (ActiveGame, bool, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return ActiveGame{}, false, unavailableIfMissing(err)
	}
	defer rows.Close()
	if !rows.Next() {
		return ActiveGame{}, false, rows.Err()
	}
	g, err := scanActiveGame(rows)
	return g, err == nil, err
}

func scanActiveGame(rows *sql.Rows) (ActiveGame, error) {
	var g ActiveGame
	var state, moves string
	if err := rows.Scan(&g.GameID, &g.White.Key, &g.White.Name, &g.Black.Key, &g.Black.Name,
		&state, &moves, &g.InCheck, &g.Version); err != nil {
		return g, err
	}
	g.Game = &Game{}
	if err := json.Unmarshal([]byte(state), g.Game); err != nil {
		return g, err
	}
	return g, json.Unmarshal([]byte(moves), &g.LegalMoves)
}

func unavailableIfMissing(err error) error {
	if strings.Contains(err.Error(), "no such table") {
		return ErrProjectionUnavailable
	}
	return err
}
