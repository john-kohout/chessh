package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const boardHeader = "a  b  c  d  e  f  g  h"

// newUIApp is a real App whose projector doesn't run in the background, so the
// test decides when projections catch up.
func newUIApp(t *testing.T, timeout time.Duration) *App {
	t.Helper()
	app := NewApp(testDB(t), timeout)
	stopTimersAtCleanup(t, app.Hub)
	if err := app.Projector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	return app
}

// startedUIApp is a real App running as it does in production.
func startedUIApp(t *testing.T, timeout time.Duration) *App {
	t.Helper()
	app := NewApp(testDB(t), timeout)
	stopTimersAtCleanup(t, app.Hub)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return app
}

// uiDriver plays the part of Bubble Tea: it runs commands in goroutines and
// feeds their messages to Update, one at a time, on the test's goroutine.
type uiDriver struct {
	t    *testing.T
	m    model
	msgs chan tea.Msg
	ctx  context.Context
	quit bool
}

func newUIDriver(t *testing.T, app *App, player PlayerRef) *uiDriver {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	session := app.Hub.Connect(ctx, player)
	d := &uiDriver{t: t, m: newModel(ctx, app, session), msgs: make(chan tea.Msg), ctx: ctx}
	d.run(d.m.Init())
	return d
}

func (d *uiDriver) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		select {
		case d.msgs <- msg:
		case <-d.ctx.Done():
		}
	}()
}

func (d *uiDriver) handle(msg tea.Msg) {
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, cmd := range msg {
			d.run(cmd)
		}
	case tea.QuitMsg:
		d.quit = true
	default:
		next, cmd := d.m.Update(msg)
		d.m = next.(model)
		d.run(cmd)
	}
}

// pump handles messages for a while, for tests that show nothing changes.
func (d *uiDriver) pump(duration time.Duration) {
	stop := time.After(duration)
	for {
		select {
		case msg := <-d.msgs:
			d.handle(msg)
		case <-stop:
			return
		}
	}
}

func (d *uiDriver) waitView(want string) {
	d.t.Helper()
	d.waitUntil("view containing "+want, func(view string) bool { return strings.Contains(view, want) })
}

func (d *uiDriver) waitUntil(what string, ok func(view string) bool) {
	d.t.Helper()
	timeout := time.After(5 * time.Second)
	for !ok(d.m.View()) {
		select {
		case msg := <-d.msgs:
			d.handle(msg)
		case <-timeout:
			d.t.Fatalf("timed out waiting for %s; view:\n%s", what, d.m.View())
		}
	}
}

func (d *uiDriver) press(keys ...tea.KeyMsg) {
	for _, key := range keys {
		d.handle(key)
	}
}

func runeKey(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

var (
	enterKey = tea.KeyMsg{Type: tea.KeyEnter}
	escKey   = tea.KeyMsg{Type: tea.KeyEsc}
)

func (d *uiDriver) cursorTo(square string) {
	to := sq(d.t, square)
	for d.m.cursorRow < to.Row {
		d.press(tea.KeyMsg{Type: tea.KeyUp})
	}
	for d.m.cursorRow > to.Row {
		d.press(tea.KeyMsg{Type: tea.KeyDown})
	}
	for d.m.cursorCol < to.Col {
		d.press(tea.KeyMsg{Type: tea.KeyRight})
	}
	for d.m.cursorCol > to.Col {
		d.press(tea.KeyMsg{Type: tea.KeyLeft})
	}
}

func (d *uiDriver) selectSquare(square string) {
	d.cursorTo(square)
	d.press(enterKey)
}

// joinUIQueue takes a player from the menu into the queue and waits until the
// hub has them, so the first to join is reliably White.
func joinUIQueue(t *testing.T, app *App, player PlayerRef) *uiDriver {
	t.Helper()
	d := newUIDriver(t, app, player)
	d.waitView("My games")
	d.press(enterKey)
	d.waitView("Waiting")
	waitFor(t, player.Name+" in the queue or a game", func() bool {
		d.pump(time.Millisecond)
		return slices.Contains(hubQueue(app.Hub), d.m.session) || countEvents(t, app.DB) > 0
	})
	return d
}

func hubQueue(h *Hub) []*Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*Session(nil), h.queue...)
}

