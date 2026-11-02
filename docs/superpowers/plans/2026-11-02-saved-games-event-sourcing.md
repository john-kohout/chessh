# Saved Games with Event Sourcing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Players identified by SSH key can resume active games and list and replay finished ones, with all game state derived from an append-only SQLite event log that serves as a talk's worked example.

**Architecture:** Commands go through a `GameAggregate`, which rebuilds itself from events, applies the chess rules, and appends new events to the store. A background projector feeds events to the `active_games` and `my_games` projections, then tells every session to re-read its current screen. Screens only read projections or event streams.

**Tech Stack:** Go 1.25, Bubble Tea, Wish, `modernc.org/sqlite`, `database/sql`, `golang.org/x/crypto/ssh`.

**Spec:** `docs/superpowers/specs/2026-11-02-saved-games-event-sourcing-design.md`

## Global Constraints

- Everything stays in `package main`. Files are named after the talk's terms so they're easy to find on stage.
- `events` rows are never updated or deleted. Current state must always be rebuildable from events.
- The UI never calls the aggregate or edits a `Game`. It sends commands through `Hub` and reads through the query functions.
- Projections never call command code, and the aggregate never reads projections.
- Applying an event never re-checks the rules. Only `GameAggregate` command methods reject anything.
- Event data is readable JSON. Colors are `"white"`/`"black"`, pieces are `"pawn"` etc., and squares are `"a7"`.
- Event types are exactly `GameStarted`, `MoveMade`, `GameEnded`. Outcomes are exactly `checkmate`, `stalemate`, `resignation`.
- The disconnect timeout is 30 seconds in production. It's a `Hub` parameter so tests can shorten it.
- No Cgo: the build stays a single binary.
- No snapshots, event versioning, spectating, draw offers, promotion choice, or board flip.
- Secret Manager startup code stays as it is.

## Decisions Made in This Plan

These go beyond the spec:

- **A restart with both players absent:** the player to move loses. Their timer is 30 seconds and the other player's is 31.
- **Dropping a table is enough to rebuild:** a projection also rebuilds if only its table or only its checkpoint is deleted, so the demo can't get stuck halfway.
- **Rule bugs are fixed:** the current code lets pawns jump pieces on a double move, lets a king castle with a captured rook, and wrongly blocks queenside castling when b1/b8 is attacked. Any of these could happen on stage.
- **Keyless connections:** the server accepts keyboard-interactive auth without a prompt, which is the only way such a client can receive the explanatory message. The library only records a key that was actually proven, so this can't be used to impersonate a player.
- **List labels:** `Won`, `Lost`, `Draw`, `Opp. left` (you won by resignation), and `Left` (you lost by resignation). Move counts are half-moves, as in the approved mockup.
- **Replay opens at the starting position.**
- **`active_games.board`** is a FEN piece-placement string, so a `SELECT` is readable on stage. Full state is kept in a separate JSON column.
- **Queue screen:** the current "explore the board while waiting" mode is removed. `Esc` returns to the menu.

## Review Focus

1. A player whose connection is replaced by the same key: the old session's cleanup must not start a timer or remove the new session.
2. `DROP TABLE` while players are connected: screens show a loading state, never crash, and the player isn't routed to the menu by mistake.
3. A timer firing as the player reconnects, or as a move ends the game: exactly one `GameEnded` is written, and a stale timer does nothing.
4. Server restart with a game in progress: projections are caught up before SSH accepts connections, so a returning player goes straight to their game.
5. Two local keys with an SSH agent running: without `IdentitiesOnly=yes` the agent may offer the same key both times, so the README must include it.

---

## File Structure

| File | Responsibility |
| --- | --- |
| `chess.go` | Rules: a side-effect-free library shared by both sides |
| `events.go` | Event type, event names, typed data structs |
| `aggregate.go` | `GameAggregate`: commands in, events out |
| `eventstore.go` | SQLite event table, `Append`, `Load`, `OpenDB` |
| `commands.go` | `Commands`: load, decide, append, retry on conflict |
| `projector.go` | `Projection` interface, background loop, checkpoints |
| `projection_active_games.go` | `active_games` table and its queries |
| `projection_my_games.go` | `my_games` table and its queries |
| `replay.go` | Build a step-through replay from a game's events |
| `hub.go` | Connected sessions, matchmaking queue, disconnect timers |
| `ui.go` | Bubble Tea screens, moved out of `main.go` |
| `main.go` | Flags, startup order, SSH server |
| `multiplayer.go` | Deleted in Task 7 |

---

### Task 1: Make the chess rules a shared library

**Files:**
- Modify: `chess.go`
- Test: `chess_test.go`

**Interfaces:**
- Produces:
  - `func (g *Game) IsLegalMove(from, to Position) bool`
  - `func (g *Game) ApplyMove(from, to Position)`: unconditional state change, including castling, en passant, automatic queen promotion, rights, turn change, and history
  - `func (g *Game) LegalMoves() []Move`: for the player to move
  - `func (g *Game) IsStalemate(color Color) bool`
  - `func (g *Game) IsCheckmate(color Color) bool`: reimplemented
  - `func (g *Game) IsPromotion(from, to Position) bool`
  - `func ParsePosition(s string) (Position, error)`
  - `func (b Board) Placement() string`: FEN piece placement, rank 8 first
  - `func (c Color) Name() string` returning `"white"`/`"black"`, and `func (t PieceType) Name() string` returning `"pawn"`, `"rook"`, `"knight"`, `"bishop"`, `"queen"`, `"king"`

