package main

import (
	"encoding/json"
	"errors"
	"time"
)

// Replay is a finished or unfinished game as a list of positions to step through.
type Replay struct {
	White, Black    PlayerRef
	Positions       []Board  // len == len(Moves)+1; Positions[0] is the start
	Moves           []string // "f1-b5"
	Outcome, Winner string
	Finished        bool
	EndedAt         time.Time
}

// BuildReplay applies a game's events through the aggregate, the same way
// LoadGameAggregate does, and keeps a copy of the board after every move.
func BuildReplay(events []Event) (Replay, error) {
	if len(events) == 0 {
		return Replay{}, errors.New("no events to replay")
	}
	a := NewGameAggregate(events[0].GameID)
	r := Replay{Positions: []Board{a.Game.Board}}
	for _, e := range events {
		if err := a.Apply(e); err != nil {
			return Replay{}, err
		}
		switch e.Type {
		case EventMoveMade:
			var d MoveMadeData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return Replay{}, err
			}
			r.Moves = append(r.Moves, d.From+"-"+d.To)
			r.Positions = append(r.Positions, a.Game.Board) // Board is an array: this is a copy
		case EventGameEnded:
			var d GameEndedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				return Replay{}, err
			}
			r.Finished, r.Outcome, r.Winner, r.EndedAt = true, d.Outcome, d.Winner, e.CreatedAt
		}
	}
	r.White, r.Black = a.White, a.Black
	return r, nil
}
