package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingProjection adds one row per event. failOn makes Apply fail for that sequence.
type countingProjection struct {
	name   string
	failOn atomic.Int64
}

func (c *countingProjection) Name() string { return c.name }

func (c *countingProjection) Create(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE ` + c.name + ` (sequence INTEGER PRIMARY KEY)`)
	return err
}

func (c *countingProjection) Apply(tx *sql.Tx, e Event) error {
	if e.Sequence == c.failOn.Load() {
		return errors.New("boom")
	}
	_, err := tx.Exec(`INSERT INTO `+c.name+` (sequence) VALUES (?)`, e.Sequence)
	return err
}

func appendGame(t *testing.T, store *EventStore, gameID string) {
	t.Helper()
	a := NewGameAggregate(gameID)
	events, err := a.Start(alice, bob)
	if err != nil {
		t.Fatal(err)
	}
	applyAll(t, a, events)
	events = append(events, playMoves(t, a, "e2e4", "e7e5")...)
	if err := store.Append(context.Background(), events); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func checkpoint(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT sequence FROM projection_checkpoints WHERE projection = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func counter(onChange *atomic.Int32) func() { return func() { onChange.Add(1) } }

func TestCatchUpProcessesAllEvents(t *testing.T) {
	db := testDB(t)
	store := NewEventStore(db)
	appendGame(t, store, "g1")
	p := NewProjector(db, store, nil, &countingProjection{name: "counted"})
	if err := p.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, db, "counted"); got != 3 {
		t.Fatalf("rows = %d, want 3", got)
	}
	if got := checkpoint(t, db, "counted"); got != 3 {
		t.Fatalf("checkpoint = %d, want 3", got)
	}
}

func TestCatchUpIsIncremental(t *testing.T) {
	db := testDB(t)
	store := NewEventStore(db)
	appendGame(t, store, "g1")
	var changes atomic.Int32
	p := NewProjector(db, store, counter(&changes), &countingProjection{name: "counted"})
	ctx := context.Background()
	if err := p.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if changes.Load() != 1 {
		t.Fatalf("onChange calls = %d, want 1", changes.Load())
	}
	if err := p.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, db, "counted"); got != 3 {
		t.Fatalf("rows = %d, want 3", got)
	}
	if changes.Load() != 1 {
		t.Fatalf("onChange called again with nothing new: %d", changes.Load())
	}
}

func TestCrashRecoveryCatchesUp(t *testing.T) {
	db := testDB(t)
	store := NewEventStore(db)
	ctx := context.Background()
	appendGame(t, store, "g1")
	first := NewProjector(db, store, nil, &countingProjection{name: "counted"})
	if err := first.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	appendGame(t, store, "g2") // written; the projector "died" before seeing it
	second := NewProjector(db, store, nil, &countingProjection{name: "counted"})
	if err := second.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, db, "counted"); got != 6 {
		t.Fatalf("rows = %d, want 6", got)
	}
}

func TestFailedApplyRollsBackAndRetries(t *testing.T) {
	db := testDB(t)
	store := NewEventStore(db)
	proj := &countingProjection{name: "counted"}
	p := NewProjector(db, store, nil, proj)
	ctx := context.Background()
	appendGame(t, store, "g1") // sequences 1-3
	if err := p.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	appendGame(t, store, "g2") // sequences 4-6
	proj.failOn.Store(5)
	if err := p.CatchUp(ctx); err == nil {
		t.Fatal("expected an error")
	}
	if got := countRows(t, db, "counted"); got != 3 {
		t.Fatalf("rows = %d after failed pass, want 3 (event 4 rolled back)", got)
	}
	if got := checkpoint(t, db, "counted"); got != 3 {
		t.Fatalf("checkpoint moved to %d", got)
	}
	proj.failOn.Store(0)
	if err := p.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, db, "counted"); got != 6 {
		t.Fatalf("rows = %d, want 6 with no duplicates", got)
	}
}

func TestFailingProjectionDoesNotBlockOthers(t *testing.T) {
	db := testDB(t)
	store := NewEventStore(db)
	appendGame(t, store, "g1")
	bad := &countingProjection{name: "bad"}
	bad.failOn.Store(1)
	var changes atomic.Int32
	p := NewProjector(db, store, counter(&changes), bad, &countingProjection{name: "good"})
	if err := p.CatchUp(context.Background()); err == nil {
		t.Fatal("expected an error from the bad projection")
	}
	if got := countRows(t, db, "good"); got != 3 {
		t.Fatalf("good rows = %d, want 3", got)
	}
	if changes.Load() != 1 {
		t.Fatalf("onChange calls = %d, want 1", changes.Load())
	}
}

func TestNewProjectionProcessesExistingHistory(t *testing.T) {
	db := testDB(t)
	store := NewEventStore(db)
	ctx := context.Background()
	appendGame(t, store, "g1")
	if err := NewProjector(db, store, nil, &countingProjection{name: "one"}).CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	p := NewProjector(db, store, nil, &countingProjection{name: "one"}, &countingProjection{name: "two"})
	if err := p.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, db, "two"); got != 3 {
		t.Fatalf("new projection rows = %d, want 3", got)
	}
	if got := countRows(t, db, "one"); got != 3 {
		t.Fatalf("old projection rows = %d, want 3", got)
	}
}

func TestDropTableRebuilds(t *testing.T) {
	db := testDB(t)
	store := NewEventStore(db)
	ctx := context.Background()
	appendGame(t, store, "g1")
	var changes atomic.Int32
	p := NewProjector(db, store, counter(&changes), &countingProjection{name: "counted"})
	if err := p.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	before := changes.Load()
	if _, err := db.Exec(`DROP TABLE counted`); err != nil {
		t.Fatal(err)
	}
	if err := p.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, db, "counted"); got != 3 {
		t.Fatalf("rows = %d, want 3", got)
	}
	if changes.Load() != before+1 {
		t.Fatal("onChange not called after rebuild")
	}
}

func TestDeleteCheckpointRebuildsWithoutDuplicates(t *testing.T) {
	db := testDB(t)
	store := NewEventStore(db)
	ctx := context.Background()
	appendGame(t, store, "g1")
	p := NewProjector(db, store, nil, &countingProjection{name: "counted"})
	if err := p.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM projection_checkpoints`); err != nil {
		t.Fatal(err)
	}
	if err := p.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, db, "counted"); got != 3 {
		t.Fatalf("rows = %d, want 3", got)
	}
}