// startUIGame matches alice (White) with bob (Black) through the screens.
func startUIGame(t *testing.T, app *App) (white, black *uiDriver) {
	t.Helper()
	white = joinUIQueue(t, app, alice)
	black = joinUIQueue(t, app, bob)
	if err := app.Projector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	white.waitView(boardHeader)
	black.waitView(boardHeader)
	return white, black
}

// playOnStore plays moves for White and Black in turn, as commands.
func playOnStore(t *testing.T, app *App, gameID string, moves ...string) {
	t.Helper()
	for i, m := range moves {
		key := alice.Key
		if i%2 == 1 {
			key = bob.Key
		}
		if err := app.Commands.MakeMove(context.Background(), gameID, key, sq(t, m[:2]), sq(t, m[2:])); err != nil {
			t.Fatalf("move %s: %v", m, err)
		}
	}
}

var ansiCode = regexp.MustCompile("\x1b\\[[0-9;]*m")

// boardCells parses a view's board into squares and the raw escape codes that
// color them, so tests can check what's on a square and how it's highlighted.
func boardCells(t *testing.T, view string) map[string]string {
	t.Helper()
	cells := map[string]string{}
	for _, line := range strings.Split(view, "\n") {
		if len(line) == 0 || line[0] < '1' || line[0] > '8' {
			continue
		}
		rank := line[0]
		parts := strings.Split(line[1:], "\x1b[0m")
		if len(parts) < 8 {
			continue
		}
		for col := range 8 {
			cells[string(rune('a'+col))+string(rank)] = parts[col]
		}
	}
	if len(cells) != 64 {
		t.Fatalf("found %d squares in view:\n%s", len(cells), view)
	}
	return cells
}

func pieceOn(t *testing.T, view, square string) string {
	t.Helper()
	return strings.TrimSpace(ansiCode.ReplaceAllString(boardCells(t, view)[square], ""))
}

const greenBackground = "\x1b[42m"

func highlighted(t *testing.T, view string) map[string]bool {
	t.Helper()
	squares := map[string]bool{}
	for square, cell := range boardCells(t, view) {
		if strings.Contains(cell, greenBackground) {
			squares[square] = true
		}
	}
	return squares
}

func TestMenuShownWithoutActiveGame(t *testing.T) {
	d := newUIDriver(t, newUIApp(t, longTimeout), alice)
	d.waitView("My games")
	if view := d.m.View(); !strings.Contains(view, "Play") {
		t.Fatalf("no Play in menu:\n%s", view)
	}
}

func TestActiveGameSkipsMenu(t *testing.T) {
	app := newUIApp(t, longTimeout)
	if err := app.Commands.StartGame(context.Background(), "g1", alice, bob); err != nil {
		t.Fatal(err)
	}
	if err := app.Projector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := newUIDriver(t, app, alice)
	d.waitView(boardHeader)
	if view := d.m.View(); strings.Contains(view, "My games") {
		t.Fatalf("menu shown with an active game:\n%s", view)
	}
}

func TestSelectAndMoveSendsCommand(t *testing.T) {
	app := startedUIApp(t, longTimeout)
	white, black := startUIGame(t, app)

	white.selectSquare("e2")
	white.selectSquare("e4")
	black.waitView("Last move: e2 -> e4")
	white.waitView("Last move: e2 -> e4")

	events, err := app.Store.Load(context.Background(), white.m.gameID)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Type != EventMoveMade {
		t.Fatalf("last event is %s", last.Type)
	}
	if d := decode[MoveMadeData](t, last); d.From != "e2" || d.To != "e4" {
		t.Fatalf("got %+v", d)
	}
	if got := pieceOn(t, white.m.View(), "e4"); got != "♙" {
		t.Fatalf("e4 holds %q", got)
	}
}

func TestUIDoesNotMoveWhenNotYourTurn(t *testing.T) {
	app := startedUIApp(t, longTimeout)
	_, black := startUIGame(t, app)
	before := countEvents(t, app.DB)

	black.selectSquare("e7")
	black.selectSquare("e5")
	black.pump(100 * time.Millisecond)

	if got := countEvents(t, app.DB); got != before {
		t.Fatalf("events = %d, want %d", got, before)
	}
	if black.m.selected != nil {
		t.Fatalf("selected %v out of turn", black.m.selected)
	}
}

