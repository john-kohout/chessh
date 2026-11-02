package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"slices"
	"sync"
	"time"
)

var (
	ErrAlreadyInGame = errors.New("already in a game")
	ErrSessionGone   = errors.New("session has disconnected or been replaced")
)

const (
	findGameRetryInterval = 50 * time.Millisecond
	findGameRetryLimit    = 2 * time.Second
	resignRetryInterval   = time.Second
)

// Session is one player's live connection.
type Session struct {
	Player  PlayerRef
	Updates <-chan struct{} // capacity 1; a signal to re-read, never data
	Closed  <-chan struct{} // closed when another connection replaces this one

	updates chan struct{}
	closed  chan struct{}
}

type timerKey struct{ gameID, playerKey string }

type disconnectTimer struct {
	timer    *time.Timer
	deadline time.Time
}

// Hub is a live registry of who is connected, who is waiting for a game, and
// who is about to lose by disconnecting. It holds no game state.
//
// One active game per key holds by construction: a key joins the queue only
// if it has no active game, and it can be in the queue once.
//
// mu is never held across a database call: the database has one connection,
// and Broadcast runs while the projector is working. For the same reason,
// catchUp, which ends in Broadcast, is never called with mu held.
type Hub struct {
	db       *sql.DB
	commands *Commands
	timeout  time.Duration
	catchUp  func(context.Context) error

	mu       sync.Mutex
	sessions map[string]*Session // by player key
	queue    []*Session          // first in line is first
	timers   map[timerKey]*disconnectTimer
}

// NewHub makes a Hub. catchUp is Projector.CatchUp: it lets the hub see a
// player's own events before deciding something that depends on them.
func NewHub(db *sql.DB, commands *Commands, timeout time.Duration, catchUp func(context.Context) error) *Hub {
	return &Hub{
		db:       db,
		commands: commands,
		timeout:  timeout,
		catchUp:  catchUp,
		sessions: map[string]*Session{},
		timers:   map[timerKey]*disconnectTimer{},
	}
}

// Connect registers a player, replacing any connection with the same key, and
// cancels their disconnect timers. It needs no query, so a lagging projection
// can't leave a timer running.
func (h *Hub) Connect(_ context.Context, player PlayerRef) *Session {
	updates, closed := make(chan struct{}, 1), make(chan struct{})
	s := &Session{Player: player, Updates: updates, Closed: closed, updates: updates, closed: closed}

	h.mu.Lock()
	if old, ok := h.sessions[player.Key]; ok {
		close(old.closed)
		h.removeFromQueue(old) // the new session starts at the menu
	}
	h.sessions[player.Key] = s

	cancelled := false
	for key, running := range h.timers {
		if key.playerKey == player.Key {
			running.timer.Stop()
			delete(h.timers, key)
			cancelled = true
		}
	}
	h.mu.Unlock()

	if cancelled {
		h.Broadcast() // the opponent's countdown stops
	}
	return s
}

// Disconnect removes a session and, if the player is in a game, starts their
// timer. A replaced session has nothing to clean up: its successor owns the key.
func (h *Hub) Disconnect(ctx context.Context, s *Session) {
	key := s.Player.Key
	h.mu.Lock()
	if h.sessions[key] != s {
		h.mu.Unlock()
		return
	}
	delete(h.sessions, key)
	h.removeFromQueue(s)
	h.mu.Unlock()

	// The context of a connection that just ended is usually already cancelled.
	game, ok, err := h.findGame(context.WithoutCancel(ctx), key)
	if err != nil {
		log.Printf("hub: finding %s's game: %v", s.Player.Name, err)
	}
	if ok && h.startTimer(game.GameID, key, h.timeout) {
		h.Broadcast() // the opponent's countdown starts
	}
}

// findGame catches the projection up, then reads it, so the answer includes
// every event written before the call. It retries a failed catch-up or a
// rebuild. If catching up never succeeds, it returns the error along with
// whatever the projection showed.
func (h *Hub) findGame(ctx context.Context, playerKey string) (ActiveGame, bool, error) {
	giveUp := time.Now().Add(findGameRetryLimit)
	for {
		catchUpErr := h.catchUp(ctx)
		game, ok, err := GetActiveGameForPlayer(ctx, h.db, playerKey)
		retry := catchUpErr != nil || errors.Is(err, ErrProjectionUnavailable)
		if !retry || ctx.Err() != nil || time.Now().After(giveUp) {
			return game, ok, errors.Join(catchUpErr, err)
		}
		time.Sleep(findGameRetryInterval)
	}
}

