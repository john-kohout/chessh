# CheSSH

Two-player chess in your terminal, played over SSH. There's nothing to install on the client: if you have `ssh`, you can play.

> [!IMPORTANT]
> It's pronounced like _"chess-ess-aych"_, or _"chesh"_ in your best Sean Connery impression.

![Screenshot of CheSSH gameplay](screenshot.png)

## Quick start

You'll need [Go](https://go.dev/) 1.25 or newer to run the server.

CheSSH identifies players by their SSH public key, so every player needs one. Make a key for each of two local players:

```bash
ssh-keygen -t ed25519 -f /tmp/chessh-white -N ''
ssh-keygen -t ed25519 -f /tmp/chessh-black -N ''
```

Start the server:

```bash
go run ./ --local
```

Then connect from two terminals, since a game needs two players:

```bash
ssh -p 2222 -i /tmp/chessh-white -o IdentitiesOnly=yes white@localhost
ssh -p 2222 -i /tmp/chessh-black -o IdentitiesOnly=yes black@localhost
```

The username is only the name your opponent sees. Your key is who you are: connect again with the same key to see your games.

## How to play

A menu offers **Play** and **My games** (up and down choose, `Enter` selects). Play puts you in a queue, and players are matched in the order they connect. The first player in the queue plays White, and the second plays Black.

| Key | Action |
| --- | --- |
| Arrow keys or `h` `j` `k` `l` | Move the cursor |
| `Space` or `Enter` | Select a piece, then choose its destination |
| `Esc` | Clear your selection (or leave the queue) |
| `q` or `Ctrl+C` | Quit. Mid-game, this starts your 30-second clock: come back in time or you lose by resignation |

Games end by checkmate, stalemate, or resignation, which happens when a player stays away for 30 seconds. When one ends, press any key to return to the menu.

**My games** lists your finished games, newest first. Press `Enter` to replay one, `left` and `right` (or `h` and `l`) to step through its moves, and `Esc` to go back.

**Reconnecting:** if you lose your connection mid-game, connect again with the same key and you're back in the game at the same position. If you stay away for 30 seconds, you lose by resignation. A server restart doesn't end games either: reconnect within 30 seconds of it coming back.

## Not implemented yet

- Draw offers
- Draws by repetition, the fifty-move rule, or insufficient material
- Choosing a promotion piece

## Running the server

| Option | Default | Description |
| --- | --- | --- |
| `--local` | off | Generate a host key, or reuse the one at `~/.chessh/host_key` |
| `--port` | `2222` | SSH port to listen on |
| `--db` | `~/.chessh/chessh.db` | Path to the SQLite database that holds every game |

Without `--local`, the server loads its host key from Google Secret Manager. Set these environment variables:

| Variable | Description |
| --- | --- |
| `SSH_HOST_KEY_SECRET` | Full resource name of the secret version that holds the host key (required without `--local`) |
| `PORT` | If set, serves a health check at `/health` on this port |

## Event sourcing in CheSSH

CheSSH stores what happened, not what things look like now. A game is a list of events in one SQLite table, and everything on screen is rebuilt from that list. The pieces have the usual names:

| Idea | Where it lives |
| --- | --- |
| Events: `GameStarted`, `MoveMade`, `GameEnded` | `events.go` |
| Aggregate: decides whether a command is allowed and which events result | `aggregate.go` |
| Commands: load the aggregate, run the command, append the new events | `commands.go` |
| Event store: the append-only `events` table | `eventstore.go` |
| Projections: read tables built from events (`active_games`, `my_games`) | `projection_active_games.go`, `projection_my_games.go` |
| Projector: feeds new events to each projection and remembers how far it got | `projector.go` |
| CQRS: the UI sends commands through the hub and reads from projections | `hub.go`, `ui.go` |

Events are never updated or deleted. Projections can always be thrown away and rebuilt. Point `sqlite3` at the database while a game is running (`/tmp/chessh-demo.db` here; use your `--db` path):

```bash
go build -o /tmp/chessh . && /tmp/chessh --local --db /tmp/chessh-demo.db
```

> [!NOTE]
> Use `sqlite3` 3.51.3 or newer. Older builds are inside SQLite's WAL-reset bug range. Use `-cmd ".timeout 5000"` for anything that writes: the `sqlite3` CLI has no busy timeout by default, and the server takes the write lock briefly every 250ms. Don't leave a `BEGIN` open, or the server stalls.

**Watch the events** as moves are played:

```bash
sqlite3 -cmd ".timeout 5000" /tmp/chessh-demo.db "SELECT sequence, game_id, version, type, data FROM events;"
```

**Rebuild a projection** while players are connected. Both boards come back and the next move works:

```bash
sqlite3 -cmd ".timeout 5000" /tmp/chessh-demo.db "DROP TABLE active_games;"
sqlite3 -cmd ".timeout 5000" /tmp/chessh-demo.db "DROP TABLE my_games;"
sqlite3 -cmd ".timeout 5000" /tmp/chessh-demo.db "DELETE FROM projection_checkpoints WHERE projection='my_games';"
```

The projector notices a missing table or checkpoint and replays every event into a new one. Drop the table; don't `DELETE FROM` it.

**Kill the server mid-game**, start it again with the same command, and reconnect within 30 seconds: both players are back at the same position, because the game is its events.

**Add a projection** by writing one file that implements `Projection` (`Name`, `Create`, `Apply`) and adding it to the list in `NewApp` in `main.go`. On the next start it is built from every game ever played, including those from before it existed.

## Project layout

| File | Contents |
| --- | --- |
| `main.go` | SSH server, command-line options, and wiring |
| `ui.go` | Terminal UI: menu, board, My games, and replay screens |
| `chess.go` | Board and chess rules |
| `events.go` | Event types and their JSON data |
| `eventstore.go` | The SQLite `events` table: append and load |
| `aggregate.go` | `GameAggregate`: validates commands and produces events |
| `commands.go` | Command handling: load, decide, append |
| `projector.go` | `Projection` interface and the projector that keeps projections current |
| `projection_active_games.go` | `active_games`: games in progress |
| `projection_my_games.go` | `my_games`: each player's finished games |
| `replay.go` | Builds a replay from a game's events |
| `hub.go` | Matchmaking, reconnects, and the 30-second disconnect timer |

## License

[Apache 2.0](LICENSE)