func TestMoveRejectionShownAsOneLine(t *testing.T) {
	app := newUIApp(t, longTimeout) // no projector: White's view stays one move behind
	white, _ := startUIGame(t, app)

	white.selectSquare("e2")
	white.selectSquare("e4")
	waitFor(t, "first move stored", func() bool {
		white.pump(time.Millisecond)
		return countEvents(t, app.DB) == 2
	})
	white.selectSquare("d2")
	white.selectSquare("d4")
	white.waitView("not your turn")

	white.selectSquare("a2")
	if view := white.m.View(); strings.Contains(view, "not your turn") {
		t.Fatalf("message outlived the next key:\n%s", view)
	}
}

func TestHighlightsComeFromProjection(t *testing.T) {
	app := startedUIApp(t, longTimeout)
	white, _ := startUIGame(t, app)

	white.selectSquare("e2")
	got := highlighted(t, white.m.View())
	if len(got) != 2 || !got["e3"] || !got["e4"] {
		t.Fatalf("highlighted %v, want e3 and e4", got)
	}

	white.press(escKey)
	if got := highlighted(t, white.m.View()); len(got) != 0 {
		t.Fatalf("highlighted %v after Esc", got)
	}
}

func TestCheckMessageShown(t *testing.T) {
	app := startedUIApp(t, longTimeout)
	white, black := startUIGame(t, app)
	playOnStore(t, app, white.m.gameID, "e2e4", "f7f6", "d1h5")
	white.waitView("in check")
	black.waitView("in check")
}

func TestOpponentCountdownShown(t *testing.T) {
	app := startedUIApp(t, longTimeout)
	white, black := startUIGame(t, app)

	app.Hub.Disconnect(context.Background(), black.m.session)
	white.waitView("disconnected")
	if !regexp.MustCompile(`\d+s`).MatchString(white.m.View()) {
		t.Fatalf("no seconds remaining:\n%s", white.m.View())
	}

	app.Hub.Connect(context.Background(), bob)
	white.waitUntil("the countdown to stop", func(view string) bool { return !strings.Contains(view, "disconnected") })
}

func TestGameOverShowsFinalBoardAndResult(t *testing.T) {
	app := startedUIApp(t, longTimeout)
	white, black := startUIGame(t, app)
	playOnStore(t, app, white.m.gameID, "f2f3", "e7e5", "g2g4", "d8h4")

	for _, d := range []*uiDriver{white, black} {
		d.waitView("Black wins")
		d.waitUntil("the final board", func(view string) bool { return strings.Contains(view, boardHeader) })
		view := d.m.View()
		for square, want := range map[string]string{"h4": "♛", "d8": "", "g4": "♙", "f3": "♙", "e5": "♟", "e1": "♔"} {
			if got := pieceOn(t, view, square); got != want {
				t.Fatalf("%s holds %q, want %q:\n%s", square, got, want, view)
			}
		}
	}

	white.press(runeKey("x"))
	white.waitView("My games")
	black.press(enterKey)
	black.waitView("My games")
}

func TestGameOverIgnoresLaterUpdates(t *testing.T) {
	app := startedUIApp(t, longTimeout)
	white, _ := startUIGame(t, app)
	playOnStore(t, app, white.m.gameID, "f2f3", "e7e5", "g2g4", "d8h4")
	white.waitView("Black wins")

	app.Hub.Broadcast()
	white.pump(50 * time.Millisecond)
	if view := white.m.View(); !strings.Contains(view, "Black wins") {
		t.Fatalf("left game over on an update:\n%s", view)
	}
}

func TestStalemateGameOverMessage(t *testing.T) {
	app := startedUIApp(t, longTimeout)
	white, black := startUIGame(t, app)
	playOnStore(t, app, white.m.gameID, "e2e3", "a7a5", "d1h5", "a8a6", "h5a5", "h7h5", "h2h4", "a6h6",
		"a5c7", "f7f6", "c7d7", "e8f7", "d7b7", "d8d3", "b7b8", "d3h7", "b8c8", "f7g6", "c8e6")
	white.waitView("Stalemate")
	black.waitView("Stalemate")
}

func TestResignationGameOverMessage(t *testing.T) {
	app := startedUIApp(t, shortTimeout)
	white, black := startUIGame(t, app)
	app.Hub.Disconnect(context.Background(), black.m.session)
	white.waitView("opponent left")
	if view := white.m.View(); !strings.Contains(view, "You win") {
		t.Fatalf("winner not told they won:\n%s", view)
	}
}