- [ ] **Step 1: Write the failing tests**

Add a helper `gameFrom(turn Color, pieces map[string]Piece) *Game` that builds a position with all rights intact. Write these tests:

```go
func TestStalemate(t *testing.T) // Black Ka8, White Qb6, White Kh1, Black to move: IsStalemate(Black) true, IsCheckmate(Black) false, len(LegalMoves()) == 0
func TestFoolsMateIsCheckmate(t *testing.T) // f2f3 e7e5 g2g4 d8h4: IsCheckmate(White) true, IsStalemate(White) false
func TestCheckmateEscapeByEnPassant(t *testing.T) // A pawn giving check can only be removed by en passant: IsCheckmate false
func TestPromotionWhiteAndBlack(t *testing.T) // a7a8 becomes White Queen; a2a1 becomes Black Queen; capture-promotion too; IsPromotion true before the move
func TestPawnCannotJumpOnDoubleMove(t *testing.T) // Knight on e3: IsLegalMove(e2, e4) false
func TestCannotCastleWithCapturedRook(t *testing.T) // Ke1, no piece on h1, rights intact: IsLegalMove(e1, g1) false
func TestQueensideCastleAllowedWhenBFileAttacked(t *testing.T) // Black rook on b8, Ke1, Ra1, empty b1-d1: IsLegalMove(e1, c1) true
func TestCastleBlockedThroughCheck(t *testing.T) // Black rook on f8, Ke1, Rh1: IsLegalMove(e1, g1) false
func TestApplyMoveCastleMovesRook(t *testing.T) // e1g1: rook now on f1, h1 empty
func TestApplyMoveEnPassantRemovesPawn(t *testing.T) // e5 pawn takes d6 after d7d5: d5 empty
func TestIsLegalMoveLeavesBoardUnchanged(t *testing.T) // Placement() and CurrentTurn identical before and after IsLegalMove, including a rejected move and a castling check
func TestParsePositionRoundTrip(t *testing.T) // "a1", "h8" round trip; "i1", "a9", "" return errors
func TestPlacementStartingPosition(t *testing.T) // "rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR"
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'Stalemate|Checkmate|Promotion|Pawn|Castle|ApplyMove|IsLegalMove|ParsePosition|Placement'`
Expected: FAIL with undefined function errors.

- [ ] **Step 3: Implement in `chess.go`**

`IsLegalMove` is `IsValidMove` plus: clone the game, execute the move on the clone, and reject it if the mover is in check. `MakeMove` becomes `IsLegalMove` then `ApplyMove`, keeping its signature. `IsCheckmate` is in check with no legal moves, and `IsStalemate` is not in check with no legal moves. The clone must deep-copy `EnPassantTarget`; it doesn't need `MoveHistory`.

Fix the rules: a pawn double move requires the square between to be empty; `canCastle` requires a rook of the right color on the corner; queenside only requires b-file emptiness, not safety; en passant execution requires `abs(dx) == 1`. Remove `undoMove`, `undoCastle`, and `undoEnPassant` once they're unused.

- [ ] **Step 4: Run all tests and build**

Run: `go test ./... && go build ./...`
Expected: PASS, and the current UI still compiles.

- [ ] **Step 5: Commit**

```bash
git add chess.go chess_test.go
git commit -m "feat: make chess rules a shared library with stalemate and promotion"
```

---

### Task 2: Events and the aggregate

**Files:**
- Create: `events.go`, `aggregate.go`
- Test: `aggregate_test.go`

**Interfaces:**
- Consumes: all of Task 1.
- Produces:

```go
const (EventGameStarted = "GameStarted"; EventMoveMade = "MoveMade"; EventGameEnded = "GameEnded")
const (OutcomeCheckmate = "checkmate"; OutcomeStalemate = "stalemate"; OutcomeResignation = "resignation")

type Event struct {
    Sequence  int64           // assigned by the store
    GameID    string
    Version   int             // 1-based per game
    Type      string
    Data      json.RawMessage
    CreatedAt time.Time       // assigned by the store
}
type PlayerRef struct { Key string `json:"key"`; Name string `json:"name"` }
type GameStartedData struct { White PlayerRef `json:"white"`; Black PlayerRef `json:"black"` }
type MoveMadeData struct {
    Color     string `json:"color"`
    Piece     string `json:"piece"`
    From      string `json:"from"`
    To        string `json:"to"`
    Promotion string `json:"promotion,omitempty"`
}
type GameEndedData struct {
    Outcome string `json:"outcome"`
    Winner  string `json:"winner,omitempty"` // "white"/"black"; empty for stalemate
}

var ErrAlreadyStarted, ErrGameNotStarted, ErrGameOver, ErrNotInGame, ErrNotYourTurn, ErrIllegalMove error

type GameAggregate struct {
    ID      string
    Version int
    White   PlayerRef
    Black   PlayerRef
    Game    *Game
    Ended   bool
}
func NewGameAggregate(id string) *GameAggregate
func LoadGameAggregate(id string, events []Event) (*GameAggregate, error)
func (a *GameAggregate) Apply(e Event) error
func (a *GameAggregate) Start(white, black PlayerRef) ([]Event, error)
func (a *GameAggregate) MakeMove(playerKey string, from, to Position) ([]Event, error)
func (a *GameAggregate) Resign(playerKey string) ([]Event, error)
```

