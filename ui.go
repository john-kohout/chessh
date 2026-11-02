package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type screen int

const (
	screenLoading screen = iota
	screenMenu
	screenQueue
	screenPlaying
	screenGameOver
)

var menuItems = []string{"Play", "My games"}

// model is one player's terminal. It sends commands through the Hub and reads
// through the query functions. Everything it shows comes from those reads,
// apart from the cursor and selection, which stay local.
type model struct {
	ctx     context.Context
	app     *App
	session *Session

	screen  screen
	message string // one line about what just went wrong; the next key clears it
	reads   int    // reads started; only the newest one's answer counts

	menuIndex int
	queued    bool // in the Hub's line, even while screenLoading hides the queue

	gameID    string // the game being played; survives screenLoading
	game      ActiveGame
	deadline  time.Time // when a disconnected opponent loses; zero if they're here
	ticking   bool
	cursorRow int
	cursorCol int
	selected  *Position

	result *Replay // what screenGameOver shows
}

func newModel(ctx context.Context, app *App, s *Session) model {
	return model{ctx: ctx, app: app, session: s, screen: screenLoading}
}

// Messages from commands back to Update.
type (
	updateMsg   struct{} // the Hub says to re-read
	replacedMsg struct{} // another connection took over this key
	tickMsg     struct{} // a second passed on the countdown
	joinedMsg   struct{ err error }
	movedMsg    struct{ err error }
	gameOverMsg struct {
		replay Replay
		err    error
	}
	readMsg struct {
		read     int
		game     ActiveGame
		found    bool
		err      error
		deadline time.Time
	}
)

func (m model) Init() tea.Cmd {
	return tea.Batch(m.readCmd(), m.waitForUpdate())
}

// waitForUpdate waits for one signal. Update starts it again after each, so
// exactly one is ever waiting, and it stops when the session ends.
func (m model) waitForUpdate() tea.Cmd {
	return func() tea.Msg {
		select {
		case <-m.session.Updates:
			return updateMsg{}
		case <-m.session.Closed:
			return replacedMsg{}
		case <-m.ctx.Done():
			return nil
		}
	}
}

// read asks the projection about the current game or, before there is one,
// about this player. Reads overlap, so each is numbered and only the newest
// answer is used: an older one could bring back a game that has since ended.
func (m model) read() (model, tea.Cmd) {
	m.reads++
	return m, m.readCmd()
}

func (m model) readCmd() tea.Cmd {
	read, gameID := m.reads, m.gameID
	return func() tea.Msg {
		msg := readMsg{read: read}
		if gameID != "" {
			msg.game, msg.found, msg.err = GetActiveGame(m.ctx, m.app.DB, gameID)
		} else {
			msg.game, msg.found, msg.err = GetActiveGameForPlayer(m.ctx, m.app.DB, m.session.Player.Key)
		}
		if msg.found {
			msg.deadline, _ = m.app.Hub.OpponentDeadline(msg.game.GameID, m.opponentOf(msg.game).Key)
		}
		return msg
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKey(msg)

	case updateMsg:
		if m.screen == screenGameOver {
			return m, m.waitForUpdate()
		}
		m, read := m.read()
		return m, tea.Batch(read, m.waitForUpdate())

	case replacedMsg:
		return m, tea.Quit

	case readMsg:
		if msg.read != m.reads {
			return m, nil
		}
		return m.route(msg)

	case joinedMsg:
		return m.handleJoined(msg)

	case movedMsg:
		if msg.err != nil {
			m.message = msg.err.Error()
		}
		return m, nil

	case gameOverMsg:
		if msg.err != nil {
			m.message = msg.err.Error()
			return m, nil
		}
		m.result = &msg.replay
		return m, nil

	case tickMsg:
		if m.screen != screenPlaying || m.deadline.IsZero() {
			m.ticking = false
			return m, nil
		}
		return m, tick()
	}
	return m, nil
}

// route picks the screen from what a read found.
func (m model) route(msg readMsg) (tea.Model, tea.Cmd) {
	switch {
	case errors.Is(msg.err, ErrProjectionUnavailable):
		m.screen = screenLoading // the next update, after the rebuild, moves us on

	case msg.err != nil:
		m.message = msg.err.Error()

	case msg.found:
		m.screen, m.queued = screenPlaying, false
		m.gameID, m.game, m.deadline = msg.game.GameID, msg.game, msg.deadline
		if !m.deadline.IsZero() && !m.ticking {
			m.ticking = true
			return m, tick()
		}

	case m.gameID != "": // the row is gone: GameEnded removed it
		gameID := m.gameID
		m.screen, m.gameID, m.selected, m.result = screenGameOver, "", nil, nil
		return m, m.loadResult(gameID)

	case m.queued:
		m.screen = screenQueue

	default:
		m.screen = screenMenu
	}
	return m, nil
}

func (m model) handleJoined(msg joinedMsg) (tea.Model, tea.Cmd) {
	if errors.Is(msg.err, ErrAlreadyInGame) {
		m.queued = false
		return m.read()
	}
	if !m.queued { // Esc came while JoinQueue was still running
		m.app.Hub.LeaveQueue(m.session)
		return m, nil
	}
	if msg.err != nil {
		m.message = msg.err.Error()
	}
	return m, nil
}