func TestLoadingWhenProjectionDropped(t *testing.T) {
	app := newUIApp(t, longTimeout)
	white, _ := startUIGame(t, app)
	gameID := white.m.gameID

	if _, err := app.DB.Exec(`DROP TABLE active_games`); err != nil {
		t.Fatal(err)
	}
	app.Hub.Broadcast()
	white.waitView("Loading")
	if view := white.m.View(); strings.Contains(view, boardHeader) || strings.Contains(view, "My games") {
		t.Fatalf("unexpected screen during rebuild:\n%s", view)
	}

	if err := app.Projector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	white.waitView(boardHeader)
	if white.m.gameID != gameID {
		t.Fatalf("game %q, want %q", white.m.gameID, gameID)
	}
}

func TestLoadingBeforeAnyGameNeverRoutesToMenu(t *testing.T) {
	app := newUIApp(t, longTimeout)
	if _, err := app.DB.Exec(`DROP TABLE active_games`); err != nil {
		t.Fatal(err)
	}

	d := newUIDriver(t, app, alice)
	d.waitView("Loading")
	d.pump(50 * time.Millisecond)
	if view := d.m.View(); !strings.Contains(view, "Loading") {
		t.Fatalf("left Loading during rebuild:\n%s", view)
	}

	if err := app.Projector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.waitView("My games")
}

func TestLoadingFromQueueReturnsToQueue(t *testing.T) {
	app := newUIApp(t, longTimeout)
	d := joinUIQueue(t, app, alice)

	if _, err := app.DB.Exec(`DROP TABLE active_games`); err != nil {
		t.Fatal(err)
	}
	app.Hub.Broadcast()
	d.waitView("Loading")

	if err := app.Projector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.waitView("Waiting")
}

func TestQueueEscReturnsToMenu(t *testing.T) {
	app := startedUIApp(t, longTimeout)
	a := joinUIQueue(t, app, alice)
	a.press(escKey)
	a.waitView("My games")

	b := joinUIQueue(t, app, bob)
	b.pump(100 * time.Millisecond)
	a.pump(100 * time.Millisecond)
	if got := countEvents(t, app.DB); got != 0 {
		t.Fatalf("events = %d: alice was matched after leaving", got)
	}
	if view := a.m.View(); !strings.Contains(view, "My games") {
		t.Fatalf("alice left the menu:\n%s", view)
	}
}

func TestQueueEscDuringJoinStillLeaves(t *testing.T) {
	app := newUIApp(t, longTimeout)
	a := newUIDriver(t, app, alice)
	a.waitView("My games")

	// The Esc lands before JoinQueue's result does, or even before it runs.
	a.press(enterKey)
	a.press(escKey)
	a.pump(100 * time.Millisecond)

	if got := hubQueue(app.Hub); len(got) != 0 {
		t.Fatalf("alice still queued after Esc")
	}
}

func TestQueueStaysOnWaitingAfterFailedStart(t *testing.T) {
	app := newUIApp(t, longTimeout)
	a := joinUIQueue(t, app, alice)

	// Break the write side, so bob's JoinQueue fails to start the game.
	if _, err := app.DB.Exec(`CREATE TRIGGER no_events BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'events are broken'); END`); err != nil {
		t.Fatal(err)
	}
	b := newUIDriver(t, app, bob)
	b.waitView("My games")
	b.press(enterKey)
	b.waitView("events are broken")
	if !strings.Contains(b.m.View(), "Waiting") {
		t.Fatalf("left the waiting screen:\n%s", b.m.View())
	}
	if got := hubQueue(app.Hub); len(got) != 2 {
		t.Fatalf("queue has %d players, want both back in line", len(got))
	}

	b.press(escKey)
	b.waitView("My games")
	if got := hubQueue(app.Hub); len(got) != 1 || got[0] != a.m.session {
		t.Fatalf("Esc should remove only bob from the queue")
	}
}

func TestJoinWhileInGameRoutesToGame(t *testing.T) {
	app := newUIApp(t, longTimeout)
	d := newUIDriver(t, app, alice)
	d.waitView("My games")

	if err := app.Commands.StartGame(context.Background(), "g1", alice, bob); err != nil {
		t.Fatal(err)
	}
	d.press(enterKey) // JoinQueue catches up and answers ErrAlreadyInGame
	d.waitView(boardHeader)
}