- [ ] **Step 1: Write the failing tests**

Helper `play(t, a, moves ...string)` runs moves such as `"e2e4"` through `MakeMove` for whichever player's turn it is, and applies the returned events.

```go
func TestStartEmitsGameStarted(t *testing.T)            // one event, Version 1; second Start returns ErrAlreadyStarted
func TestMakeMoveEmitsMoveMade(t *testing.T)            // data equals {white,pawn,e2,e4}, no promotion, Version 2
func TestMakeMoveDoesNotChangeStateUntilApplied(t *testing.T) // a.Version and board unchanged after MakeMove returns
func TestRejections(t *testing.T)                       // table: wrong turn ErrNotYourTurn; stranger ErrNotInGame; illegal ErrIllegalMove; before Start ErrGameNotStarted; after end ErrGameOver; all return no events
func TestSameKeyCannotBeBothPlayers(t *testing.T)       // Start(p, p) returns an error
func TestFoolsMateEndsTheGame(t *testing.T)             // d8h4 returns MoveMade then GameEnded{checkmate, black}
func TestStalemateEndsTheGame(t *testing.T)             // moves below; final move returns MoveMade then GameEnded{stalemate, ""}
func TestPromotionRecordedInEvent(t *testing.T)         // Promotion == "queen", and a loaded aggregate has a queen on the last rank
func TestResignNamesOpponentWinner(t *testing.T)        // White resigns: GameEnded{resignation, black}; after that ErrGameOver
func TestLoadRebuildsSameState(t *testing.T)            // Placement, Version, Ended, and turn from LoadGameAggregate equal the played aggregate
func TestApplyDoesNotValidate(t *testing.T)             // Apply a MoveMade that would be illegal: no error, board changes
```

The stalemate game (Loyd's 10-move stalemate): `e2e3 a7a5 d1h5 a8a6 h5a5 h7h5 h2h4 a6h6 a5c7 f7f6 c7d7 e8f7 d7b7 d8d3 b7b8 d3h7 b8c8 f7g6 c8e6`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'Start|MakeMove|Rejections|SameKey|EndsTheGame|Promotion|Resign|Load|ApplyDoesNot'`
Expected: FAIL with undefined errors.

- [ ] **Step 3: Implement `events.go` and `aggregate.go`**

Command methods never mutate the aggregate. To decide whether a move ends the game, they clone the game, apply the move to the clone, and check the opponent for checkmate and stalemate. Returned events have `GameID`, `Version`, `Type`, and `Data` set. `Apply` calls `Game.ApplyMove` and sets `Ended`, `Version`, and the players.

- [ ] **Step 4: Run all tests**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add events.go aggregate.go aggregate_test.go
git commit -m "feat: add game events and aggregate"
```

---

### Task 3: Event store and command execution

**Files:**
- Create: `eventstore.go`, `commands.go`
- Modify: `go.mod`, `go.sum`
- Test: `eventstore_test.go`, `commands_test.go`

**Interfaces:**
- Consumes: `Event`, `GameAggregate`, `PlayerRef`.
- Produces:

```go
var ErrConflict error
func OpenDB(path string) (*sql.DB, error) // parent directories created; WAL; busy_timeout 5000; MaxOpenConns 1; creates events
type EventStore struct{ db *sql.DB }
func NewEventStore(db *sql.DB) *EventStore
func (s *EventStore) Append(ctx context.Context, events []Event) error // atomic; ErrConflict on UNIQUE(game_id, version); sets nothing on the input
func (s *EventStore) Load(ctx context.Context, gameID string) ([]Event, error) // version order
func (s *EventStore) After(ctx context.Context, tx *sql.Tx, sequence int64) ([]Event, error) // sequence order

type Commands struct{ ... }
func NewCommands(store *EventStore, onAppended func()) *Commands
func (c *Commands) StartGame(ctx context.Context, gameID string, white, black PlayerRef) error
func (c *Commands) MakeMove(ctx context.Context, gameID, playerKey string, from, to Position) error
func (c *Commands) Resign(ctx context.Context, gameID, playerKey string) error
func NewGameID() string // "g_" + 16 hex characters from crypto/rand
```

`events` uses the exact schema in the spec. `MaxOpenConns(1)` means nothing may hold open `Rows` while issuing another query on the same `*sql.DB`.

- [ ] **Step 1: Add the driver**

Run: `go get modernc.org/sqlite@v1.61.1`

- [ ] **Step 2: Write the failing tests**

Helper `testDB(t)` opens `filepath.Join(t.TempDir(), "test.db")`.

```go
func TestAppendAndLoadPreservesEvents(t *testing.T)   // JSON data intact; Sequence and CreatedAt populated; other game's events excluded
func TestAppendConflict(t *testing.T)                 // same game and version: errors.Is(err, ErrConflict); row count unchanged
func TestAppendIsAtomic(t *testing.T)                 // batch of two where the second conflicts: neither stored
func TestAfterReturnsLaterEventsInOrder(t *testing.T) // across two games
func TestDataIsReadableJSON(t *testing.T)             // raw SELECT data contains "from":"e2"
func TestOpenDBCreatesParentDirectories(t *testing.T) // path under a directory that doesn't exist yet
func TestCommandsPersistAndNotify(t *testing.T)       // StartGame + MakeMove: Load returns 2 events; onAppended called twice
func TestCommandRejectionAppendsNothing(t *testing.T) // illegal move: ErrIllegalMove; event count unchanged; onAppended not called
func TestConcurrentResignAndMoveWritesOneEnding(t *testing.T) // goroutines race Resign and a mating move, repeated 50 times with fresh games: exactly one GameEnded per game
func TestConcurrentCommandsRetryOnConflict(t *testing.T) // two distinct Resign calls: one nil, one ErrGameOver; no ErrConflict leaks out
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./... -run 'Append|After|DataIsReadable|OpenDB|Command|Concurrent'`
Expected: FAIL with undefined errors.

- [ ] **Step 4: Implement `eventstore.go` and `commands.go`**

Use DSN `file:<path>?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)`. `Commands` methods share one loop: load events, rebuild the aggregate, run the decision, append. On `ErrConflict` they repeat, up to 5 attempts. `onAppended` runs only after a successful append with at least one event.

- [ ] **Step 5: Run all tests with the race detector**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum eventstore.go eventstore_test.go commands.go commands_test.go
git commit -m "feat: add SQLite event store and command execution"
```

