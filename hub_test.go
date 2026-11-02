package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"sync"
	"testing"
	"time"
)

var (
	carol = PlayerRef{Key: "key-carol", Name: "carol"}
	dave  = PlayerRef{Key: "key-dave", Name: "dave"}
)

const (
	shortTimeout = 100 * time.Millisecond
	pastTimeout  = 2 * shortTimeout
	longTimeout  = time.Minute // for tests where the timer must not fire
)

// hubFixture is a real database, write side, and projector around one Hub.
// Tests call catchUp themselves, so projection lag is whatever the test wants.
type hubFixture struct {
	t         *testing.T
	db        *sql.DB
	store     *EventStore
	commands  *Commands
	projector *Projector
	hub       *Hub
}

func newHubFixture(t *testing.T, timeout time.Duration) *hubFixture {
	t.Helper()
	db := testDB(t)
	store := NewEventStore(db)
	commands := NewCommands(store, nil)
	f := &hubFixture{t: t, db: db, store: store, commands: commands}
	f.hub = NewHub(db, commands, timeout, func(ctx context.Context) error { return f.projector.CatchUp(ctx) })
	f.projector = NewProjector(db, store, f.hub.Broadcast, ActiveGames{})
	stopTimersAtCleanup(t, f.hub)
	f.catchUp()
	return f
}

// stopTimersAtCleanup keeps timers from firing after the test's database closes.
func stopTimersAtCleanup(t *testing.T, h *Hub) {
	t.Cleanup(func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, running := range h.timers {
			running.timer.Stop()
		}
	})
}