func TestMenuKeys(t *testing.T) {
	d := newUIDriver(t, newUIApp(t, longTimeout), alice)
	d.waitView("My games")
	d.press(runeKey("j"))
	if !strings.Contains(d.m.View(), "> My games") {
		t.Fatalf("j did not move down:\n%s", d.m.View())
	}
	d.press(runeKey("k"))
	if !strings.Contains(d.m.View(), "> Play") {
		t.Fatalf("k did not move up:\n%s", d.m.View())
	}
	d.press(runeKey("q"))
	d.pump(10 * time.Millisecond)
	if !d.quit {
		t.Fatal("q did not quit")
	}
}

func TestReplacedSessionQuits(t *testing.T) {
	app := newUIApp(t, longTimeout)
	d := newUIDriver(t, app, alice)
	d.waitView("My games")
	app.Hub.Connect(context.Background(), alice)
	waitFor(t, "quit", func() bool {
		d.pump(time.Millisecond)
		return d.quit
	})
}

// finishGame plays fool's mate for a game: bob (Black) beats alice in 4 moves.
func finishGame(t *testing.T, app *App, gameID string) {
	t.Helper()
	if err := app.Commands.StartGame(context.Background(), gameID, alice, bob); err != nil {
		t.Fatal(err)
	}
	playOnStore(t, app, gameID, "f2f3", "e7e5", "g2g4", "d8h4")
}

// bobWithFinishedGame is bob on the menu, after beating alice.
func bobWithFinishedGame(t *testing.T) (*App, *uiDriver) {
	t.Helper()
	app := newUIApp(t, longTimeout)
	finishGame(t, app, "g1")
	if err := app.Projector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := newUIDriver(t, app, bob)
	d.waitView("My games")
	return app, d
}

func openMyGames(d *uiDriver) {
	d.t.Helper()
	d.press(runeKey("j"), enterKey)
	d.waitView("Opponent")
}

var rightKey, leftKey = tea.KeyMsg{Type: tea.KeyRight}, tea.KeyMsg{Type: tea.KeyLeft}

func TestMyGamesListShowsGame(t *testing.T) {
	_, d := bobWithFinishedGame(t)
	openMyGames(d)
	view := d.m.View()
	for _, want := range []string{"alice", "Black", "Won", "Date", "Moves"} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in:\n%s", want, view)
		}
	}
	if !regexp.MustCompile(`Won\s+4\n`).MatchString(view) {
		t.Fatalf("no move count 4 after Won:\n%s", view)
	}
	if !strings.Contains(view, d.m.myGames[0].EndedAt.Local().Format("Jan 2")) {
		t.Fatalf("no date in:\n%s", view)
	}
}

func TestMyGamesEmptyState(t *testing.T) {
	d := newUIDriver(t, newUIApp(t, longTimeout), alice)
	d.waitView("My games")
	d.press(runeKey("j"), enterKey)
	d.waitView("No games yet")
	d.press(enterKey, rightKey, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyUp}) // nothing to open
	if !strings.Contains(d.m.View(), "No games yet") {
		t.Fatalf("left the empty list:\n%s", d.m.View())
	}
}

func TestMyGamesEscReturnsToMenu(t *testing.T) {
	_, d := bobWithFinishedGame(t)
	openMyGames(d)
	d.press(escKey)
	d.waitView("> My games")
	if strings.Contains(d.m.View(), "Opponent") {
		t.Fatalf("still on the list:\n%s", d.m.View())
	}
}

func TestMyGamesQuitKeys(t *testing.T) {
	for _, key := range []tea.KeyMsg{runeKey("q"), {Type: tea.KeyCtrlC}} {
		_, d := bobWithFinishedGame(t)
		openMyGames(d)
		d.press(key)
		d.pump(10 * time.Millisecond)
		if !d.quit {
			t.Fatalf("%v did not quit the list", key)
		}
		d.quit = false
		d.press(enterKey)
		d.waitView("Move 0 of 4")
		d.press(key)
		d.pump(10 * time.Millisecond)
		if !d.quit {
			t.Fatalf("%v did not quit the replay", key)
		}
	}
}

func TestMyGamesSelectionStaysInRange(t *testing.T) {
	_, d := bobWithFinishedGame(t)
	openMyGames(d)
	up, down := tea.KeyMsg{Type: tea.KeyUp}, tea.KeyMsg{Type: tea.KeyDown}
	d.press(up, up, down, down, down)
	if d.m.gamesIndex != 0 {
		t.Fatalf("index = %d, want 0 with one game", d.m.gamesIndex)
	}
	if !strings.Contains(d.m.View(), "alice") {
		t.Fatalf("list lost its row:\n%s", d.m.View())
	}
}

