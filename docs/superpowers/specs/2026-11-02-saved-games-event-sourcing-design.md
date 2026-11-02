# Saved games with event sourcing: design

## Why this exists

CheSSH is two-player chess over SSH. Today every game lives in memory and is gone when it ends.

This work has two purposes, and the second one shapes most decisions:

1. **Product:** a returning player can see a list of their games and replay any of them move by move.
2. **Teaching:** the owner is giving a talk on event sourcing, and CheSSH is the worked example. The audience should leave understanding event sourcing and CQRS, with a working definition of **aggregate**, **event store**, and **projection**.

Simpler designs could deliver the product goal. A moves table would be enough for replay, for example. We're choosing event sourcing on purpose because it's what the talk teaches. When clarity for the audience conflicts with minimal code, choose clarity.

## Goals

- Players are identified by SSH key, and their games are tied to that key.
- Connecting without an active game shows a menu: **Play** or **My games**.
- **My games** lists finished games, and any of them can be replayed move by move.
- Connecting with an active game returns you straight to that game.
- All game state is derived from an append-only event log in SQLite.
- Each talk term maps to code and data that someone can point at, and the code is readable on a slide.
- The server can be killed mid-game and restarted, and both players can continue.

## Non-goals

- Hosting. This runs locally for the talk, and no hosted copy exists. The existing Secret Manager startup code can stay as it is, and it doesn't need to be tested or extended.
- Running more than one server. SQLite means one machine.
- Snapshots, event versioning or upcasting, or event replication.
- Accounts, ratings, spectating, or chat.
- Draw offers, repetition, the fifty-move rule, insufficient material, or choosing a promotion piece.
- A built-in viewer for events. The presenter uses `sqlite3` directly.
- Flipping the board for Black.

## Anti-goals

This would be a failure even if it worked:

- A shortcut that blurs the teaching concepts, such as a read screen querying the aggregate, or a command handler editing a projection directly.
- Current state being stored anywhere that can't be rebuilt from events.
- Code so abstract or framework-heavy that the concepts can't be seen on a slide.
- A demo that can fail on stage because of a rule gap, such as a stalemate that leaves both players stuck.

## How the talk terms map to the code

| Term | In CheSSH |
| --- | --- |
| **Aggregate** | The game. Takes commands, applies the chess rules, emits events. Rebuilt from its events for every command. It's the only place that decides whether a move is legal. |
| **Event store** | One append-only SQLite table. Rows are never updated or deleted. |
| **Projection** | `active_games` (current board and turn) and `my_games` (finished-game list). Built only from events, updated in the background, and safe to delete and rebuild. |
| **CQRS** | Commands go to the aggregate and write events. All screens read from projections or, for replay, from the event stream. Nothing on the read side calls the aggregate. The two sides are eventually consistent. |

## Write side

### Commands and events

Commands: `StartGame`, `MakeMove`, `Resign`.

Events, stored as readable JSON:

| Event | Data |
| --- | --- |
| `GameStarted` | White and Black: key fingerprint and display name |
| `MoveMade` | Color, piece, from, to, and promotion piece if any |
| `GameEnded` | Outcome (`checkmate`, `stalemate`, `resignation`) and winner (none for stalemate) |

Example `MoveMade` data: `{"color":"white","piece":"pawn","from":"a7","to":"a8","promotion":"queen"}`

Applying an event doesn't re-check the rules. The event is a record of something that already happened. This is a point to bring out in the talk.

Not events: cursor movement, piece selection, connecting, disconnecting, and the timer itself. They're session activity. A timeout only reaches the log through the `Resign` command and the resulting `GameEnded`.

### Aggregate

For each command:

1. Load the game's events and apply them to get current state.
2. Check the command, including that it's that player's turn, that the player is in this game, and that the game hasn't ended.
3. Return new events: a `MoveMade`, followed by a `GameEnded` if the move ends the game.
4. Append them to the event store.

The aggregate reuses the existing rules in `chess.go`. The code must stop editing the shared `*Game` from the UI, which is what `main.go:156` does today.

### Rule changes

- **Stalemate:** the player to move isn't in check and has no legal move. Ends the game with no winner.
- **Promotion:** a pawn reaching the last rank automatically becomes a queen.
- **Checkmate:** becomes a real ending through `GameEnded`, rather than just text on screen.
- Checkmate and stalemate must use one shared "does this player have a legal move" check that applies the same rules as `MakeMove`. The current `IsCheckmate` uses a simpler try-the-move approach (`chess.go:498`) that doesn't handle special moves such as en passant, so it can't be reused as is.

### Event store

SQLite, in an `events` table:

```sql
CREATE TABLE events (
  sequence   INTEGER PRIMARY KEY AUTOINCREMENT,
  game_id    TEXT NOT NULL,
  version    INTEGER NOT NULL,
  type       TEXT NOT NULL,
  data       TEXT NOT NULL,   -- JSON
  created_at TEXT NOT NULL,
  UNIQUE (game_id, version)
);
```

`UNIQUE (game_id, version)` stops two conflicting writes from both succeeding, which gives the talk a simple example of optimistic concurrency. Game IDs must stay unique across restarts, so the current `game_N` counter has to go.

## Read side

### Projections