func (f *hubFixture) catchUp() {
	f.t.Helper()
	if err := f.projector.CatchUp(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *hubFixture) join(players ...PlayerRef) []*Session {
	f.t.Helper()
	var sessions []*Session
	for _, p := range players {
		s := f.hub.Connect(context.Background(), p)
		if err := f.hub.JoinQueue(context.Background(), s); err != nil {
			f.t.Fatal(err)
		}
		sessions = append(sessions, s)
	}
	return sessions
}

// startGame matches alice (White) with bob (Black) and projects the game.
func (f *hubFixture) startGame() (gameID string, white, black *Session) {
	f.t.Helper()
	sessions := f.join(alice, bob)
	f.catchUp()
	game := f.activeGame(alice)
	return game.GameID, sessions[0], sessions[1]
}

// onlyGameID reads the event store, for tests where no projection shows the game.
func (f *hubFixture) onlyGameID() string {
	f.t.Helper()
	var gameID string
	if err := f.db.QueryRow(`SELECT game_id FROM events LIMIT 1`).Scan(&gameID); err != nil {
		f.t.Fatal(err)
	}
	return gameID
}

func (f *hubFixture) activeGame(p PlayerRef) ActiveGame {
	f.t.Helper()
	game, ok, err := GetActiveGameForPlayer(context.Background(), f.db, p.Key)
	if err != nil {
		f.t.Fatal(err)
	}
	if !ok {
		f.t.Fatalf("%s has no active game", p.Name)
	}
	return game
}

func (f *hubFixture) hasActiveGame(p PlayerRef) bool {
	f.t.Helper()
	_, ok, err := GetActiveGameForPlayer(context.Background(), f.db, p.Key)
	if err != nil {
		f.t.Fatal(err)
	}
	return ok
}

// endings returns the data of every GameEnded event of a game.
func (f *hubFixture) endings(gameID string) []GameEndedData {
	f.t.Helper()
	events, err := f.store.Load(context.Background(), gameID)
	if err != nil {
		f.t.Fatal(err)
	}
	var ended []GameEndedData
	for _, e := range events {
		if e.Type == EventGameEnded {
			ended = append(ended, decode[GameEndedData](f.t, e))
		}
	}
	return ended
}

func (f *hubFixture) waitForResignation(gameID, winner string) {
	f.t.Helper()
	waitFor(f.t, "GameEnded", func() bool { return len(f.endings(gameID)) > 0 })
	f.wantOneResignation(gameID, winner)
}

func (f *hubFixture) wantOneResignation(gameID, winner string) {
	f.t.Helper()
	want := GameEndedData{Outcome: OutcomeResignation, Winner: winner}
	if got := f.endings(gameID); len(got) != 1 || got[0] != want {
		f.t.Fatalf("endings = %+v, want one %+v", got, want)
	}
}

func (f *hubFixture) waitForNoTimers() {
	f.t.Helper()
	waitFor(f.t, "timers to finish", func() bool {
		f.hub.mu.Lock()
		defer f.hub.mu.Unlock()
		return len(f.hub.timers) == 0
	})
}

func signaled(s *Session) bool {
	select {
	case <-s.Updates:
		return true
	default:
		return false
	}
}

func isClosed(s *Session) bool {
	select {
	case <-s.Closed:
		return true
	default:
		return false
	}
}

func TestQueueMatchesTwoPlayers(t *testing.T) {
	f := newHubFixture(t, longTimeout)
	gameID, white, black := f.startGame()

	events, err := f.store.Load(context.Background(), gameID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != EventGameStarted {
		t.Fatalf("events = %+v, want one GameStarted", events)
	}
	if d := decode[GameStartedData](t, events[0]); d.White != alice || d.Black != bob {
		t.Fatalf("started = %+v, want alice White and bob Black", d)
	}
	if !signaled(white) || !signaled(black) {
		t.Fatal("projecting the game did not signal both players")
	}
}

func TestQueueDoesNotMatchSameKey(t *testing.T) {
	f := newHubFixture(t, longTimeout)
	ctx := context.Background()
	first := f.join(alice)[0]
	second := f.hub.Connect(ctx, alice)
	if !isClosed(first) {
		t.Fatal("first session was not closed")
	}

	f.join(bob) // alice left the line when she reconnected, so bob waits
	if n := countEvents(t, f.db); n != 0 {
		t.Fatalf("%d events, want no game", n)
	}

	if err := f.hub.JoinQueue(ctx, second); err != nil { // she chooses Play
		t.Fatal(err)
	}
	f.catchUp()
	if game := f.activeGame(alice); game.White != bob || game.Black != alice {
		t.Fatalf("game = %+v, want bob White and alice Black", game)
	}
}

func TestQueueSameKeyJoiningTwiceDoesNotMatch(t *testing.T) {
	f := newHubFixture(t, longTimeout)
	first := f.join(alice)[0]
	if err := f.hub.JoinQueue(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if n := countEvents(t, f.db); n != 0 {
		t.Fatalf("%d events, want no game", n)
	}
}

func TestQueueRestoresOrderWhenStartFails(t *testing.T) {
	f := newHubFixture(t, longTimeout)
	ctx := context.Background()
	if _, err := f.db.Exec(`CREATE TRIGGER refuse BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	f.join(alice)
	for _, p := range []PlayerRef{bob, carol} {
		if err := f.hub.JoinQueue(ctx, f.hub.Connect(ctx, p)); err == nil {
			t.Fatalf("JoinQueue for %s returned no error when StartGame failed", p.Name)
		}
	}
	if _, err := f.db.Exec(`DROP TRIGGER refuse`); err != nil {
		t.Fatal(err)
	}

	f.join(dave) // everyone waiting is matched, in their original order
	f.catchUp()
	if game := f.activeGame(alice); game.White != alice || game.Black != bob {
		t.Fatalf("game = %+v, want alice White and bob Black", game)
	}
	if game := f.activeGame(dave); game.White != carol || game.Black != dave {
		t.Fatalf("game = %+v, want carol White and dave Black", game)
	}
}

func TestJoinQueueRefusesPlayerWithActiveGame(t *testing.T) {
	f := newHubFixture(t, longTimeout)
	_, white, _ := f.startGame()

	if err := f.hub.JoinQueue(context.Background(), white); !errors.Is(err, ErrAlreadyInGame) {
		t.Fatalf("got %v, want ErrAlreadyInGame", err)
	}
	f.join(carol)
	f.catchUp()
	if f.hasActiveGame(carol) {
		t.Fatal("carol was matched with a player who already has a game")
	}
}

func TestJoinQueueRightAfterStartGameIsRefused(t *testing.T) {
	f := newHubFixture(t, longTimeout)
	ctx := context.Background()
	first := f.join(alice, bob)[0] // no catchUp: only JoinQueue can bring the projection up to date

	if err := f.hub.JoinQueue(ctx, first); !errors.Is(err, ErrAlreadyInGame) {
		t.Fatalf("got %v, want ErrAlreadyInGame", err)
	}
	if err := f.hub.JoinQueue(ctx, f.hub.Connect(ctx, bob)); !errors.Is(err, ErrAlreadyInGame) {
		t.Fatalf("after reconnecting: got %v, want ErrAlreadyInGame", err)
	}
}

func TestJoinQueueRefusesReplacedSession(t *testing.T) {
	f := newHubFixture(t, longTimeout)
	ctx := context.Background()
	first := f.hub.Connect(ctx, alice)
	f.hub.Connect(ctx, alice)

	if err := f.hub.JoinQueue(ctx, first); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("got %v, want ErrSessionGone", err)
	}
}

func TestLeaveQueue(t *testing.T) {
	f := newHubFixture(t, longTimeout)
	departed := f.join(alice)[0]
	f.hub.LeaveQueue(departed)

	f.join(bob)
	if n := countEvents(t, f.db); n != 0 {
		t.Fatalf("%d events, want bob still waiting", n)
	}
	f.join(carol)
	f.catchUp()
	if game := f.activeGame(bob); game.White != bob || game.Black != carol {
		t.Fatalf("game = %+v, want bob White and carol Black", game)
	}
	if f.hasActiveGame(alice) {
		t.Fatal("the departed player was matched")
	}
}

func TestBroadcastSignalsEverySessionWithoutBlocking(t *testing.T) {
	f := newHubFixture(t, longTimeout)
	ctx := context.Background()
	sessions := []*Session{f.hub.Connect(ctx, alice), f.hub.Connect(ctx, bob), f.hub.Connect(ctx, carol)}

	for i := 0; i < 100; i++ {
		f.hub.Broadcast()
	}
	for _, s := range sessions {
		if !signaled(s) {
			t.Fatalf("%s got no signal", s.Player.Name)
		}
		if signaled(s) {
			t.Fatalf("%s has more than one signal pending", s.Player.Name)
		}
	}
}

func TestDisconnectStartsTimerAndResigns(t *testing.T) {
	f := newHubFixture(t, shortTimeout)
	gameID, white, _ := f.startGame()

	f.hub.Disconnect(context.Background(), white)
	f.waitForResignation(gameID, "black")
}

func TestDisconnectIgnoresCancelledContext(t *testing.T) {
	// The SSH session's context is usually already over by the time we disconnect.
	f := newHubFixture(t, longTimeout)
	gameID, white, _ := f.startGame()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	f.hub.Disconnect(ctx, white)
	if _, ok := f.hub.OpponentDeadline(gameID, alice.Key); !ok {
		t.Fatal("no timer started")
	}
}

func TestDisconnectRightAfterStartGameGetsTimer(t *testing.T) {
	f := newHubFixture(t, shortTimeout)
	first := f.join(alice, bob)[0] // no catchUp: only Disconnect can bring the projection up to date

	f.hub.Disconnect(context.Background(), first)
	f.waitForResignation(f.onlyGameID(), "black")
}

func TestDisconnectRetriesWhenCatchUpFails(t *testing.T) {
	f := newHubFixture(t, longTimeout)
	first := f.join(alice, bob)[0]
	calls := 0
	f.hub.catchUp = func(ctx context.Context) error {
		if calls++; calls <= 2 {
			return errors.New("busy")
		}
		return f.projector.CatchUp(ctx)
	}

	f.hub.Disconnect(context.Background(), first)
	if _, ok := f.hub.OpponentDeadline(f.onlyGameID(), alice.Key); !ok {
		t.Fatal("a failed catch-up lost the timer")
	}
}

func TestDisconnectRetriesDuringRebuild(t *testing.T) {
	f := newHubFixture(t, longTimeout)
	gameID, white, _ := f.startGame()
	if _, err := f.db.Exec(`DROP TABLE active_games`); err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.hub.catchUp = func(ctx context.Context) error {
		if calls++; calls <= 2 {
			return nil // the table is still missing
		}
		return f.projector.CatchUp(ctx)
	}

	f.hub.Disconnect(context.Background(), white)
	if _, ok := f.hub.OpponentDeadline(gameID, alice.Key); !ok {
		t.Fatal("the rebuild lost the timer")
	}
}

func TestQueueAndDisconnectWhenCatchUpKeepsFailing(t *testing.T) {
	f := newHubFixture(t, longTimeout)
	ctx := context.Background()
	gameID, white, _ := f.startGame()
	carolSession := f.hub.Connect(ctx, carol)
	f.hub.catchUp = func(context.Context) error { return errors.New("busy") }

	var joinErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); joinErr = f.hub.JoinQueue(ctx, carolSession) }()
	go func() { defer wg.Done(); f.hub.Disconnect(ctx, white) }()
	wg.Wait()

	if joinErr == nil {
		t.Fatal("JoinQueue accepted a player without an up-to-date projection")
	}
	if _, ok := f.hub.OpponentDeadline(gameID, alice.Key); !ok {
		t.Fatal("a game the projection already showed got no timer")
	}
}

func TestReconnectCancelsTimer(t *testing.T) {
	f := newHubFixture(t, shortTimeout)
	ctx := context.Background()
	gameID, white, _ := f.startGame()

	f.hub.Disconnect(ctx, white)
	f.hub.Connect(ctx, alice)
	time.Sleep(pastTimeout)
	if got := f.endings(gameID); len(got) != 0 {
		t.Fatalf("endings = %+v, want none", got)
	}
}

func TestReconnectCancelsTimerWithoutProjection(t *testing.T) {
	f := newHubFixture(t, shortTimeout)
	ctx := context.Background()
	gameID, white, _ := f.startGame()

	f.hub.Disconnect(ctx, white)
	if _, err := f.db.Exec(`DROP TABLE active_games`); err != nil {
		t.Fatal(err)
	}
	f.hub.Connect(ctx, alice)
	time.Sleep(pastTimeout)
	if got := f.endings(gameID); len(got) != 0 {
		t.Fatalf("endings = %+v, want none", got)
	}
}

func TestOpponentDeadlineReported(t *testing.T) {
	f := newHubFixture(t, longTimeout)
	ctx := context.Background()
	gameID, white, black := f.startGame()
	signaled(black) // clear the signal from the game starting

	if _, ok := f.hub.OpponentDeadline(gameID, alice.Key); ok {
		t.Fatal("deadline reported while connected")
	}
	before := time.Now()
	f.hub.Disconnect(ctx, white)
	deadline, ok := f.hub.OpponentDeadline(gameID, alice.Key)
	if !ok {
		t.Fatal("no deadline while disconnected")
	}
	if deadline.Before(before.Add(longTimeout)) || deadline.After(time.Now().Add(longTimeout)) {
		t.Fatalf("deadline %v is not one timeout from the disconnect", deadline)
	}
	if !signaled(black) {
		t.Fatal("opponent was not told about the countdown")
	}

	f.hub.Connect(ctx, alice)
	if _, ok := f.hub.OpponentDeadline(gameID, alice.Key); ok {
		t.Fatal("deadline reported after reconnect")
	}
	if !signaled(black) {
		t.Fatal("opponent was not told the countdown stopped")
	}
}

func TestReplacedSessionDoesNotStartTimer(t *testing.T) {
	f := newHubFixture(t, shortTimeout)
	ctx := context.Background()
	gameID, first, _ := f.startGame()

	second := f.hub.Connect(ctx, alice)
	if !isClosed(first) {
		t.Fatal("first session's Closed is still open")
	}
	if isClosed(second) {
		t.Fatal("new session's Closed is closed")
	}

	f.hub.Disconnect(ctx, first)
	if _, ok := f.hub.OpponentDeadline(gameID, alice.Key); ok {
		t.Fatal("cleaning up the replaced session started a timer")
	}
	signaled(second)
	f.hub.Broadcast()
	if !signaled(second) {
		t.Fatal("new session is no longer registered")
	}
	time.Sleep(pastTimeout)
	if got := f.endings(gameID); len(got) != 0 {
		t.Fatalf("endings = %+v, want none", got)
	}
}

func TestDisconnectFirstLosesWhenBothLeave(t *testing.T) {
	f := newHubFixture(t, shortTimeout)
	ctx := context.Background()
	gameID, white, black := f.startGame()

	f.hub.Disconnect(ctx, white)
	time.Sleep(shortTimeout / 2)
	f.hub.Disconnect(ctx, black)

	f.waitForResignation(gameID, "black")
	f.waitForNoTimers()
	time.Sleep(shortTimeout / 2) // let the second timer's Resign finish
	f.wantOneResignation(gameID, "black")
}

func TestTimerAfterGameEndsDoesNothing(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)

	f := newHubFixture(t, 2*shortTimeout)
	ctx := context.Background()
	gameID, white, _ := f.startGame()

	f.hub.Disconnect(ctx, white)
	if err := f.commands.Resign(ctx, gameID, bob.Key); err != nil { // bob gives up first
		t.Fatal(err)
	}
	f.waitForNoTimers()
	time.Sleep(shortTimeout / 2) // let the timer's Resign finish

	log.SetOutput(os.Stderr) // also orders our read after any log write
	f.wantOneResignation(gameID, "white")
	if logged.Len() != 0 {
		t.Fatalf("logged %q, want nothing", logged.String())
	}
}

func TestDisconnectWithoutActiveGameDoesNothing(t *testing.T) {
	f := newHubFixture(t, shortTimeout)
	queued := f.join(alice)[0]

	f.hub.Disconnect(context.Background(), queued)
	f.waitForNoTimers()

	f.join(bob) // alice left the queue when she disconnected
	time.Sleep(pastTimeout)
	if n := countEvents(t, f.db); n != 0 {
		t.Fatalf("%d events, want none", n)
	}
}

func TestQueueLeaverDuringMatchGetsTimer(t *testing.T) {
	// The first player disconnects as the game is written. Disconnect looks too
	// early to see it (a no-op catchUp stands in for that), so only JoinQueue
	// can start their timer.
	f := newHubFixture(t, shortTimeout)
	ctx := context.Background()
	first := f.join(alice)[0]
	f.hub.catchUp = func(context.Context) error { return nil }
	var once sync.Once
	f.commands.onAppended = func() {
		once.Do(func() { f.hub.Disconnect(ctx, first) })
	}

	f.join(bob)
	f.waitForResignation(f.onlyGameID(), "black")
}

// restart makes a new Hub over the same database, as a server restart would,
// with the game at Black's turn.
func restart(t *testing.T, f *hubFixture, gameID string) *Hub {
	t.Helper()
	ctx := context.Background()
	if err := f.commands.MakeMove(ctx, gameID, alice.Key, sq(t, "e2"), sq(t, "e4")); err != nil {
		t.Fatal(err)
	}
	f.catchUp()
	h := NewHub(f.db, f.commands, shortTimeout, f.projector.CatchUp)
	stopTimersAtCleanup(t, h)
	if err := h.RestoreTimers(ctx); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestRestoreTimersPlayerToMoveLoses(t *testing.T) {
	f := newHubFixture(t, longTimeout)
	gameID, _, _ := f.startGame()
	h := restart(t, f, gameID)

	white, ok := h.OpponentDeadline(gameID, alice.Key)
	if !ok {
		t.Fatal("White has no timer")
	}
	black, ok := h.OpponentDeadline(gameID, bob.Key)
	if !ok {
		t.Fatal("Black has no timer")
	}
	if gap := white.Sub(black); gap < 900*time.Millisecond || gap > 1100*time.Millisecond {
		t.Fatalf("White's deadline is %v after Black's, want about 1s", gap)
	}
	f.waitForResignation(gameID, "white")
}

func TestRestoreTimersCancelledByReconnect(t *testing.T) {
	f := newHubFixture(t, longTimeout)
	ctx := context.Background()
	gameID, _, _ := f.startGame()
	h := restart(t, f, gameID)

	h.Connect(ctx, alice)
	black := h.Connect(ctx, bob)
	time.Sleep(pastTimeout)
	if got := f.endings(gameID); len(got) != 0 {
		t.Fatalf("endings = %+v, want none", got)
	}
	if err := h.MakeMove(ctx, black, gameID, sq(t, "e7"), sq(t, "e5")); err != nil {
		t.Fatalf("game did not continue: %v", err)
	}
}

func TestMakeMoveRejectsWrongGame(t *testing.T) {
	f := newHubFixture(t, longTimeout)
	ctx := context.Background()
	gameID, white, _ := f.startGame()
	outsider := f.hub.Connect(ctx, carol)

	if err := f.hub.MakeMove(ctx, outsider, gameID, sq(t, "e2"), sq(t, "e4")); !errors.Is(err, ErrNotInGame) {
		t.Fatalf("got %v, want ErrNotInGame", err)
	}
	if err := f.hub.MakeMove(ctx, white, gameID, sq(t, "e2"), sq(t, "e4")); err != nil {
		t.Fatalf("a player's own move failed: %v", err)
	}
}
