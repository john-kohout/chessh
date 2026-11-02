# CheSSH

Two-player chess in your terminal, played over SSH. There's nothing to install on the client: if you have `ssh`, you can play.

> [!IMPORTANT]
> It's pronounced like _"chess-ess-aych"_, or _"chesh"_ in your best Sean Connery impression.

![Screenshot of CheSSH gameplay](screenshot.png)

## Quick start

You'll need [Go](https://go.dev/) 1.25 or newer to run the server.

Start the server:

```bash
go run ./ --local
```

Then connect from two terminals, since a game needs two players:

```bash
ssh localhost -p 2222
```

## How to play

Players are matched in the order they connect. The first player in the queue plays White, and the second plays Black.

| Key | Action |
| --- | --- |
| Arrow keys or `h` `j` `k` `l` | Move the cursor |
| `Space` or `Enter` | Select a piece, then choose its destination |
| `Esc` | Clear your selection |
| `q` or `Ctrl+C` | Quit |

If your opponent disconnects, you win.

## Current limitations

- Checkmate and check are announced on screen, but a game doesn't end until a player disconnects.
- Pawn promotion, stalemate, draws, and resigning aren't implemented.
- Games are kept in memory only, so they're lost when the server restarts, and players can't view past games.

## Running the server

| Option | Default | Description |
| --- | --- | --- |
| `--local` | off | Generate a host key, or reuse the one at `~/.chessh/host_key` |
| `--port` | `2222` | SSH port to listen on |

Without `--local`, the server loads its host key from Google Secret Manager. Set these environment variables:

| Variable | Description |
| --- | --- |
| `SSH_HOST_KEY_SECRET` | Full resource name of the secret version that holds the host key (required without `--local`) |
| `PORT` | If set, serves a health check at `/health` on this port |

## Project layout

| File | Contents |
| --- | --- |
| `main.go` | SSH server and terminal UI |
| `multiplayer.go` | Matchmaking and game sessions |
| `chess.go` | Board and chess rules |

## License

[Apache 2.0](LICENSE)