---

### Task 4: Projector and the active games projection

**Files:**
- Create: `projector.go`, `projection_active_games.go`
- Test: `projector_test.go`, `projection_active_games_test.go`

**Interfaces:**
- Consumes: `EventStore.After`, `Event`, event data types, `Game.ApplyMove`, `Game.LegalMoves`, `Board.Placement`.
- Produces:

```go
type Projection interface {
    Name() string                     // also the table name
    Create(tx *sql.Tx) error          // CREATE TABLE
    Apply(tx *sql.Tx, e Event) error
}
type Projector struct{ ... }
func NewProjector(db *sql.DB, store *EventStore, onChange func(), projections ...Projection) *Projector
func (p *Projector) CatchUp(ctx context.Context) error // one pass over every projection, synchronous
func (p *Projector) Wake()                             // non-blocking
func (p *Projector) Run(ctx context.Context)           // on Wake or every 250ms, CatchUp; logs errors and continues

type ActiveGames struct{}
type ActiveGame struct {
    GameID     string
    White      PlayerRef
    Black      PlayerRef
    Game       *Game    // includes turn and last move
    LegalMoves []Move
    InCheck    bool
    Version    int
}
var ErrProjectionUnavailable error
func GetActiveGame(ctx context.Context, db *sql.DB, gameID string) (ActiveGame, bool, error)
func GetActiveGameForPlayer(ctx context.Context, db *sql.DB, playerKey string) (ActiveGame, bool, error)
func ListActiveGames(ctx context.Context, db *sql.DB) ([]ActiveGame, error)
```

Table `active_games`: `game_id` primary key, `white_key`, `white_name`, `black_key`, `black_name`, `board` (Placement), `turn` (`"white"`/`"black"`), `state` (JSON of `Game`), `legal_moves` (JSON), `in_check`, `version`. Index `white_key` and `black_key`. Query functions return `ErrProjectionUnavailable` when the table doesn't exist, so screens can show a loading state.

Table `projection_checkpoints`: `projection` primary key, `sequence`. `Projector` creates it.

- [ ] **Step 1: Write the failing tests**

Tests in `projector_test.go` use a small `countingProjection` that adds a row per event.

```go
func TestCatchUpProcessesAllEvents(t *testing.T)          // count equals events; checkpoint equals last sequence
func TestCatchUpIsIncremental(t *testing.T)               // second CatchUp with no new events leaves count unchanged and doesn't call onChange
func TestCrashRecoveryCatchesUp(t *testing.T)             // append with no projector; new Projector CatchUp processes them
func TestFailedApplyRollsBackAndRetries(t *testing.T)     // Apply errors on event 2: no rows, no checkpoint movement; CatchUp succeeds once fixed; no duplicates
func TestNewProjectionProcessesExistingHistory(t *testing.T) // add a second projection after games exist: it sees every event
func TestDropTableRebuilds(t *testing.T)                  // DROP table only: same rows after CatchUp; onChange called
func TestDeleteCheckpointRebuildsWithoutDuplicates(t *testing.T) // delete checkpoint only: same rows, no duplicates
func TestDropBothRebuildsEvenWithNoEvents(t *testing.T)   // onChange called even though no events exist
func TestRunWakesAndStops(t *testing.T)                   // Wake processes events promptly; cancelling ctx stops Run
```

Tests in `projection_active_games_test.go`:

```go
func TestActiveGameCreatedOnStart(t *testing.T)      // players, starting board, turn white, 20 legal moves
func TestActiveGameUpdatedByMove(t *testing.T)       // board "rnbqkbnr/pppppppp/8/8/4P3/8/PPPP1PPP/RNBQKBNR" after e2e4; turn black; Version 2
func TestActiveGameInCheckFlag(t *testing.T)         // after a checking move InCheck true, and LegalMoves are only escapes
func TestActiveGameRemovedOnEnd(t *testing.T)        // fool's mate: GetActiveGame found == false
func TestActiveGameForPlayerFindsEitherColor(t *testing.T) // white key and black key both find it; a stranger doesn't
func TestActiveGameMatchesAggregate(t *testing.T)    // after a game with castling, en passant, and promotion: Placement, turn, and legal moves equal LoadGameAggregate's
func TestQueriesReportUnavailableAfterDrop(t *testing.T) // errors.Is(err, ErrProjectionUnavailable), found false
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'CatchUp|Crash|FailedApply|NewProjection|Drop|DeleteCheckpoint|RunWakes|ActiveGame|QueriesReport'`
Expected: FAIL with undefined errors.

- [ ] **Step 3: Implement `projector.go` and `projection_active_games.go`**

Each projection is handled in one transaction in `CatchUp`, in this order, so it's never visible half-rebuilt:

```text
begin tx
if the table is missing or the checkpoint row is missing:
    DROP TABLE IF EXISTS <name>; Create(tx); checkpoint = 0; changed = true
events = store.After(tx, checkpoint)    -- read fully into a slice first
for each event: projection.Apply(tx, event); changed = true
save checkpoint
commit
```

Call `onChange` once at the end of a pass if any projection changed. `CatchUp` is serialized with a mutex so `Run` and direct calls can't overlap.

- [ ] **Step 4: Run all tests with the race detector**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add projector.go projector_test.go projection_active_games.go projection_active_games_test.go
git commit -m "feat: add background projector and active games projection"
```

---

### Task 5: My games projection and replay

**Files:**
- Create: `projection_my_games.go`, `replay.go`
- Test: `projection_my_games_test.go`, `replay_test.go`

**Interfaces:**
- Consumes: `Projection`, `EventStore`, event data types, `LoadGameAggregate`.
- Produces:

```go
type MyGames struct{}
type MyGame struct {
    GameID       string
    PlayerKey    string
    OpponentName string
    Color        string // "white"/"black"
    Result       string // "won", "lost", "draw"
    Reason       string // an Outcome value
    MoveCount    int    // half-moves
    StartedAt    time.Time
    EndedAt      time.Time
}
func (g MyGame) ResultLabel() string // Won, Lost, Draw, Opp. left, Left
func ListMyGames(ctx context.Context, db *sql.DB, playerKey string) ([]MyGame, error) // newest first; ErrProjectionUnavailable if the table is missing

type Replay struct {
    White, Black PlayerRef
    Positions    []Board  // len == len(Moves)+1; Positions[0] is the start
    Moves        []string // "f1-b5"
    Outcome, Winner string
    Finished     bool
    EndedAt      time.Time
}
func BuildReplay(events []Event) (Replay, error)
```

Table `my_games`: `game_id`, `player_key`, `opponent_name`, `color`, `result`, `reason`, `move_count`, `started_at`, `ended_at`, primary key `(game_id, player_key)`, index on `player_key`. On `GameEnded`, `Apply` reads the game's events from the `events` table through the same `tx` and writes two rows. It ignores `GameStarted` and `MoveMade`.

- [ ] **Step 1: Write the failing tests**

```go
func TestMyGamesTwoRowsPerFinishedGame(t *testing.T)  // fool's mate: white "lost", black "won", reason checkmate, MoveCount 4, each with the other's name
func TestMyGamesExcludesActiveGames(t *testing.T)     // started game: empty list
func TestMyGamesNewestFirst(t *testing.T)             // two finished games: order by EndedAt descending
func TestMyGamesScopedToPlayer(t *testing.T)          // a third player sees nothing
func TestResultLabels(t *testing.T)                   // table: checkmate won "Won", checkmate lost "Lost", stalemate "Draw", resignation won "Opp. left", resignation lost "Left"
func TestMyGamesRebuildIsIdentical(t *testing.T)      // list, DROP TABLE, CatchUp, list again: reflect.DeepEqual
func TestMyGamesQueryUnavailableAfterDrop(t *testing.T)
func TestReplayPositions(t *testing.T)                // fool's mate: 5 positions, Moves[3] == "d8-h4", Positions[0] is start, last equals aggregate's board
func TestReplayShowsPromotion(t *testing.T)           // last rank holds a queen after the promotion step, a pawn before it
func TestReplayCastlingMovesRook(t *testing.T)        // step after e1-g1 has rook on f1
func TestReplayPositionsAreIndependent(t *testing.T)  // Positions[1] != Positions[2]
func TestReplayOfEmptyEventsErrors(t *testing.T)
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'MyGames|ResultLabels|Replay'`
Expected: FAIL with undefined errors.

- [ ] **Step 3: Implement both files**

`BuildReplay` and the `my_games` handler both apply events through `GameAggregate.Apply`, the same way the aggregate rebuilds itself. That parallel is something to point at in the talk, so keep it visible.

- [ ] **Step 4: Run all tests with the race detector**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add projection_my_games.go projection_my_games_test.go replay.go replay_test.go
git commit -m "feat: add my games projection and replay"
```