func TestMyGamesManyGamesScroll(t *testing.T) {
	app := newUIApp(t, longTimeout)
	for i := range 50 {
		finishGame(t, app, fmt.Sprintf("g%02d", i))
	}
	if err := app.Projector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	d := newUIDriver(t, app, bob)
	d.waitView("My games")
	d.handle(tea.WindowSizeMsg{Width: 80, Height: 24})
	openMyGames(d)

	check := func(index int) {
		t.Helper()
		view := d.m.View()
		if lines := strings.Count(view, "\n"); lines > 24 {
			t.Fatalf("view has %d lines:\n%s", lines, view)
		}
		selected := 0
		for _, line := range strings.Split(view, "\n") {
			if strings.HasPrefix(line, "> ") {
				selected++
			}
		}
		if selected != 1 || d.m.gamesIndex != index {
			t.Fatalf("selected rows = %d, index = %d, want %d:\n%s", selected, d.m.gamesIndex, index, view)
		}
	}
	check(0)
	for range 49 {
		d.press(tea.KeyMsg{Type: tea.KeyDown})
	}
	check(49)
	for range 30 {
		d.press(tea.KeyMsg{Type: tea.KeyUp})
	}
	check(19)
}

func TestMyGamesRefreshesAfterProjectionDrop(t *testing.T) {
	app, d := bobWithFinishedGame(t)
	openMyGames(d)
	before := d.m.View()

	if _, err := app.DB.Exec(`DROP TABLE my_games`); err != nil {
		t.Fatal(err)
	}
	app.Hub.Broadcast()
	d.waitView("Loading")
	d.press(enterKey, tea.KeyMsg{Type: tea.KeyDown}) // safe with no rows

	if err := app.Projector.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.waitView("Opponent")
	if d.m.View() != before {
		t.Fatalf("list changed:\n%s\nwas:\n%s", d.m.View(), before)
	}
}

func openReplay(t *testing.T) (*App, *uiDriver) {
	app, d := bobWithFinishedGame(t)
	openMyGames(d)
	d.press(enterKey)
	d.waitView("Move 0 of 4")
	return app, d
}

func TestReplayStartsAtStartingPosition(t *testing.T) {
	_, d := openReplay(t)
	view := d.m.View()
	if pieceOn(t, view, "f2") != "♙" || pieceOn(t, view, "d8") != "♛" {
		t.Fatalf("not the starting position:\n%s", view)
	}
	for _, want := range []string{"you vs alice", "You won", "left/right", "esc"} {
		if !strings.Contains(view, want) {
			t.Fatalf("no %q in:\n%s", want, view)
		}
	}
}

func TestReplayStepsForwardAndBack(t *testing.T) {
	_, d := openReplay(t)
	d.press(rightKey)
	view := d.m.View()
	if !strings.Contains(view, "Move 1 of 4") || pieceOn(t, view, "f3") != "♙" || pieceOn(t, view, "f2") != "" {
		t.Fatalf("after right:\n%s", view)
	}
	if !strings.Contains(ansiCode.ReplaceAllString(view, ""), "1. f2-f3") {
		t.Fatalf("no move list:\n%s", view)
	}
	d.press(leftKey)
	view = d.m.View()
	if !strings.Contains(view, "Move 0 of 4") || pieceOn(t, view, "f2") != "♙" {
		t.Fatalf("after left:\n%s", view)
	}
}

func TestReplayStopsAtEnds(t *testing.T) {
	_, d := openReplay(t)
	d.press(leftKey, leftKey)
	d.waitView("Move 0 of 4")
	d.press(rightKey, rightKey, rightKey, rightKey, rightKey, rightKey)
	view := d.m.View()
	if !strings.Contains(view, "Move 4 of 4") || pieceOn(t, view, "h4") != "♛" {
		t.Fatalf("at the end:\n%s", view)
	}
	if !strings.Contains(ansiCode.ReplaceAllString(view, ""), "d8-h4") {
		t.Fatalf("no d8-h4 in:\n%s", view)
	}
}

func TestReplayEscReturnsToList(t *testing.T) {
	_, d := openReplay(t)
	d.press(escKey)
	d.waitView("Opponent")
	if d.m.gamesIndex != 0 || !strings.Contains(d.m.View(), "> ") {
		t.Fatalf("no selected row:\n%s", d.m.View())
	}
}

