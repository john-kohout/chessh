package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/activeterm"
	"github.com/charmbracelet/wish/bubbletea"
	"github.com/charmbracelet/wish/logging"
	gossh "golang.org/x/crypto/ssh"
)

// App is the whole backend: the write side, the read side, and the Hub between
// them and the connected players.
type App struct {
	DB        *sql.DB
	Store     *EventStore
	Commands  *Commands
	Hub       *Hub
	Projector *Projector
}

// NewApp wires the parts together. Commands wake the Projector, and the
// Projector's changes reach players through the Hub. The Hub also needs
// Projector.CatchUp, so these closures see a Projector made after them.
func NewApp(db *sql.DB, timeout time.Duration) *App {
	store := NewEventStore(db)
	var projector *Projector
	commands := NewCommands(store, func() { projector.Wake() })
	hub := NewHub(db, commands, timeout, func(ctx context.Context) error { return projector.CatchUp(ctx) })
	projector = NewProjector(db, store, hub.Broadcast, ActiveGames{}, MyGames{})
	return &App{DB: db, Store: store, Commands: commands, Hub: hub, Projector: projector}
}

// Start gets projections and timers ready, then keeps projections up to date
// until ctx ends. Call it before accepting connections.
func (a *App) Start(ctx context.Context) error {
	if err := a.Projector.CatchUp(ctx); err != nil {
		return err
	}
	if err := a.Hub.RestoreTimers(ctx); err != nil {
		return err
	}
	go a.Projector.Run(ctx)
	return nil
}

// playerKey is a player's identity: their SSH public key's fingerprint.
func playerKey(key ssh.PublicKey) string {
	return gossh.FingerprintSHA256(key)
}

// NewSSHServer accepts any client, so that one without a key can be told why
// it can't play. The middleware runs from the last in the list to the first.
func NewSSHServer(addr string, hostKey []byte, app *App) (*ssh.Server, error) {
	return wish.NewServer(
		wish.WithAddress(addr),
		wish.WithHostKeyPEM(hostKey),
		wish.WithPublicKeyAuth(func(ssh.Context, ssh.PublicKey) bool { return true }),
		wish.WithKeyboardInteractiveAuth(func(ssh.Context, gossh.KeyboardInteractiveChallenge) bool { return true }),
		wish.WithMiddleware(
			playGame(app),
			activeterm.Middleware(),
			requireKey,
			logging.Middleware(),
		),
	)
}

func requireKey(next ssh.Handler) ssh.Handler {
	return func(s ssh.Session) {
		if s.PublicKey() == nil {
			wish.Println(s, "CheSSH identifies players by SSH key, and your client didn't offer one.")
			wish.Println(s, "Create one with: ssh-keygen -t ed25519")
			_ = s.Exit(1)
			return
		}
		next(s)
	}
}

// playGame runs one player's connection to the Hub around one Bubble Tea
// program. The program ends when the player quits, is replaced, or the SSH
// connection drops, so all three lead to the Disconnect below. It runs in a
// goroutine because it can take seconds, and the client shouldn't wait for it.
func playGame(app *App) wish.Middleware {
	return func(next ssh.Handler) ssh.Handler {
		return func(s ssh.Session) {
			ctx, cancel := context.WithCancel(s.Context())
			defer cancel()
			player := PlayerRef{Key: playerKey(s.PublicKey()), Name: cleanName(s.User())}
			session := app.Hub.Connect(ctx, player)

			runProgram := bubbletea.Middleware(func(ssh.Session) (tea.Model, []tea.ProgramOption) {
				return newModel(ctx, app, session), []tea.ProgramOption{tea.WithAltScreen()}
			})
			runProgram(next)(s)

			go app.Hub.Disconnect(context.Background(), session)
			select {
			case <-session.Closed:
				wish.Println(s, "This key connected elsewhere, so this session has ended.")
			default:
			}
		}
	}
}

// cleanName removes control characters from a username. Clients choose their
// own, and it's drawn in the opponent's terminal, so it can't carry escape codes.
func cleanName(name string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
}

func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".chessh", "chessh.db")
	}
	return filepath.Join(home, ".chessh", "chessh.db")
}

// generateOrLoadHostKey generates a new ED25519 host key or loads existing one
func generateOrLoadHostKey(keyPath string) ([]byte, error) {
	// Try to read existing key first
	if keyData, err := os.ReadFile(keyPath); err == nil {
		return keyData, nil
	}

	// Generate new key
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate ED25519 key: %w", err)
	}

	// Convert to PKCS#8 format
	pkcs8Key, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal private key: %w", err)
	}

	// Encode as PEM
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: pkcs8Key,
	})

	// Create directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		return nil, fmt.Errorf("failed to create key directory: %w", err)
	}

	// Save key to file
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return nil, fmt.Errorf("failed to save host key: %w", err)
	}

	log.Printf("Generated new SSH host key: %s", keyPath)
	return keyPEM, nil
}

func main() {
	var (
		sshPort = flag.Int("port", 2222, "SSH server port")
		local   = flag.Bool("local", false, "run in local mode (generates/uses local host key instead of Secret Manager)")
		dbPath  = flag.String("db", defaultDBPath(), "path to the SQLite database")
	)
	flag.Parse()

	var hostKeyData []byte
	var err error

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *local {
		// Local mode: generate or load local host key
		homeDir, err := os.UserHomeDir()
		if err != nil {
			log.Fatalf("failed to get user home directory: %v", err)
		}
		keyPath := filepath.Join(homeDir, ".chessh", "host_key")
		hostKeyData, err = generateOrLoadHostKey(keyPath)
		if err != nil {
			log.Fatalf("failed to generate/load host key: %v", err)
		}
		log.Println("Running in local mode")
	} else {
		// Cloud mode: use Secret Manager
		client, err := secretmanager.NewClient(ctx)
		if err != nil {
			log.Fatalf("failed to create Secret Manager client: %v", err)
		}
		defer client.Close()
		resp, err := client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
			Name: os.Getenv("SSH_HOST_KEY_SECRET"),
		})
		if err != nil {
			log.Fatalf("failed to access secret version: %v", err)
		}
		hostKeyData = resp.Payload.Data
		log.Println("Running in cloud mode with Secret Manager")
	}

	db, err := OpenDB(*dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	app := NewApp(db, 30*time.Second)
	if err := app.Start(ctx); err != nil {
		log.Fatalf("failed to start: %v", err)
	}

	s, err := NewSSHServer(fmt.Sprintf(":%d", *sshPort), hostKeyData, app)
	if err != nil {
		log.Fatalln(err)
	}
	go func() {
		log.Printf("Starting SSH chess server on :%d", *sshPort)
		if err = s.ListenAndServe(); err != nil {
			log.Fatalln(err)
		}
	}()

	if httpPort := os.Getenv("PORT"); httpPort != "" {
		log.Print("Starting HTTP health check server on port ", httpPort)
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		})
		if err := http.ListenAndServe(fmt.Sprintf(":%s", httpPort), nil); err != nil {
			log.Fatalln("HTTP server error:", err)
		}
	}

	<-ctx.Done()
	log.Println("Stopping SSH server")

	tctx, tcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer tcancel()
	if err := s.Shutdown(tctx); err != nil {
		log.Fatalln(err)
	}
}