func (m model) handleKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if key.Type == tea.KeyCtrlC {
		return m, tea.Quit
	}
	if m.screen == screenGameOver { // any other key, even q
		m.screen, m.result, m.message = screenMenu, nil, ""
		return m, nil
	}
	if key.String() == "q" {
		return m, tea.Quit
	}
	m.message = ""

	switch m.screen {
	case screenMenu:
		return m.handleMenuKey(key)
	case screenQueue:
		if key.Type == tea.KeyEsc {
			m.app.Hub.LeaveQueue(m.session)
			m.screen, m.queued = screenMenu, false
		}
	case screenPlaying:
		return m.handleBoardKey(key)
	}
	return m, nil
}

func (m model) handleMenuKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "up", "k":
		m.menuIndex = max(m.menuIndex-1, 0)
	case "down", "j":
		m.menuIndex = min(m.menuIndex+1, len(menuItems)-1)
	case "enter":
		if menuItems[m.menuIndex] == "Play" {
			m.screen, m.queued = screenQueue, true
			return m, m.joinQueue()
		}
	}
	return m, nil
}

func (m model) handleBoardKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "up", "k":
		m.cursorRow = min(m.cursorRow+1, 7)
	case "down", "j":
		m.cursorRow = max(m.cursorRow-1, 0)
	case "left", "h":
		m.cursorCol = max(m.cursorCol-1, 0)
	case "right", "l":
		m.cursorCol = min(m.cursorCol+1, 7)
	case "esc":
		m.selected = nil
	case "enter", " ":
		cursor := Position{m.cursorRow, m.cursorCol}
		switch {
		case m.selected == nil:
			piece := m.game.Game.Board.At(cursor)
			if m.myTurn() && piece.Type != Empty && piece.Color == m.myColor() {
				m.selected = &cursor
			}
		case *m.selected == cursor:
			m.selected = nil
		default:
			from := *m.selected
			m.selected = nil
			return m, m.makeMove(from, cursor)
		}
	}
	return m, nil
}

func (m model) joinQueue() tea.Cmd {
	return func() tea.Msg {
		return joinedMsg{m.app.Hub.JoinQueue(m.ctx, m.session)}
	}
}

func (m model) makeMove(from, to Position) tea.Cmd {
	gameID := m.gameID
	return func() tea.Msg {
		return movedMsg{m.app.Hub.MakeMove(m.ctx, m.session, gameID, from, to)}
	}
}

