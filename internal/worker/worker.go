// Package worker walks running sheets through their checklist, one step at a
// time, and records the outcome on the sheet.
package worker

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"listingboard/internal/board"
)

// MissingBrochure is the exact error text a sheet carries when its brochure
// cannot be read.
const MissingBrochure = "Brochure is missing"

// Store is the part of the persistence the worker needs. *store.Store
// satisfies it; the tests use a small in-memory fake.
type Store interface {
	RunningJobs(ctx context.Context) ([]board.Job, error)
	UpdateJob(ctx context.Context, job board.Job) error
}

// Run advances every running sheet by one step, waits pause, and repeats
// until ctx is cancelled. Errors are logged and the loop goes on; a lost
// database connection is retried on the next round.
func Run(ctx context.Context, st Store, dataDir string, pause time.Duration) {
	for {
		if _, err := RunOnce(ctx, st, dataDir); err != nil && ctx.Err() == nil {
			log.Printf("worker: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
	}
}

// RunOnce performs one step on each running sheet and reports how many sheets
// it touched. Every sheet is saved right after its step, so a crash between
// two steps loses nothing: the next run continues with the first pending step
// and the runId stays as it was.
func RunOnce(ctx context.Context, st Store, dataDir string) (int, error) {
	jobs, err := st.RunningJobs(ctx)
	if err != nil {
		return 0, err
	}
	var firstErr error
	n := 0
	for _, job := range jobs {
		if err := advance(ctx, st, dataDir, job); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		n++
	}
	return n, firstErr
}

// advance marks the first pending step done. A step that is already done is
// left alone. Saving the third step also moves the sheet to "success". If the
// brochure cannot be read, the sheet fails instead.
func advance(ctx context.Context, st Store, dataDir string, job board.Job) error {
	if job.Status != "running" {
		return nil
	}
	job.UpdatedAt = time.Now().UTC()
	if !readable(filepath.Join(dataDir, job.UserID, job.ID, "body")) {
		job.Status = "failed"
		job.Error = MissingBrochure
		return st.UpdateJob(ctx, job)
	}
	next := -1
	for i, step := range job.Checklist {
		if step.State == "pending" {
			next = i
			break
		}
	}
	if next >= 0 {
		job.Checklist[next].State = "done"
	}
	if next < 0 || next == len(job.Checklist)-1 {
		job.Status = "success"
	}
	return st.UpdateJob(ctx, job)
}

// readable reports whether the file can be opened and read.
func readable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var probe [1]byte
	_, err = f.Read(probe[:])
	return err == nil
}
