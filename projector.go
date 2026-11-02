package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// Projection builds one read table from events.
type Projection interface {
	Name() string // also the table name
	Create(tx *sql.Tx) error
	Apply(tx *sql.Tx, e Event) error
}

const createCheckpoints = `
CREATE TABLE IF NOT EXISTS projection_checkpoints (
  projection TEXT PRIMARY KEY,
  sequence   INTEGER NOT NULL
)`

const pollInterval = 250 * time.Millisecond

// Projector feeds new events to every projection, each in its own transaction.
type Projector struct {
	db          *sql.DB
	store       *EventStore
	onChange    func()
	projections []Projection
	wake        chan struct{}
	mu          sync.Mutex // one CatchUp at a time
}

func NewProjector(db *sql.DB, store *EventStore, onChange func(), projections ...Projection) *Projector {
	return &Projector{db: db, store: store, onChange: onChange, projections: projections, wake: make(chan struct{}, 1)}
}

// Wake asks Run to catch up now. It never blocks.
func (p *Projector) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Run catches up on Wake and every 250ms until ctx ends. A failed pass
// (for example SQLITE_BUSY) is logged and retried on the next one.
func (p *Projector) Run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if err := p.CatchUp(ctx); err != nil && ctx.Err() == nil {
			log.Printf("projector: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-ticker.C:
		}
	}
}

// CatchUp brings every projection up to date. One projection failing does not
// stop the others; the errors are returned together. onChange runs after commit.
func (p *Projector) CatchUp(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	var errs []error
	changed := false
	for _, projection := range p.projections {
		c, err := p.catchUpOne(ctx, projection)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		changed = changed || c
	}
	if changed && p.onChange != nil {
		p.onChange()
	}
	return errors.Join(errs...)
}

// catchUpOne is one transaction: rebuild if the table or checkpoint is
// missing, apply new events, save the checkpoint. It reports whether anything changed.
func (p *Projector) catchUpOne(ctx context.Context, projection Projection) (bool, error) {
	name := projection.Name()
	fail := func(err error) (bool, error) { return false, fmt.Errorf("%s: %w", name, err) }

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, createCheckpoints); err != nil {
		return fail(err)
	}
	changed := false

	var tables int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&tables); err != nil {
		return fail(err)
	}
	var checkpoint int64
	err = tx.QueryRowContext(ctx, `SELECT sequence FROM projection_checkpoints WHERE projection = ?`, name).Scan(&checkpoint)
	hasCheckpoint := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fail(err)
	}

	if tables == 0 || !hasCheckpoint {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+name); err != nil {
			return fail(err)
		}
		if err := projection.Create(tx); err != nil {
			return fail(err)
		}
		checkpoint, changed = 0, true
	}

	events, err := p.store.After(ctx, tx, checkpoint)
	if err != nil {
		return fail(err)
	}
	for _, e := range events {
		if err := projection.Apply(tx, e); err != nil {
			return false, fmt.Errorf("%s at sequence %d: %w", name, e.Sequence, err)
		}
		checkpoint, changed = e.Sequence, true
	}

	if changed {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO projection_checkpoints (projection, sequence) VALUES (?, ?)`, name, checkpoint); err != nil {
			return fail(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	return changed, nil
}
