package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
)

const maxAttempts = 5

// Commands is the write side: load a game's events, ask the aggregate to
// decide, append what it returns.
type Commands struct {
	store      *EventStore
	onAppended func()
}

// NewCommands makes the write side. onAppended runs after each successful
// append and may be nil.
func NewCommands(store *EventStore, onAppended func()) *Commands {
	return &Commands{store: store, onAppended: onAppended}
}

func (c *Commands) StartGame(ctx context.Context, gameID string, white, black PlayerRef) error {
	return c.execute(ctx, gameID, func(a *GameAggregate) ([]Event, error) {
		return a.Start(white, black)
	})
}

func (c *Commands) MakeMove(ctx context.Context, gameID, playerKey string, from, to Position) error {
	return c.execute(ctx, gameID, func(a *GameAggregate) ([]Event, error) {
		return a.MakeMove(playerKey, from, to)
	})
}

func (c *Commands) Resign(ctx context.Context, gameID, playerKey string) error {
	return c.execute(ctx, gameID, func(a *GameAggregate) ([]Event, error) {
		return a.Resign(playerKey)
	})
}

// execute is the whole write path. If another writer got in first, the
// append conflicts, so we reload and decide again.
func (c *Commands) execute(ctx context.Context, gameID string, decide func(*GameAggregate) ([]Event, error)) error {
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var past, events []Event
		if past, err = c.store.Load(ctx, gameID); err != nil {
			return err
		}
		var game *GameAggregate
		if game, err = LoadGameAggregate(gameID, past); err != nil {
			return err
		}
		if events, err = decide(game); err != nil {
			return err
		}
		if err = c.store.Append(ctx, events); errors.Is(err, ErrConflict) {
			continue
		} else if err != nil {
			return err
		}
		if len(events) > 0 && c.onAppended != nil {
			c.onAppended()
		}
		return nil
	}
	return err
}

// NewGameID returns "g_" and 16 random hex characters.
func NewGameID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "g_" + hex.EncodeToString(b)
}