func TestReplayUnaffectedByProjectionDrop(t *testing.T) {
	app, d := openReplay(t)
	d.press(rightKey)
	for _, table := range []string{"my_games", "active_games"} {
		if _, err := app.DB.Exec(`DROP TABLE ` + table); err != nil {
			t.Fatal(err)
		}
	}
	app.Hub.Broadcast()
	d.pump(100 * time.Millisecond)
	d.press(rightKey)
	view := d.m.View()
	if !strings.Contains(view, "Move 2 of 4") || strings.Contains(view, "Loading") {
		t.Fatalf("replay disturbed:\n%s", view)
	}
}

func TestReplayMoveListStaysInWindow(t *testing.T) {
	var shuffle []string
	for range 20 {
		shuffle = append(shuffle, "g1f3", "g8f6", "f3g1", "f6g8")
	}
	r, err := BuildReplay(gameEvents(t, "g1", alice, bob, shuffle...))
	if err != nil {
		t.Fatal(err)
	}
	m := model{screen: screenReplay, replay: &r, replayGame: MyGame{OpponentName: "bob", Result: "won"}, step: 41, height: 24}
	view := m.View()
	if lines := strings.Count(view, "\n"); lines > 24 {
		t.Fatalf("view has %d lines:\n%s", lines, view)
	}
	plain := ansiCode.ReplaceAllString(view, "")
	if !strings.Contains(plain, "21. ") || strings.Contains(plain, "\n1. ") || !strings.Contains(plain, "Move 41 of 80") {
		t.Fatalf("window not around move 41:\n%s", plain)
	}
}

func TestVanishedRowStaysOnBoardAndAcceptsMove(t *testing.T) {
	app := startedUIApp(t, longTimeout)
	white, black := startUIGame(t, app)
	gameID := white.m.gameID

	if _, err := app.DB.Exec(`DELETE FROM active_games`); err != nil {
		t.Fatal(err)
	}
	app.Hub.Broadcast()
	white.waitView("Game over") // the read found no row, so it asks the events
	white.waitUntil("the events' answer", func(string) bool {
		return white.m.result != nil || white.m.screen == screenPlaying
	})

	view := white.m.View()
	for _, invented := range []string{"Checkmate", "wins", "Game over"} {
		if strings.Contains(view, invented) {
			t.Fatalf("view says %q about an unfinished game:\n%s", invented, view)
		}
	}
	if !strings.Contains(view, "Your turn") || !strings.Contains(view, boardHeader) || white.m.gameID != gameID {
		t.Fatalf("not back on the board (game %q):\n%s", white.m.gameID, view)
	}

	white.selectSquare("e2")
	white.selectSquare("e4")
	white.waitView("Last move: e2 -> e4")
	black.waitView("Last move: e2 -> e4")
	if white.m.message != "" {
		t.Fatalf("move rejected: %s", white.m.message)
	}
}

func TestResultLineNeverInventsAResult(t *testing.T) {
	got := resultLine(Replay{White: alice, Black: bob}, alice.Key)
	if strings.Contains(got, "Checkmate") || strings.Contains(got, "wins") {
		t.Fatalf("resultLine with no outcome = %q", got)
	}
}

func TestFailedJoinReturnsToMenu(t *testing.T) {
	app := newUIApp(t, longTimeout)
	d := newUIDriver(t, app, alice)
	d.waitView("My games")

	catchUp := app.Hub.catchUp
	app.Hub.catchUp = func(context.Context) error { return errors.New("database is busy") }
	d.press(enterKey)
	d.waitView("database is busy")
	if view := d.m.View(); !strings.Contains(view, "My games") || strings.Contains(view, "Waiting") {
		t.Fatalf("not back at the menu:\n%s", view)
	}
	if d.m.queued {
		t.Fatal("model still thinks it is in line")
	}
	if got := hubQueue(app.Hub); len(got) != 0 {
		t.Fatalf("queue has %d players, want 0", len(got))
	}

	app.Hub.catchUp = catchUp
	d.press(enterKey)
	d.waitView("Waiting")
	waitFor(t, "alice in the queue", func() bool {
		d.pump(time.Millisecond)
		return slices.Contains(hubQueue(app.Hub), d.m.session)
	})
}