---

### Task 6: Hub for sessions, matchmaking, and timers

**Files:**
- Create: `hub.go`
- Test: `hub_test.go`

**Interfaces:**
- Consumes: `Commands`, `GetActiveGameForPlayer`, `ListActiveGames`, `NewGameID`.
- Produces:

```go
type Session struct {
    Player  PlayerRef
    Updates <-chan struct{} // capacity 1; a signal to re-read, never data
    Closed  <-chan struct{} // closed when another connection replaces this one
}
type Hub struct{ ... }
func NewHub(db *sql.DB, commands *Commands, timeout time.Duration) *Hub
func (h *Hub) Connect(ctx context.Context, player PlayerRef) *Session
func (h *Hub) Disconnect(ctx context.Context, s *Session)
func (h *Hub) JoinQueue(ctx context.Context, s *Session) error
func (h *Hub) LeaveQueue(s *Session)
func (h *Hub) MakeMove(ctx context.Context, s *Session, gameID string, from, to Position) error
func (h *Hub) Broadcast()                                   // wired to Projector's onChange
func (h *Hub) OpponentDeadline(gameID, opponentKey string) (time.Time, bool)
func (h *Hub) RestoreTimers(ctx context.Context) error      // call once at startup
```

The hub is a live registry. It holds connected sessions, the queue, and timers, and no game state.

- [ ] **Step 1: Write the failing tests**

Use a real database, projector (`CatchUp` called directly in the test or via `onChange`), and a short timeout such as 50ms.

```go
func TestQueueMatchesTwoPlayers(t *testing.T)           // first to join is White; GameStarted stored with both refs
func TestQueueDoesNotMatchSameKey(t *testing.T)         // reconnect while queued replaces the entry; no game
func TestLeaveQueue(t *testing.T)                       // joins after leaving don't match the departed player
func TestBroadcastSignalsEverySessionWithoutBlocking(t *testing.T) // many Broadcasts with nobody reading: returns; one signal pending per session
func TestDisconnectStartsTimerAndResigns(t *testing.T)  // GameEnded{resignation, opponent} after timeout
func TestReconnectCancelsTimer(t *testing.T)            // reconnect inside the timeout: no GameEnded after it passes
func TestOpponentDeadlineReported(t *testing.T)         // true while disconnected; false after reconnect
func TestReplacedSessionDoesNotStartTimer(t *testing.T) // Connect twice with one key, Disconnect the first: no timer, new session stays registered, first's Closed is closed
func TestDisconnectFirstLosesWhenBothLeave(t *testing.T) // White at t=0, Black at t=timeout/2: Black wins; exactly one GameEnded
func TestTimerAfterGameEndsDoesNothing(t *testing.T)    // game ends during the timer: one GameEnded, no error logged as failure
func TestDisconnectWithoutActiveGameDoesNothing(t *testing.T)
func TestRestoreTimersPlayerToMoveLoses(t *testing.T)   // new Hub over existing game with a timeout: player to move resigns
func TestRestoreTimersCancelledByReconnect(t *testing.T) // both reconnect: game continues
func TestMakeMoveRejectsWrongGame(t *testing.T)         // a key that isn't in the game: ErrNotInGame
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'Queue|LeaveQueue|Broadcast|Disconnect|Reconnect|OpponentDeadline|ReplacedSession|Timer|MakeMoveRejects'`
Expected: FAIL with undefined errors.

- [ ] **Step 3: Implement `hub.go`**

Compare `*Session` pointers, not keys, when removing sessions, so a replaced session's cleanup can't touch its successor. `time.AfterFunc` timers call `commands.Resign` outside the hub's mutex, and `ErrGameOver` is treated as success. Timers are keyed by game and player key. `Disconnect` finds the player's game with `GetActiveGameForPlayer`; if that returns `ErrProjectionUnavailable`, it retries every 50ms up to 2 seconds so a rebuild can't lose a timer. In `RestoreTimers`, the player to move gets `timeout` and the other gets `timeout + 1s`.

- [ ] **Step 4: Run all tests with the race detector**

Run: `go test -race ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add hub.go hub_test.go
git commit -m "feat: add hub for matchmaking and disconnect timers"
```

---

### Task 7: Live play over SSH keys

Replaces the old in-memory flow with the menu, queue, live game, and game-over screens.

**Files:**
- Create: `ui.go`
- Modify: `main.go`
- Delete: `multiplayer.go`
- Test: `ui_test.go`, `e2e_test.go`

**Interfaces:**
- Consumes: `Hub`, `Session`, `GetActiveGameForPlayer`, `GetActiveGame`, `EventStore.Load`, `BuildReplay`, `Projector`.
- Produces:

```go
type App struct { DB *sql.DB; Store *EventStore; Commands *Commands; Hub *Hub; Projector *Projector }
func NewApp(db *sql.DB, timeout time.Duration) *App      // wires onAppended to Projector.Wake and onChange to Hub.Broadcast
func (a *App) Start(ctx context.Context) error           // CatchUp, RestoreTimers, then go Projector.Run. In that order.
func NewSSHServer(addr string, hostKey []byte, app *App) (*ssh.Server, error)
func playerKey(key ssh.PublicKey) string                 // gossh.FingerprintSHA256
func newModel(ctx context.Context, app *App, s *Session) model
```