// loadResult reads the game's events, as replay does, because GameEnded has
// removed its row from active_games.
func (m model) loadResult(gameID string) tea.Cmd {
	return func() tea.Msg {
		events, err := m.app.Store.Load(m.ctx, gameID)
		if err != nil {
			return gameOverMsg{err: err}
		}
		replay, err := BuildReplay(events)
		return gameOverMsg{replay, err}
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m model) myColor() Color {
	if m.game.White.Key == m.session.Player.Key {
		return White
	}
	return Black
}

func (m model) myTurn() bool { return m.game.Game.CurrentTurn == m.myColor() }

func (m model) opponentOf(game ActiveGame) PlayerRef {
	if game.White.Key == m.session.Player.Key {
		return game.Black
	}
	return game.White
}

func (m model) View() string {
	var s strings.Builder
	s.WriteString("CheSSH\n\n")
	switch m.screen {
	case screenLoading:
		s.WriteString("Loading...\n")
	case screenMenu:
		m.viewMenu(&s)
	case screenQueue:
		s.WriteString("Waiting for an opponent...\n\nEsc goes back, q quits\n")
	case screenPlaying:
		m.viewPlaying(&s)
	case screenGameOver:
		m.viewGameOver(&s)
	}
	if m.message != "" {
		s.WriteString("\n" + m.message + "\n")
	}
	return s.String()
}

func (m model) viewMenu(s *strings.Builder) {
	for i, item := range menuItems {
		pointer := "  "
		if i == m.menuIndex {
			pointer = "> "
		}
		s.WriteString(pointer + item + "\n")
	}
	s.WriteString("\nUp and down choose, Enter selects, q quits\n")
}

func (m model) viewPlaying(s *strings.Builder) {
	opponent := m.opponentOf(m.game)
	fmt.Fprintf(s, "You: %s (%s) vs %s (%s)\n", m.session.Player.Name, m.myColor(), opponent.Name, opponentColor(m.myColor()))
	if m.myTurn() {
		s.WriteString("Your turn. Arrows move, Enter selects, Esc deselects, q quits\n")
	} else {
		s.WriteString("Opponent's turn. q quits\n")
	}
	if m.game.InCheck {
		fmt.Fprintf(s, "%s is in check!\n", m.game.Game.CurrentTurn)
	}
	if !m.deadline.IsZero() {
		seconds := max(0, int(math.Ceil(time.Until(m.deadline).Seconds())))
		fmt.Fprintf(s, "Your opponent disconnected. They lose in %ds.\n", seconds)
	}
	s.WriteString("\n")

	cursor := Position{m.cursorRow, m.cursorCol}
	marks := boardMarks{cursor: &cursor, selected: m.selected}
	if m.selected != nil {
		for _, move := range m.game.LegalMoves {
			if move.From == *m.selected {
				marks.targets = append(marks.targets, move.To)
			}
		}
	}
	boardLines := getBoardLines(m.game.Game.Board, marks)
	infoLines := getInfoLines(m.game.Game, cursor)
	for i, line := range boardLines {
		s.WriteString(line + "   ")
		if i < len(infoLines) {
			s.WriteString(infoLines[i])
		}
		s.WriteString("\n")
	}
}

func (m model) viewGameOver(s *strings.Builder) {
	s.WriteString("Game over\n")
	if m.result == nil {
		return // still reading the events, or the message says why not
	}
	s.WriteString(resultLine(*m.result, m.session.Player.Key) + "\n\n")
	final := m.result.Positions[len(m.result.Positions)-1]
	for _, line := range getBoardLines(final, boardMarks{}) {
		s.WriteString(line + "\n")
	}
	s.WriteString("\nPress any key for the menu\n")
}

func resultLine(r Replay, playerKey string) string {
	winner := "White"
	winnerKey := r.White.Key
	if r.Winner == Black.Name() {
		winner, winnerKey = "Black", r.Black.Key
	}
	switch r.Outcome {
	case OutcomeStalemate:
		return "Stalemate. The game is a draw."
	case OutcomeResignation:
		if winnerKey == playerKey {
			return "Your opponent left the game. You win."
		}
		return fmt.Sprintf("You left the game. %s wins.", winner)
	}
	return fmt.Sprintf("Checkmate. %s wins.", winner)
}

func opponentColor(c Color) Color {
	if c == White {
		return Black
	}
	return White
}

// boardMarks are the squares to color on a board. All of them are optional.
type boardMarks struct {
	cursor   *Position
	selected *Position
	targets  []Position
}

func getBoardLines(board Board, marks boardMarks) []string {
	var lines []string

	lines = append(lines, "  a  b  c  d  e  f  g  h  ")

	for row := 7; row >= 0; row-- {
		var line strings.Builder
		line.WriteString(fmt.Sprintf("%d", row+1))

		for col := range 8 {
			pos := Position{row, col}
			piece := board.At(pos)

			cellChar := piece.String()
			if piece.Type == Empty {
				cellChar = " "
			}

			// Determine background color
			var bgColor string
			if marks.cursor != nil && *marks.cursor == pos {
				bgColor = "\033[41m" // Red background for cursor
			} else if marks.selected != nil && *marks.selected == pos {
				bgColor = "\033[43m" // Yellow background for selected
			} else if slices.Contains(marks.targets, pos) {
				bgColor = "\033[42m" // Green background for valid moves
			} else if (row+col)%2 == 0 {
				bgColor = "\033[100m" // Light grey background for light squares
			} else {
				bgColor = "\033[40m" // Black background for dark squares
			}

			line.WriteString(fmt.Sprintf("%s %s \033[0m", bgColor, cellChar))
		}

		line.WriteString(fmt.Sprintf("%d", row+1))
		lines = append(lines, line.String())
	}

	lines = append(lines, "  a  b  c  d  e  f  g  h  ")

	return lines
}

func getInfoLines(game *Game, cursor Position) []string {
	var lines []string

	lines = append(lines, "┌─────────────────────┐")
	lines = append(lines, "│ GAME INFO           │")
	lines = append(lines, "├─────────────────────┤")
	lines = append(lines, fmt.Sprintf("│ Turn: %-13s │", game.CurrentTurn))
	lines = append(lines, "│                     │")

	piece := game.Board.At(cursor)
	lines = append(lines, fmt.Sprintf("│ Cursor: %-11s │", cursor.String()))

	if piece.Type == Empty {
		lines = append(lines, "│ Piece: Empty        │")
	} else {
		lines = append(lines, fmt.Sprintf("│ Piece: %-12s │", getPieceName(piece)))
	}

	lines = append(lines, "│                     │")

	lines = append(lines, "└─────────────────────┘")

	if len(game.MoveHistory) > 0 {
		lastMove := game.MoveHistory[len(game.MoveHistory)-1]
		lines = append(lines, fmt.Sprintf("Last move: %s -> %-12s", lastMove.From.String(), lastMove.To.String()))
	}

	return lines
}

func getPieceName(piece Piece) string {
	if piece.Type == Empty {
		return "Empty"
	}

	color := "White"
	if piece.Color == Black {
		color = "Black"
	}

	pieceType := ""
	switch piece.Type {
	case Pawn:
		pieceType = "Pawn"
	case Rook:
		pieceType = "Rook"
	case Knight:
		pieceType = "Knight"
	case Bishop:
		pieceType = "Bishop"
	case Queen:
		pieceType = "Queen"
	case King:
		pieceType = "King"
	}

	return fmt.Sprintf("%s %s", color, pieceType)
}