// JoinQueue puts a player in line and starts a game if two are waiting. The
// first in line plays White.
func (h *Hub) JoinQueue(ctx context.Context, s *Session) error {
	_, inGame, err := h.findGame(ctx, s.Player.Key)
	if err != nil {
		return err
	}
	if inGame {
		return ErrAlreadyInGame
	}

	h.mu.Lock()
	if h.sessions[s.Player.Key] != s {
		h.mu.Unlock()
		return ErrSessionGone
	}
	if !slices.Contains(h.queue, s) {
		h.queue = append(h.queue, s)
	}
	h.mu.Unlock()

	return h.startGames(ctx)
}

func (h *Hub) startGames(ctx context.Context) error {
	for {
		h.mu.Lock()
		if len(h.queue) < 2 {
			h.mu.Unlock()
			return nil
		}
		white, black := h.queue[0], h.queue[1]
		h.queue = h.queue[2:]
		h.mu.Unlock()

		gameID := NewGameID()
		if err := h.commands.StartGame(ctx, gameID, white.Player, black.Player); err != nil {
			h.putBack(white, black)
			return err
		}
		// A player who left while the game was being written couldn't find it
		// to start their own timer.
		h.startTimer(gameID, white.Player.Key, h.timeout)
		h.startTimer(gameID, black.Player.Key, h.timeout)
		h.Broadcast() // the projection's signal may already have gone out
	}
}

// putBack returns players to the front of the queue, in order, unless they
// left or got back in line while their game was failing to start.
func (h *Hub) putBack(players ...*Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var front []*Session
	for _, s := range players {
		if h.sessions[s.Player.Key] == s && !slices.Contains(h.queue, s) {
			front = append(front, s)
		}
	}
	h.queue = append(front, h.queue...)
}

func (h *Hub) LeaveQueue(s *Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removeFromQueue(s)
}

func (h *Hub) removeFromQueue(s *Session) {
	if i := slices.Index(h.queue, s); i >= 0 {
		h.queue = slices.Delete(h.queue, i, i+1)
	}
}

func (h *Hub) MakeMove(ctx context.Context, s *Session, gameID string, from, to Position) error {
	return h.commands.MakeMove(ctx, gameID, s.Player.Key, from, to)
}

// Broadcast tells every session to re-read. The projector calls it while
// working, so it never blocks: a session with a signal pending needs no other.
func (h *Hub) Broadcast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sessions {
		select {
		case s.updates <- struct{}{}:
		default:
		}
	}
}

// OpponentDeadline reports when a disconnected player's timer runs out.
func (h *Hub) OpponentDeadline(gameID, opponentKey string) (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	running, ok := h.timers[timerKey{gameID, opponentKey}]
	if !ok {
		return time.Time{}, false
	}
	return running.deadline, true
}

// RestoreTimers starts a timer for both players of every active game. The
// player to move runs out first, so if neither returns, they lose.
func (h *Hub) RestoreTimers(ctx context.Context) error {
	games, err := ListActiveGames(ctx, h.db)
	if err != nil {
		return err
	}
	for _, game := range games {
		white, black := h.timeout, h.timeout+time.Second
		if game.Game.CurrentTurn == Black {
			white, black = black, white
		}
		h.startTimer(game.GameID, game.White.Key, white)
		h.startTimer(game.GameID, game.Black.Key, black)
	}
	return nil
}

// startTimer reports whether it started one. A connected player needs no
// timer, and a running timer keeps its earlier deadline.
func (h *Hub) startTimer(gameID, playerKey string, timeout time.Duration) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, connected := h.sessions[playerKey]; connected {
		return false
	}
	key := timerKey{gameID, playerKey}
	if _, running := h.timers[key]; running {
		return false
	}
	entry := &disconnectTimer{deadline: time.Now().Add(timeout)}
	entry.timer = time.AfterFunc(timeout, func() { h.expire(key, entry) })
	h.timers[key] = entry
	return true
}

func (h *Hub) expire(key timerKey, entry *disconnectTimer) {
	h.mu.Lock()
	if h.timers[key] != entry { // Connect got here first
		h.mu.Unlock()
		return
	}
	delete(h.timers, key)
	h.mu.Unlock()

	// ErrGameOver is success: the game ended while the timer ran.
	err := h.commands.Resign(context.Background(), key.gameID, key.playerKey)
	if err != nil && !errors.Is(err, ErrGameOver) {
		log.Printf("hub: resigning %s from %s: %v", key.playerKey, key.gameID, err)
		// Try again soon. startTimer skips a player who has reconnected, and if
		// the game has ended, the next Resign returns ErrGameOver.
		if h.startTimer(key.gameID, key.playerKey, resignRetryInterval) {
			h.Broadcast() // the opponent's deadline moved
		}
	}
}