Projections update in the background, separately from the event being written. A small loop reads new events in `sequence` order and passes them to each projection. Each projection records the last event it processed in a `projection_checkpoints` table, updated in the same transaction as the projection's own changes so that a crash can't apply an event twice or skip one.

This is the talk's example of **eventual consistency**: for a few milliseconds, an event exists that a screen doesn't show yet. Players shouldn't notice. Screens refresh after the projection has processed the event, not when the command succeeds.

Three behaviors follow, and all of them must work without a server restart:

- **Rebuild:** deleting a projection's table and its checkpoint makes it rebuild from the first event while players stay connected.
- **Recovery:** if the server dies between writing an event and updating a projection, the projection catches up on the next start.
- **New projections:** a new projection starts with no checkpoint, so it processes all of the existing history.

The aggregate never reads projections, so a missing or rebuilding projection can't stop a move from being accepted. Screens may briefly show a loading state during a rebuild, and they must not crash.

**`active_games`**: one row per game in progress, holding the players, the current board, whose turn it is, the last move, and the version. It's created by `GameStarted`, updated by `MoveMade`, and removed by `GameEnded`.

**`my_games`**: one row per player per finished game, holding the player's key, the opponent's name, the player's color, the result, the reason, the move count, and the dates. It's created by `GameEnded`.

`GameEnded` updating both projections is deliberate, because it shows one event driving two views.

### Screens

Mocked up and approved:

- **Menu:** Play and My games, plus `q` to quit. Shown only when the player has no active game.
- **My games:** finished games, newest first, with date, opponent, your color, result, and move count.
- **Replay:** the board next to the move list. Left and right step through moves, and `Esc` goes back. Moves are shown as `f1-b5`.
- **Live game:** both players' boards render from `active_games`, not from the aggregate.

- **Game over:** both players see the final board and the result, whatever the ending. This screen reads the game's events, as replay does, because `GameEnded` has already removed the `active_games` row. Any key returns to the menu. A player who reconnects after the game ended gets the menu, and the game is in My games.

Replay reads the game's events and applies them in order. It's a good parallel to show next to the aggregate, which rebuilds its state the same way.

### Move highlights and the check banner

The live screen keeps today's legal-move highlights and check message. `active_games` stores what they need: each game's legal moves and whether the player to move is in check, computed when the projection applies an event.

The chess rules are a shared, side-effect-free library used by the aggregate, projections, and replay, including one shared "apply this event to the board" function. Using the rules to prepare a view is allowed. Only the aggregate decides whether a command is accepted, and it never relies on projection data to do so.

## Identity

- A player's identity is their SSH public key fingerprint. The server must start asking for public keys.
- The display name is the SSH username, stored in `GameStarted`. It can be faked, and only the key protects a player's games.
- Connecting without a key shows a short message explaining why, and then disconnects.
- A second connection with the same key replaces the first.
- You can't play yourself with one key. A one-laptop demo needs two keys, using `ssh -i`.

## Disconnects and restarts

- Disconnecting mid-game starts a 30-second timer, and the opponent sees a countdown.
- Reconnecting with the same key cancels the timer and returns the player to the game.
- If the timer runs out, the disconnected player resigns and the opponent wins.
- Pressing `q` mid-game counts as a disconnect.
- If both are gone, the player who disconnected first loses, because their timer runs out first.
- After a server restart, active games are restored and timers start again from zero for both players.

## Talk demos this has to support

The presenter should be able to do all of these live:

1. Show the events table with `sqlite3` while a game is played.
2. Delete a projection's table and checkpoint with `sqlite3` while players are connected, and show the screen return unchanged once the projection has rebuilt.
3. Kill the server mid-game, restart it, and have both players resume.
4. Replay a finished game.
5. Add a new projection, such as win/loss stats, and have it cover games played before it existed. It isn't built as part of this work, but the design must keep it small, ideally a single new file.

## Decisions made on the owner's behalf

The owner was shown these and didn't object, but didn't choose them. They're worth checking.

- Event names and data above.
- Keeping cursor and selection, connections, and timers out of the event log.
- Quitting with `q` counts as a disconnect.
- SSH usernames as display names.
- No board flip, and `f1-b5` move format.
- The remaining player may keep moving while the opponent's timer runs.

These were settled after the owner last reviewed the design, so they haven't been shown yet:

- A game-over screen that reads the game's events.
- Keeping move highlights and the check message, stored in `active_games`, with the chess rules as a shared library.

## Left to the builder

- File and package layout, as long as the talk terms are easy to find in the code.
- The SQLite driver. Prefer one that keeps the build to a single binary with no C toolchain.
- The database location and flag.
- How sessions are told that state has changed.
- How the current board is stored in `active_games`.

## Definition of done

- Automated tests cover the aggregate (including stalemate, promotion, checkmate, and resignation), the event store's conflict protection, rebuilding each projection from events, and catching up after a crash between an event and its projection update. The repo has no tests today.
- The rebuild demo has been run with players connected.
- A two-player game has been played end to end over real SSH connections, then listed and replayed.
- The disconnect, reconnect, timeout, and restart cases have been checked over real SSH connections.
- The README is updated: SSH key requirement, two-key local play, removal of limitations that no longer apply, and a short section on the event sourcing design and how to inspect the database.
