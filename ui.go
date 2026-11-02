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
	screenMyGames
	screenReplay
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

	height     int // terminal rows, from tea.WindowSizeMsg; 0 until known
	myGames    []MyGame
	gamesReady bool // false while the my_games projection is rebuilding
	gamesIndex int
	replay     *Replay // nil while its events load
	replayGame MyGame
	step       int // how many moves of the replay are on the board
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
	myGamesMsg struct {
		read  int
		games []MyGame
		err   error
	}
	replayMsg struct {
		gameID string
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

	case tea.WindowSizeMsg:
		m.height = msg.Height
		return m, nil

	case updateMsg:
		if m.screen == screenGameOver || m.screen == screenReplay { // replay reads events, not projections
			return m, m.waitForUpdate()
		}
		if m.screen == screenMyGames {
			m.reads++
			return m, tea.Batch(m.myGamesCmd(), m.waitForUpdate())
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

	case myGamesMsg:
		if msg.read != m.reads || m.screen != screenMyGames {
			return m, nil
		}
		return m.showMyGames(msg), nil

	case replayMsg:
		if m.screen != screenReplay || m.replay != nil || msg.gameID != m.replayGame.GameID {
			return m, nil
		}
		if msg.err != nil {
			m.message = msg.err.Error()
			return m, nil
		}
		m.replay, m.step = &msg.replay, 0
		return m, nil

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
	case screenMyGames:
		return m.handleMyGamesKey(key)
	case screenReplay:
		return m.handleReplayKey(key)
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
		m.screen, m.gamesReady = screenMyGames, false
		m.reads++ // an older read must not route us back to the menu
		return m, m.myGamesCmd()
	}
	return m, nil
}

func (m model) handleMyGamesKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "up", "k":
		m.gamesIndex = max(m.gamesIndex-1, 0)
	case "down", "j":
		m.gamesIndex = max(min(m.gamesIndex+1, len(m.myGames)-1), 0)
	case "esc":
		m.screen = screenMenu
		return m.read() // a game may have started meanwhile
	case "enter":
		if m.gamesReady && m.gamesIndex < len(m.myGames) {
			m.replayGame, m.replay, m.step = m.myGames[m.gamesIndex], nil, 0
			m.screen = screenReplay
			return m, m.loadReplay(m.replayGame.GameID)
		}
	}
	return m, nil
}

func (m model) handleReplayKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "left", "h":
		m.step = max(m.step-1, 0)
	case "right", "l":
		if m.replay != nil {
			m.step = min(m.step+1, len(m.replay.Moves))
		}
	case "esc":
		m.screen, m.replay = screenMyGames, nil
		m.reads++
		return m, m.myGamesCmd()
	}
	return m, nil
}

func (m model) myGamesCmd() tea.Cmd {
	read := m.reads
	return func() tea.Msg {
		games, err := ListMyGames(m.ctx, m.app.DB, m.session.Player.Key)
		return myGamesMsg{read, games, err}
	}
}

// showMyGames keeps the same game selected when the list changes under it.
func (m model) showMyGames(msg myGamesMsg) model {
	switch {
	case errors.Is(msg.err, ErrProjectionUnavailable):
		m.gamesReady = false // shows Loading; the update after the rebuild brings the list back
	case msg.err != nil:
		m.message = msg.err.Error()
	default:
		selected := ""
		if m.gamesIndex < len(m.myGames) {
			selected = m.myGames[m.gamesIndex].GameID
		}
		m.myGames, m.gamesReady = msg.games, true
		if i := slices.IndexFunc(m.myGames, func(g MyGame) bool { return g.GameID == selected }); i >= 0 {
			m.gamesIndex = i
		}
		m.gamesIndex = max(min(m.gamesIndex, len(m.myGames)-1), 0)
	}
	return m
}