func TestDropBothRebuildsEvenWithNoEvents(t *testing.T) {
	db := testDB(t)
	store := NewEventStore(db)
	ctx := context.Background()
	var changes atomic.Int32
	p := NewProjector(db, store, counter(&changes), &countingProjection{name: "counted"})
	if err := p.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	before := changes.Load()
	if _, err := db.Exec(`DROP TABLE counted`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM projection_checkpoints`); err != nil {
		t.Fatal(err)
	}
	if err := p.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if changes.Load() != before+1 {
		t.Fatal("onChange not called after rebuild with no events")
	}
	if got := countRows(t, db, "counted"); got != 0 {
		t.Fatalf("rows = %d, want 0", got)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunWakesAndStops(t *testing.T) {
	db := testDB(t)
	store := NewEventStore(db)
	p := NewProjector(db, store, nil, &countingProjection{name: "counted"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	appendGame(t, store, "g1")
	p.Wake()
	waitFor(t, "projection after Wake", func() bool {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM counted`).Scan(&n)
		return n == 3
	})
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}

// An outside sqlite3 process holding the write lock makes a pass fail with
// SQLITE_BUSY. Run must keep going and succeed on a later pass.
func TestRunSurvivesBusyDatabase(t *testing.T) {
	db := testDB(t)
	store := NewEventStore(db)
	appendGame(t, store, "g1")
	if _, err := db.Exec(`PRAGMA busy_timeout = 20`); err != nil {
		t.Fatal(err)
	}
	var path string
	var seq int
	var name string
	if err := db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	other, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	lock, err := other.Begin() // holds the write lock like an external writer
	if err != nil {
		t.Fatal(err)
	}

	p := NewProjector(db, store, nil, &countingProjection{name: "counted"})
	err = p.CatchUp(context.Background())
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "locked") {
		t.Fatalf("expected a busy error, got %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	time.Sleep(300 * time.Millisecond) // at least one failed pass inside Run
	lock.Rollback()
	waitFor(t, "a successful pass after the lock is released", func() bool {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM counted`).Scan(&n)
		return n == 3
	})
}
