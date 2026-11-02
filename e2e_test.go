package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/ssh"
	gossh "golang.org/x/crypto/ssh"
)

const e2eTimeout = 5 * time.Second

// e2eServer is the real thing on a free localhost port: database file, App,
// and SSH server.
type e2eServer struct {
	t      *testing.T
	addr   string
	db     *sql.DB
	app    *App
	server *ssh.Server
	cancel context.CancelFunc
	once   sync.Once
}

func startE2EServer(t *testing.T, dir string, timeout time.Duration) *e2eServer {
	t.Helper()
	db, err := OpenDB(filepath.Join(dir, "chessh.db"))
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := generateOrLoadHostKey(filepath.Join(dir, "host_key"))
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp(db, timeout)
	ctx, cancel := context.WithCancel(context.Background())
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewSSHServer(listener.Addr().String(), hostKey, app)
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)

	s := &e2eServer{t: t, addr: listener.Addr().String(), db: db, app: app, server: server, cancel: cancel}
	t.Cleanup(s.stop)
	return s
}

// stop shuts everything down as a process exit would: no resignation written
// after this returns, because the timers are gone with the database.
func (s *e2eServer) stop() {
	s.once.Do(func() {
		s.server.Close()
		// Each closed session's Disconnect runs in its own goroutine. Let them
		// start their timers, so none starts after we stop them below.
		deadline := time.Now().Add(e2eTimeout)
		for !s.settled() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		s.cancel()
		s.app.Hub.mu.Lock()
		for _, running := range s.app.Hub.timers {
			running.timer.Stop()
		}
		s.app.Hub.mu.Unlock()
		s.db.Close()
	})
}

func (s *e2eServer) settled() bool {
	games, err := ListActiveGames(context.Background(), s.db)
	if err != nil {
		return false
	}
	h := s.app.Hub
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions) == 0 && len(h.timers) >= 2*len(games)
}

