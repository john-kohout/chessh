package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// MyGame is one row of my_games: a finished game from one player's side.
type MyGame struct {
	GameID       string
	PlayerKey    string
	OpponentName string
	Color        string // "white" or "black"
	Result       string // "won", "lost", "draw"
	Reason       string // an Outcome value
	MoveCount    int    // half-moves
	StartedAt    time.Time
	EndedAt      time.Time
}

// ResultLabel is the short text for the history list.
func (g MyGame) ResultLabel() string {
	switch {
	case g.Result == "draw":
		return "Draw"
	case g.Reason == OutcomeResignation && g.Result == "won":
		return "Opp. left"
	case g.Reason == OutcomeResignation:
		return "Left"
	case g.Result == "won":
		return "Won"
	}
	return "Lost"
}

// MyGames keeps two rows per finished game, one for each player.
type MyGames struct{}

func (MyGames) Name() string { return "my_games" }

func (MyGames) Create(tx *sql.Tx) error {
	for _, stmt := range []string{
		`CREATE TABLE my_games (
		   game_id       TEXT NOT NULL,
		   player_key    TEXT NOT NULL,
		   opponent_name TEXT NOT NULL,
		   color         TEXT NOT NULL,
		   result        TEXT NOT NULL,
		   reason        TEXT NOT NULL,
		   move_count    INTEGER NOT NULL,
		   started_at    TEXT NOT NULL,
		   ended_at      TEXT NOT NULL,
		   PRIMARY KEY (game_id, player_key)
		 )`,
		`CREATE INDEX my_games_player ON my_games (player_key)`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// Fixed width, so sorting the text sorts by time.
const myGamesTime = "2006-01-02T15:04:05.000000000Z"

// Apply only cares about GameEnded. It replays the game's events, the same
// way the aggregate rebuilds itself, to learn the players and the move count.
func (MyGames) Apply(tx *sql.Tx, e Event) error {
	if e.Type != EventGameEnded {
		return nil
	}
	events, err := LoadInTx(context.Background(), tx, e.GameID)
	if err != nil {
		return err
	}
	a, err := LoadGameAggregate(e.GameID, events)
	if err != nil {
		return err
	}
	moves := 0
	for _, ev := range events {
		if ev.Type == EventMoveMade {
			moves++
		}
	}
	var ended GameEndedData
	if err := json.Unmarshal(e.Data, &ended); err != nil {
		return err
	}
	started := events[0].CreatedAt.UTC().Format(myGamesTime)
	endedAt := e.CreatedAt.UTC().Format(myGamesTime)

	for _, side := range []struct {
		player   PlayerRef
		color    string
		opponent PlayerRef
	}{
		{a.White, "white", a.Black},
		{a.Black, "black", a.White},
	} {
		result := "draw"
		if ended.Winner == side.color {
			result = "won"
		} else if ended.Winner != "" {
			result = "lost"
		}
		if _, err := tx.Exec(`INSERT INTO my_games
			(game_id, player_key, opponent_name, color, result, reason, move_count, started_at, ended_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.GameID, side.player.Key, side.opponent.Name, side.color, result, ended.Outcome, moves, started, endedAt); err != nil {
			return err
		}
	}
	return nil
}

// ListMyGames returns the player's finished games, newest first.
func ListMyGames(ctx context.Context, db *sql.DB, playerKey string) ([]MyGame, error) {
	rows, err := db.QueryContext(ctx, `SELECT game_id, player_key, opponent_name, color, result, reason, move_count, started_at, ended_at
		FROM my_games WHERE player_key = ? ORDER BY ended_at DESC, game_id DESC`, playerKey)
	if err != nil {
		return nil, unavailableIfMissing(err)
	}
	defer rows.Close()
	var games []MyGame
	for rows.Next() {
		var g MyGame
		var started, ended string
		if err := rows.Scan(&g.GameID, &g.PlayerKey, &g.OpponentName, &g.Color, &g.Result, &g.Reason, &g.MoveCount, &started, &ended); err != nil {
			return nil, err
		}
		if g.StartedAt, err = time.Parse(myGamesTime, started); err != nil {
			return nil, err
		}
		if g.EndedAt, err = time.Parse(myGamesTime, ended); err != nil {
			return nil, err
		}
		games = append(games, g)
	}
	return games, rows.Err()
}