Add a `--db` flag with default `~/.chessh/chessh.db`.

Screens in `ui.go`: `screenLoading`, `screenMenu`, `screenQueue`, `screenPlaying`, `screenGameOver`. Task 8 adds the other two. All re-reads happen in `tea.Cmd`s triggered by `Session.Updates`.

| Screen | Reads | Keys |
| --- | --- | --- |
| Menu | none | up/down/`j`/`k`, enter, `q` quits |
| Queue | `GetActiveGameForPlayer` on each update | `Esc` back to menu, `q` quits |
| Playing | `GetActiveGame`; highlights from `ActiveGame.LegalMoves` | existing cursor keys; `q` quits |
| Game over | `Store.Load` + `BuildReplay` | any key to menu; `Ctrl+C` still quits |

Routing: on start and on each update, `GetActiveGameForPlayer` with a game means `screenPlaying`. `ErrProjectionUnavailable` means `screenLoading`, staying put until the next update. No active game means the menu. While playing, a row that vanishes without an error means `screenGameOver`.

- [ ] **Step 1: Write the failing tests**

`ui_test.go` drives `model.Update` directly with a real `App` and a short timeout, running returned `tea.Cmd`s, and asserts on `View()` text.

```go
func TestMenuShownWithoutActiveGame(t *testing.T)     // contains "Play" and "My games"
func TestActiveGameSkipsMenu(t *testing.T)            // key with active game: board shown, not "My games"
func TestSelectAndMoveSendsCommand(t *testing.T)      // cursor to e2, enter, e4, enter: MoveMade stored
func TestUIDoesNotMoveWhenNotYourTurn(t *testing.T)   // nothing stored
func TestHighlightsComeFromProjection(t *testing.T)   // selecting e2 highlights e3 and e4 only
func TestCheckMessageShown(t *testing.T)              // "in check" after a checking move
func TestOpponentCountdownShown(t *testing.T)         // opponent disconnected: "disconnected" and seconds remaining
func TestGameOverShowsFinalBoardAndResult(t *testing.T) // fool's mate: both models show the mated position and "Black wins"; any key returns to menu
func TestStalemateGameOverMessage(t *testing.T)       // "Stalemate"
func TestResignationGameOverMessage(t *testing.T)     // winner told the opponent left
func TestLoadingWhenProjectionDropped(t *testing.T)   // DROP active_games: playing model shows "Loading" without panicking; after CatchUp the board returns
func TestQueueEscReturnsToMenu(t *testing.T)          // and a later player isn't matched with them
```

`e2e_test.go` starts `NewSSHServer` on a free localhost port and uses `gossh` clients with generated ed25519 keys, a PTY, and a helper `waitFor(t, output, substring)` with a 5-second limit:

```go
func TestE2EFullGameListedAndReplayed(t *testing.T)    // two keys, fool's mate, game over; then My games lists it (assert in Task 8)
func TestE2EKeylessClientGetsMessage(t *testing.T)     // keyboard-interactive only: output contains "SSH key" and the session ends
func TestE2EReconnectResumes(t *testing.T)             // close a client mid-game, reconnect with same key within timeout: board shown; game continues
func TestE2ETimeoutResigns(t *testing.T)               // close a client; opponent sees countdown, then game over
func TestE2ERestartResumes(t *testing.T)               // shut down server, NewApp over same db file, Start, new server: both keys return to the board at the same position
func TestE2ESecondConnectionReplacesFirst(t *testing.T) // first connection ends; second can play
```

Remove the `My games` assertion comment once Task 8 adds it. In this task, stop `TestE2EFullGameListedAndReplayed` at the game-over screen.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'Menu|ActiveGameSkips|SelectAndMove|UIDoesNot|Highlights|CheckMessage|Countdown|GameOver|Loading|QueueEsc|E2E'`
Expected: FAIL with undefined errors.

- [ ] **Step 3: Move and rewrite the UI**

Move the model and its rendering from `main.go` to `ui.go`. Reuse `getBoardLines` for every board. Delete `multiplayer.go`, `model.getValidMoves`, and the cursor and selection broadcasts. Render the board from a `Board` value so playing, game-over, and replay share one function. Show `Move` errors from `Hub.MakeMove` as a one-line message.

- [ ] **Step 4: Rewire `main.go`**

Accept both auth methods, and reject keyless sessions in a middleware that runs before the Bubble Tea one:

```go
wish.WithPublicKeyAuth(func(ssh.Context, ssh.PublicKey) bool { return true }),
wish.WithKeyboardInteractiveAuth(func(ssh.Context, gossh.KeyboardInteractiveChallenge) bool { return true }),
```

The rejection message: `CheSSH identifies players by SSH key, and your client didn't offer one.` followed by `Create one with: ssh-keygen -t ed25519`. In the Bubble Tea handler, use `s.PublicKey()` and `s.User()` for the `PlayerRef`, call `Hub.Connect`, and in a goroutine wait for `s.Context().Done()` to call `Hub.Disconnect`. When `Session.Closed` fires, close the SSH session with a one-line "connected elsewhere" message.