// e2eClient is an SSH client on a PTY whose output piles up in a buffer.
type e2eClient struct {
	t       *testing.T
	client  *gossh.Client
	session *gossh.Session
	stdin   io.Writer
	output  *lockedBuffer
	ended   chan struct{} // closed when the SSH session ends
	row     int
	col     int
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newSigner(t *testing.T) gossh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func dial(t *testing.T, addr, name string, auth ...gossh.AuthMethod) *e2eClient {
	t.Helper()
	client, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{
		User:            name,
		Auth:            auth,
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         e2eTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	c := &e2eClient{t: t, client: client, session: session, output: &lockedBuffer{}, ended: make(chan struct{})}
	session.Stdout, session.Stderr = c.output, c.output
	if c.stdin, err = session.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := session.RequestPty("xterm-256color", 24, 80, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := session.Shell(); err != nil {
		t.Fatal(err)
	}
	go func() {
		session.Wait()
		close(c.ended)
	}()
	return c
}

func connect(t *testing.T, addr, name string, key gossh.Signer) *e2eClient {
	t.Helper()
	return dial(t, addr, name, gossh.PublicKeys(key))
}

// waitForOutput scans everything the client has received, ANSI codes included.
func waitForOutput(t *testing.T, output *lockedBuffer, substring string) {
	t.Helper()
	deadline := time.Now().Add(e2eTimeout)
	for !strings.Contains(output.String(), substring) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in output:\n%q", substring, output.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (c *e2eClient) wait(substring string) {
	c.t.Helper()
	waitForOutput(c.t, c.output, substring)
}

func (c *e2eClient) waitEnded() {
	c.t.Helper()
	select {
	case <-c.ended:
	case <-time.After(e2eTimeout):
		c.t.Fatalf("session still open; output:\n%q", c.output.String())
	}
}

func (c *e2eClient) send(keys string) {
	c.t.Helper()
	if _, err := c.stdin.Write([]byte(keys)); err != nil {
		c.t.Fatal(err)
	}
}

// keysTo moves the cursor with arrow keys. Unlike letters, Bubble Tea reads
// several of these arriving at once as separate key presses.
func (c *e2eClient) keysTo(square string) string {
	to := sq(c.t, square)
	var keys strings.Builder
	for ; c.row < to.Row; c.row++ {
		keys.WriteString("\x1b[A")
	}
	for ; c.row > to.Row; c.row-- {
		keys.WriteString("\x1b[B")
	}
	for ; c.col < to.Col; c.col++ {
		keys.WriteString("\x1b[C")
	}
	for ; c.col > to.Col; c.col-- {
		keys.WriteString("\x1b[D")
	}
	return keys.String()
}

// move plays a move such as "e2e4" with the keyboard.
func (c *e2eClient) move(move string) {
	c.t.Helper()
	keys := c.keysTo(move[:2]) + "\r"
	keys += c.keysTo(move[2:]) + "\r"
	c.send(keys)
}

func lastMoveText(move string) string { return "Last move: " + move[:2] + " -> " + move[2:] }

// playBoth plays moves in turn, waiting after each for both clients to show it.
func playBoth(white, black *e2eClient, moves ...string) {
	white.t.Helper()
	for i, m := range moves {
		mover := white
		if i%2 == 1 {
			mover = black
		}
		mover.move(m)
		white.wait(lastMoveText(m))
		black.wait(lastMoveText(m))
	}
}

// startE2EGame connects two new keys and queues them, White first.
func startE2EGame(t *testing.T, s *e2eServer) (white, black *e2eClient, whiteKey, blackKey gossh.Signer) {
	t.Helper()
	whiteKey, blackKey = newSigner(t), newSigner(t)
	white = connect(t, s.addr, "alice", whiteKey)
	white.wait("My games")
	white.send("\r")
	waitFor(t, "alice in the queue", func() bool { return len(hubQueue(s.app.Hub)) == 1 })

	black = connect(t, s.addr, "bob", blackKey)
	black.wait("My games")
	black.send("\r")
	white.wait("You: alice (White) vs bob (Black)")
	black.wait("You: bob (Black) vs alice (White)")
	return white, black, whiteKey, blackKey
}

func gameIDOf(t *testing.T, s *e2eServer, key gossh.Signer) string {
	t.Helper()
	game, ok, err := GetActiveGameForPlayer(context.Background(), s.db, playerKey(key.PublicKey()))
	if err != nil || !ok {
		t.Fatalf("no active game: ok=%v err=%v", ok, err)
	}
	return game.GameID
}

func TestCleanNameRemovesEscapeCodes(t *testing.T) {
	if got := cleanName("bob\x1b[2J\r\n"); got != "bob[2J" {
		t.Fatalf("got %q", got)
	}
}

func TestE2EFullGameListedAndReplayed(t *testing.T) {
	s := startE2EServer(t, t.TempDir(), time.Minute)
	white, black, _, _ := startE2EGame(t, s)

	playBoth(white, black, "f2f3", "e7e5", "g2g4")
	black.move("d8h4")
	white.wait("Black wins")
	black.wait("Black wins")
}

func TestE2EKeylessClientGetsMessage(t *testing.T) {
	s := startE2EServer(t, t.TempDir(), time.Minute)
	noAnswers := func(string, string, []string, []bool) ([]string, error) { return nil, nil }
	c := dial(t, s.addr, "nokey", gossh.KeyboardInteractive(noAnswers))

	c.wait("CheSSH identifies players by SSH key, and your client didn't offer one.")
	c.wait("Create one with: ssh-keygen -t ed25519")
	c.waitEnded()
}

// unprovenKey claims a public key without holding its private half.
type unprovenKey struct{ key gossh.PublicKey }

func (u unprovenKey) PublicKey() gossh.PublicKey { return u.key }
func (u unprovenKey) Sign(io.Reader, []byte) (*gossh.Signature, error) {
	return nil, errors.New("no private key")
}

// Public keys are public. Offering someone's key and then logging in another
// way mustn't make you them.
func TestE2EOfferedKeyWithoutProofIsKeyless(t *testing.T) {
	s := startE2EServer(t, t.TempDir(), time.Minute)
	white, _, whiteKey, _ := startE2EGame(t, s)
	noAnswers := func(string, string, []string, []bool) ([]string, error) { return nil, nil }

	c := dial(t, s.addr, "mallory",
		gossh.PublicKeys(unprovenKey{whiteKey.PublicKey()}),
		gossh.KeyboardInteractive(noAnswers))

	c.wait("Create one with: ssh-keygen -t ed25519")
	c.waitEnded()
	if strings.Contains(white.output.String(), "connected elsewhere") {
		t.Fatal("an unproven key replaced the real player's connection")
	}
}

func TestE2EReconnectResumes(t *testing.T) {
	s := startE2EServer(t, t.TempDir(), time.Minute)
	white, black, _, blackKey := startE2EGame(t, s)
	playBoth(white, black, "e2e4")
	gameID := gameIDOf(t, s, blackKey)

	black.client.Close()
	white.wait("disconnected")

	black = connect(t, s.addr, "bob", blackKey)
	black.wait(lastMoveText("e2e4"))
	black.wait("a  b  c  d  e  f  g  h")
	waitFor(t, "timer to stop", func() bool {
		_, running := s.app.Hub.OpponentDeadline(gameID, playerKey(blackKey.PublicKey()))
		return !running
	})

	black.move("e7e5")
	white.wait(lastMoveText("e7e5"))
	black.wait(lastMoveText("e7e5"))
}

func TestE2ETimeoutResigns(t *testing.T) {
	s := startE2EServer(t, t.TempDir(), time.Second)
	white, black, _, _ := startE2EGame(t, s)
	playBoth(white, black, "e2e4")

	black.client.Close()
	white.wait("disconnected")
	white.wait("opponent left")
}

func TestE2EQuitCountsAsDisconnect(t *testing.T) {
	s := startE2EServer(t, t.TempDir(), time.Minute)
	white, black, _, _ := startE2EGame(t, s)

	// q ends the SSH session and leaves the connection open, as a client sharing
	// one connection between sessions would.
	black.send("q")
	black.waitEnded()
	white.wait("disconnected")
}

func TestE2ERestartResumes(t *testing.T) {
	dir := t.TempDir()
	s := startE2EServer(t, dir, time.Minute)
	white, black, whiteKey, blackKey := startE2EGame(t, s)
	playBoth(white, black, "f2f3", "e7e5")
	s.stop()

	s = startE2EServer(t, dir, time.Minute)
	white = connect(t, s.addr, "alice", whiteKey)
	black = connect(t, s.addr, "bob", blackKey)
	for _, c := range []*e2eClient{white, black} {
		c.wait(lastMoveText("e7e5"))
		c.wait("Turn: White")
	}
	white.move("g2g4")
	white.wait(lastMoveText("g2g4"))
	black.wait(lastMoveText("g2g4"))
}

func TestE2ESecondConnectionReplacesFirst(t *testing.T) {
	s := startE2EServer(t, t.TempDir(), time.Minute)
	first, black, whiteKey, _ := startE2EGame(t, s)

	second := connect(t, s.addr, "alice", whiteKey)
	first.wait("connected elsewhere")
	first.waitEnded()

	second.wait("You: alice (White) vs bob (Black)")
	playBoth(second, black, "e2e4")
}
