package worker

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"listingboard/internal/board"
)

func TestRunOnceMarksOnlyTheFirstPendingStep(t *testing.T) {
	dir := t.TempDir()
	job := sheet(t, dir, "running", 0, true)
	st := newFake(job)

	n, err := RunOnce(context.Background(), st, dir)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v, want 1 and nil", n, err)
	}
	got := st.jobs[job.ID]
	wantStates(t, got, "done", "pending", "pending")
	if got.Status != "running" {
		t.Fatalf("status %s, want running", got.Status)
	}
	if got.RunID != job.RunID {
		t.Fatal("runId changed")
	}
}

func TestRunOnceContinuesWhereTheLastRunStopped(t *testing.T) {
	dir := t.TempDir()
	job := sheet(t, dir, "running", 1, true) // first step already done, as after a crash
	st := newFake(job)

	if _, err := RunOnce(context.Background(), st, dir); err != nil {
		t.Fatal(err)
	}
	got := st.jobs[job.ID]
	wantStates(t, got, "done", "done", "pending")
	if got.Status != "running" || got.RunID != job.RunID {
		t.Fatalf("status %s runId changed=%v", got.Status, got.RunID != job.RunID)
	}
}

func TestSavingTheThirdStepMakesTheSheetSuccessful(t *testing.T) {
	dir := t.TempDir()
	job := sheet(t, dir, "running", 2, true)
	st := newFake(job)

	if _, err := RunOnce(context.Background(), st, dir); err != nil {
		t.Fatal(err)
	}
	got := st.jobs[job.ID]
	wantStates(t, got, "done", "done", "done")
	if got.Status != "success" {
		t.Fatalf("status %s, want success", got.Status)
	}
	if got.Error != "" {
		t.Fatalf("error %q, want empty", got.Error)
	}
}

func TestAMissingBrochureFailsTheSheet(t *testing.T) {
	dir := t.TempDir()
	job := sheet(t, dir, "running", 0, false)
	st := newFake(job)

	if _, err := RunOnce(context.Background(), st, dir); err != nil {
		t.Fatal(err)
	}
	got := st.jobs[job.ID]
	if got.Status != "failed" {
		t.Fatalf("status %s, want failed", got.Status)
	}
	if got.Error != "Brochure is missing" {
		t.Fatalf("error %q, want %q", got.Error, "Brochure is missing")
	}
	wantStates(t, got, "pending", "pending", "pending")
}

func TestRunOnceLeavesSheetsAloneThatAreNotRunning(t *testing.T) {
	dir := t.TempDir()
	draft := sheet(t, dir, "draft", 0, true)
	done := sheet(t, dir, "success", 3, true)
	failed := sheet(t, dir, "failed", 0, false)
	st := newFake(draft, done, failed)

	n, err := RunOnce(context.Background(), st, dir)
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v, want 0 and nil", n, err)
	}
	for _, before := range []board.Job{draft, done, failed} {
		after := st.jobs[before.ID]
		if after.Status != before.Status || after.Checklist[0].State != before.Checklist[0].State {
			t.Fatalf("sheet %s changed from %s to %s", before.ID, before.Status, after.Status)
		}
	}
}

func TestEveryRunningSheetAdvancesOneStepPerRound(t *testing.T) {
	dir := t.TempDir()
	a := sheet(t, dir, "running", 0, true)
	b := sheet(t, dir, "running", 1, true)
	st := newFake(a, b)

	n, err := RunOnce(context.Background(), st, dir)
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v, want 2 and nil", n, err)
	}
	wantStates(t, st.jobs[a.ID], "done", "pending", "pending")
	wantStates(t, st.jobs[b.ID], "done", "done", "pending")
}

func TestRunStopsWhenTheContextIsCancelled(t *testing.T) {
	dir := t.TempDir()
	st := newFake()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		Run(ctx, st, dir, 5*time.Millisecond)
		close(stopped)
	}()
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// sheet builds a job with the first `done` steps marked done and, if asked,
// a brochure at {dir}/{userID}/{jobID}/body.
func sheet(t *testing.T, dir, status string, done int, withBody bool) board.Job {
	t.Helper()
	now := time.Now().UTC()
	job := board.Job{
		ID:        board.NewID(),
		UserID:    "user-" + board.NewID(),
		Title:     "Market loft",
		Address:   "10 Market Street",
		Status:    status,
		RunID:     board.NewID(),
		Filename:  "brochure.pdf",
		Checklist: board.NewChecklist(),
		CreatedAt: now,
		UpdatedAt: now,
	}
	for i := 0; i < done; i++ {
		job.Checklist[i].State = "done"
	}
	if withBody {
		path := filepath.Join(dir, job.UserID, job.ID, "body")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("%PDF-1.4\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return job
}

func wantStates(t *testing.T, job board.Job, states ...string) {
	t.Helper()
	for i, want := range states {
		if got := job.Checklist[i].State; got != want {
			t.Fatalf("step %d (%s) is %s, want %s", i+1, job.Checklist[i].Label, got, want)
		}
	}
}

type fake struct {
	jobs map[string]board.Job
}

func newFake(jobs ...board.Job) *fake {
	f := &fake{jobs: map[string]board.Job{}}
	for _, job := range jobs {
		f.jobs[job.ID] = clone(job)
	}
	return f
}

func (f *fake) RunningJobs(_ context.Context) ([]board.Job, error) {
	var out []board.Job
	for _, job := range f.jobs {
		if job.Status == "running" {
			out = append(out, clone(job))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fake) UpdateJob(_ context.Context, job board.Job) error {
	if _, ok := f.jobs[job.ID]; !ok {
		return board.ErrNotFound
	}
	f.jobs[job.ID] = clone(job)
	return nil
}

func clone(job board.Job) board.Job {
	job.Checklist = append([]board.Step(nil), job.Checklist...)
	return job
}
