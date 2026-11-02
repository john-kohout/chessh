package main

import (
	"encoding/json"
	"time"
)

const (
	EventGameStarted = "GameStarted"
	EventMoveMade    = "MoveMade"
	EventGameEnded   = "GameEnded"
)

const (
	OutcomeCheckmate   = "checkmate"
	OutcomeStalemate   = "stalemate"
	OutcomeResignation = "resignation"
)

// Event is one fact about a game. Rows of these are the only thing we store.
type Event struct {
	Sequence  int64 // assigned by the store
	GameID    string
	Version   int // 1-based per game
	Type      string
	Data      json.RawMessage
	CreatedAt time.Time // assigned by the store
}

type PlayerRef struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

type GameStartedData struct {
	White PlayerRef `json:"white"`
	Black PlayerRef `json:"black"`
}

type MoveMadeData struct {
	Color     string `json:"color"`
	Piece     string `json:"piece"`
	From      string `json:"from"`
	To        string `json:"to"`
	Promotion string `json:"promotion,omitempty"`
}

type GameEndedData struct {
	Outcome string `json:"outcome"`
	Winner  string `json:"winner,omitempty"` // "white" or "black"; empty for stalemate
}
