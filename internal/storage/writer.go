package storage

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"trevorblackman.dev/agent-board/internal/domain"
)

// ErrQueueFull is returned when the pending write queue is at capacity.
var ErrQueueFull = errors.New("write queue full")

// ErrWriterClosed is returned after shutdown began.
var ErrWriterClosed = errors.New("writer closed")

// Job runs inside one write transaction. Returning an error rolls back.
type Job func(ctx context.Context, tx *sql.Tx) error

type queued struct {
	ctx  context.Context
	job  Job
	done chan error
}

// Writer serializes all mutations through one goroutine and one connection.
type Writer struct {
	db      *sql.DB
	queue   chan queued
	closing atomic.Bool
	wg      sync.WaitGroup
	Depth   atomic.Int64
	// Metrics
	Committed atomic.Int64
	Failed    atomic.Int64
	WaitNanos atomic.Int64
}

func newWriter(db *sql.DB) *Writer {
	w := &Writer{db: db, queue: make(chan queued, domain.PendingWritesMax)}
	w.wg.Add(1)
	go w.loop()
	return w
}

func (w *Writer) loop() {
	defer w.wg.Done()
	for q := range w.queue {
		w.Depth.Add(-1)
		q.done <- w.run(q)
	}
}

func (w *Writer) run(q queued) error {
	if err := q.ctx.Err(); err != nil {
		return err
	}
	tx, err := w.db.BeginTx(q.ctx, nil)
	if err != nil {
		w.Failed.Add(1)
		return err
	}
	if err := q.job(q.ctx, tx); err != nil {
		tx.Rollback()
		w.Failed.Add(1)
		return err
	}
	if err := tx.Commit(); err != nil {
		w.Failed.Add(1)
		return err
	}
	w.Committed.Add(1)
	return nil
}

// Submit enqueues job and waits for its result. It never blocks on a full
// queue; callers translate ErrQueueFull to 429.
func (w *Writer) Submit(ctx context.Context, job Job) error {
	if w.closing.Load() {
		return ErrWriterClosed
	}
	q := queued{ctx: ctx, job: job, done: make(chan error, 1)}
	start := time.Now()
	select {
	case w.queue <- q:
		w.Depth.Add(1)
	default:
		return ErrQueueFull
	}
	err := <-q.done
	w.WaitNanos.Add(int64(time.Since(start)))
	return err
}

// Close stops accepting work and drains what was already accepted.
func (w *Writer) Close() {
	if w.closing.Swap(true) {
		return
	}
	close(w.queue)
	w.wg.Wait()
}