Startup order in `main`: open the DB, `NewApp`, `app.Start`, then begin listening, so no one connects before projections have caught up.

Run `go mod tidy` so `golang.org/x/crypto` becomes a direct dependency.

- [ ] **Step 5: Run all tests with the race detector**

Run: `go test -race ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -m "feat: run live games through events with SSH key identity"
```

---

### Task 8: My games and replay screens

**Files:**
- Modify: `ui.go`, `ui_test.go`, `e2e_test.go`

**Interfaces:**
- Consumes: `ListMyGames`, `MyGame.ResultLabel`, `EventStore.Load`, `BuildReplay`.
- Produces: `screenMyGames`, `screenReplay`.

Layout follows the approved mockup. List columns are `Date  Opponent  You  Result  Moves`, dated `Jan 2`, with a selected row. The replay shows the board on the left and the numbered move list on the right, with the current move highlighted, a `Move N of M` line, and the header `<you> vs <opponent>` with the date and result.

- [ ] **Step 1: Write the failing tests**

```go
func TestMyGamesListShowsGame(t *testing.T)            // opponent, color, "Won", 4 after a finished game
func TestMyGamesEmptyState(t *testing.T)               // new player: "No games yet"
func TestMyGamesEscReturnsToMenu(t *testing.T)
func TestMyGamesSelectionStaysInRange(t *testing.T)    // up at top and down at bottom don't panic
func TestMyGamesManyGamesScroll(t *testing.T)          // 50 games in a 24-line window: selected row visible, no overflow
func TestMyGamesRefreshesAfterProjectionDrop(t *testing.T) // DROP my_games: "Loading"; after CatchUp the same rows
func TestReplayStartsAtStartingPosition(t *testing.T)  // "Move 0 of 4"
func TestReplayStepsForwardAndBack(t *testing.T)       // right: "Move 1 of 4" and pawn moved; left returns
func TestReplayStopsAtEnds(t *testing.T)               // left at 0 and right at 4 stay put
func TestReplayEscReturnsToList(t *testing.T)          // same row selected
func TestReplayUnaffectedByProjectionDrop(t *testing.T) // DROP both tables: replay keeps working, since it reads events
```

Extend `TestE2EFullGameListedAndReplayed` to open My games, open the game, step to `Move 4 of 4`, and find the `d8-h4` move.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./... -run 'MyGames|Replay|E2EFullGame'`
Expected: FAIL.

- [ ] **Step 3: Implement both screens in `ui.go`**

Replay state is loaded once when opened. The list uses the window height from `tea.WindowSizeMsg` to scroll.

- [ ] **Step 4: Run all tests with the race detector**

Run: `go test -race ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add ui.go ui_test.go e2e_test.go
git commit -m "feat: add my games and replay screens"
```

---

### Task 9: README and demo rehearsal

**Files:**
- Modify: `README.md`
- Modify: `.gitignore` if the build leaves a binary or database in the repo

- [ ] **Step 1: Update `README.md`**

- Add that an SSH key is required.
- Update Quick start for two local players:

```bash
ssh-keygen -t ed25519 -f /tmp/chessh-white -N ''
ssh-keygen -t ed25519 -f /tmp/chessh-black -N ''
ssh -p 2222 -i /tmp/chessh-white -o IdentitiesOnly=yes white@localhost
ssh -p 2222 -i /tmp/chessh-black -o IdentitiesOnly=yes black@localhost
```

- Update How to play: the menu, My games, replay keys, reconnecting, and the 30-second timer.
- Replace Current limitations with what's still missing: draw offers, repetition, the fifty-move rule, insufficient material, and choosing a promotion piece.
- Add `--db` to the options table.
- Add a Project layout row for each new file.
- Add an "Event sourcing in CheSSH" section mapping aggregate, event store, projection, and CQRS to files, plus the `sqlite3` commands from Step 2.

- [ ] **Step 2: Rehearse the talk demos with real OpenSSH and `sqlite3`**

Build with `go build -o /tmp/chessh . && /tmp/chessh --local --db /tmp/chessh-demo.db`, and connect two players using the README commands. Confirm each one:

1. Events are visible during a game: `sqlite3 /tmp/chessh-demo.db "SELECT sequence, game_id, version, type, data FROM events;"`
2. Rebuild with players connected: `sqlite3 /tmp/chessh-demo.db "DROP TABLE active_games;"`. Both boards return, and the next move works. Repeat with `DROP TABLE my_games;` on the My games screen, and with `DELETE FROM projection_checkpoints WHERE projection='my_games';`.
3. Kill and restart: `kill -9` the server mid-game and start it again. Both players reconnect within 30 seconds to the same position.
4. Replay a finished game from My games.
5. Add a temporary win/loss projection file and one registration line, restart, and confirm it includes earlier games. Don't commit it. Record the number of lines changed outside the new file: it should be one.

Expected: all five work as described. Fix any failures before continuing.

- [ ] **Step 3: Run the full suite a final time**

Run: `go test -race -count=3 ./... && go vet ./... && go build ./...`
Expected: PASS with no flaky failures.

- [ ] **Step 4: Commit**

```bash
git add README.md .gitignore
git commit -m "docs: document saved games and the event sourcing design"
```
