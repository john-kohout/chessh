package main

import (
	"encoding/json"
	"errors"
	"fmt"
)

var (
	ErrAlreadyStarted = errors.New("game already started")
	ErrGameNotStarted = errors.New("game not started")
	ErrGameOver       = errors.New("game is over")
	ErrNotInGame      = errors.New("not a player in this game")
	ErrNotYourTurn    = errors.New("not your turn")
	ErrIllegalMove    = errors.New("illegal move")
)

// GameAggregate decides what may happen next. Commands (Start, MakeMove,
// Resign) return new events and change nothing. Apply changes state.
type GameAggregate struct {
	ID      string
	Version int
	White   PlayerRef
	Black   PlayerRef
	Game    *Game
	Ended   bool
}

func NewGameAggregate(id string) *GameAggregate {
	return &GameAggregate{ID: id, Game: NewGame()}
}

// LoadGameAggregate rebuilds a game by replaying its events.
func LoadGameAggregate(id string, events []Event) (*GameAggregate, error) {
	a := NewGameAggregate(id)
	for _, e := range events {
		if err := a.Apply(e); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// Apply records a fact. It never checks the rules.
func (a *GameAggregate) Apply(e Event) error {
	switch e.Type {
	case EventGameStarted:
		var d GameStartedData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return err
		}
		a.White, a.Black = d.White, d.Black
	case EventMoveMade:
		if err := ApplyMoveEvent(a.Game, e); err != nil {
			return err
		}
	case EventGameEnded:
		a.Ended = true
	default:
		return fmt.Errorf("unknown event type %q", e.Type)
	}
	a.Version = e.Version
	return nil
}

func (a *GameAggregate) Start(white, black PlayerRef) ([]Event, error) {
	if a.Version > 0 {
		return nil, ErrAlreadyStarted
	}
	if white.Key == black.Key {
		return nil, errors.New("a player cannot play both sides")
	}
	return []Event{a.newEvent(1, EventGameStarted, GameStartedData{White: white, Black: black})}, nil
}

func (a *GameAggregate) MakeMove(playerKey string, from, to Position) ([]Event, error) {
	if a.Version == 0 {
		return nil, ErrGameNotStarted
	}
	if a.Ended {
		return nil, ErrGameOver
	}
	var color Color
	switch playerKey {
	case a.White.Key:
		color = White
	case a.Black.Key:
		color = Black
	default:
		return nil, ErrNotInGame
	}
	if a.Game.CurrentTurn != color {
		return nil, ErrNotYourTurn
	}
	if !a.Game.IsLegalMove(from, to) {
		return nil, ErrIllegalMove
	}

	moved := MoveMadeData{
		Color: color.Name(),
		Piece: a.Game.Board.At(from).Type.Name(),
		From:  from.String(),
		To:    to.String(),
	}
	if a.Game.IsPromotion(from, to) {
		moved.Promotion = "queen"
	}
	events := []Event{a.newEvent(a.Version+1, EventMoveMade, moved)}

	// Play the move on a copy to see whether it ends the game.
	after := a.Game.clone()
	after.ApplyMove(from, to)
	opponent := after.CurrentTurn
	switch {
	case after.IsCheckmate(opponent):
		events = append(events, a.newEvent(a.Version+2, EventGameEnded,
			GameEndedData{Outcome: OutcomeCheckmate, Winner: color.Name()}))
	case after.IsStalemate(opponent):
		events = append(events, a.newEvent(a.Version+2, EventGameEnded,
			GameEndedData{Outcome: OutcomeStalemate}))
	}
	return events, nil
}

func (a *GameAggregate) Resign(playerKey string) ([]Event, error) {
	if a.Version == 0 {
		return nil, ErrGameNotStarted
	}
	if a.Ended {
		return nil, ErrGameOver
	}
	winner := ""
	switch playerKey {
	case a.White.Key:
		winner = Black.Name()
	case a.Black.Key:
		winner = White.Name()
	default:
		return nil, ErrNotInGame
	}
	return []Event{a.newEvent(a.Version+1, EventGameEnded,
		GameEndedData{Outcome: OutcomeResignation, Winner: winner})}, nil
}

func (a *GameAggregate) newEvent(version int, eventType string, data any) Event {
	b, err := json.Marshal(data)
	if err != nil {
		panic(err) // the data types above always marshal
	}
	return Event{GameID: a.ID, Version: version, Type: eventType, Data: b}
}