// loadReplay reads the game's events, not a projection, so a replay keeps
// working while projections rebuild.
func (m model) loadReplay(gameID string) tea.Cmd {
	return func() tea.Msg {
		events, err := m.app.Store.Load(m.ctx, gameID)
		if err != nil {
			return replayMsg{gameID: gameID, err: err}
		}
		replay, err := BuildReplay(events)
		return replayMsg{gameID, replay, err}
	}
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
	case screenMyGames:
		m.viewMyGames(&s)
	case screenReplay:
		m.viewReplay(&s)
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

func (m model) viewMyGames(s *strings.Builder) {
	switch {
	case !m.gamesReady:
		s.WriteString("Loading...\n")
		return
	case len(m.myGames) == 0:
		s.WriteString("No games yet. Esc goes back, q quits\n")
		return
	}
	fmt.Fprintf(s, "  %-6s  %-8s  %-5s  %-8s%s\n", "Date", "Opponent", "You", "Result", "Moves")
	// The title takes 2 lines, the header 1, the help 2, and a message 2.
	rows := max(m.windowHeight()-7, 1)
	start := max(min(m.gamesIndex-rows/2, len(m.myGames)-rows), 0)
	for i := start; i < min(start+rows, len(m.myGames)); i++ {
		g := m.myGames[i]
		pointer := "  "
		if i == m.gamesIndex {
			pointer = "> "
		}
		fmt.Fprintf(s, "%s%-6s  %-8.8s  %-5s  %-10s%3d\n",
			pointer, g.EndedAt.Local().Format("Jan 2"), g.OpponentName, capitalize(g.Color), g.ResultLabel(), g.MoveCount)
	}
	s.WriteString("\nUp and down choose, Enter replays, Esc goes back, q quits\n")
}

func capitalize(s string) string { return strings.ToUpper(s[:1]) + s[1:] }

func (m model) windowHeight() int {
	if m.height == 0 {
		return 24
	}
	return m.height
}

func (m model) viewReplay(s *strings.Builder) {
	if m.replay == nil {
		s.WriteString("Loading...\n")
		return
	}
	boardLines := getBoardLines(m.replay.Positions[m.step], boardMarks{})
	panel := m.replayPanel()
	for i := range max(len(boardLines), len(panel)) {
		if i < len(boardLines) {
			s.WriteString(boardLines[i] + "   ")
		} else {
			s.WriteString(strings.Repeat(" ", 29)) // the board's width plus the gap
		}
		if i < len(panel) {
			s.WriteString(panel[i])
		}
		s.WriteString("\n")
	}
}

// replayPanel is the text beside the board: who played, the moves around the
// current one, and the keys.
func (m model) replayPanel() []string {
	g := m.replayGame
	outcome := map[string]string{"won": "You won", "lost": "You lost", "draw": "Draw"}[g.Result]
	lines := []string{
		"you vs " + g.OpponentName,
		g.EndedAt.Local().Format("Jan 2") + " - " + outcome,
		"",
	}

	// Moves are listed in pairs, a window of them so long games still fit.
	moves := m.replay.Moves
	pairs := (len(moves) + 1) / 2
	rows := max(min(6, m.windowHeight()-11), 1)
	current := max(m.step-1, 0) / 2
	start := max(min(current-rows/2, pairs-rows), 0)
	for p := start; p < min(start+rows, pairs); p++ {
		line := fmt.Sprintf("%d. %s", p+1, m.moveText(2*p))
		if 2*p+1 < len(moves) {
			line += "  " + m.moveText(2*p+1)
		}
		lines = append(lines, line)
	}
	return append(lines, "", fmt.Sprintf("Move %d of %d", m.step, len(moves)), "",
		"left/right  step", "esc         back")
}

// moveText is one move, highlighted if it is the one just played.
func (m model) moveText(i int) string {
	if i == m.step-1 {
		return "\033[7m" + m.replay.Moves[i] + "\033[0m"
	}
	return m.replay.Moves[i]
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
