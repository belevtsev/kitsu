package localrunner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/belevtsev/kitsu/v3/internal/workflowrunner"
)

const attemptName = "ATTEMPT0ATTEMPT0ATTEMPT0AB"

// asUser gives the runner the configuration and cache directories of a user
// of the test's own, with no signing key yet.
func asUser(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "AppData", "LocalAppData"} {
		t.Setenv(name, filepath.Join(home, name))
	}
}

func open(t *testing.T, root string) *Runner {
	t.Helper()
	r, err := Open(t.Context(), root, workflowrunner.Policy{Declaration: "declared"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestVerifyAcceptsOnlyAReceiptOfThisKeyWorktreeAndReservation(t *testing.T) {
	asUser(t)
	root := t.TempDir()
	r := open(t, root)
	signed := r.sign(r.receipt(attemptName, "binding", "job", "returned", ""))

	saved, err := Load(root)
	if err != nil || saved.Authority() != r.Authority() {
		t.Fatalf("a later command did not find the same runner: %v", err)
	}
	if got, err := saved.Verify(signed, attemptName, "binding"); err != nil || got.Profile != Profile || !got.Stopped {
		t.Fatalf("the runner refused its own receipt %+v: %v", got, err)
	}

	forged := signed
	forged.Receipt = []byte(strings.Replace(string(signed.Receipt), `"returned"`, `"failed"`, 1))
	elsewhere, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	asUser(t)
	stranger := open(t, root)
	for name, err := range map[string]error{
		"another binding":   second(r.Verify(signed, attemptName, "other")),
		"another attempt":   second(r.Verify(signed, attemptName+"C", "binding")),
		"changed bytes":     second(r.Verify(forged, attemptName, "binding")),
		"another worktree":  second(elsewhere.Verify(signed, attemptName, "binding")),
		"another user":      second(stranger.Verify(signed, attemptName, "binding")),
		"another authority": authorityDiffers(r, elsewhere, stranger),
	} {
		if err == nil {
			t.Errorf("a receipt for %s was accepted", name)
		}
	}
}

func second(_ workflowrunner.Receipt, err error) error { return err }

// authorityDiffers fails, as an accepted receipt does, when another worktree
// or user has the authority of r.
func authorityDiffers(r *Runner, others ...*Runner) error {
	for _, other := range others {
		if other.Authority() == r.Authority() {
			return nil
		}
	}
	return os.ErrInvalid
}

func TestAJobTheRunnerNoLongerHoldsIsReleasedWithItsReason(t *testing.T) {
	asUser(t)
	r := open(t, t.TempDir())
	_, lost, err := r.Resume(t.Context(), attemptName, "binding", "job")
	if err != nil {
		t.Fatal(err)
	}
	if lost.Status != "stopped" || !lost.Stopped || !lost.Release || lost.Job != "job" || !strings.Contains(string(lost.Result), "no longer holds this job") {
		t.Errorf("want a released stop that says why, got %+v with %s", lost, lost.Result)
	}
	if entries, _ := os.ReadDir(r.cache); len(entries) != 0 {
		t.Errorf("a job that never started left %d entries behind", len(entries))
	}

	job := workflowrunner.Job{Attempt: attemptName, Binding: "binding", Profile: Profile, Policy: "another policy", Kind: "verify"}
	if _, _, err := r.Run(t.Context(), job); err == nil {
		t.Error("a job built for another policy was accepted")
	}
}

// One command drives the run of a task at a time. A second one says that it
// waits, and goes on when the first has ended. The lock of a run that nobody
// drives is forgotten after a week, and one that is held stays.
func TestOneCommandDrivesARunAndASecondWaitsForIt(t *testing.T) {
	asUser(t)
	r := open(t, t.TempDir())
	first, err := r.Drive(t.Context(), "task", func() { t.Error("the first command waited") })
	if err != nil {
		t.Fatal(err)
	}
	other, err := r.Drive(t.Context(), "another task", func() { t.Error("a command for another task waited") })
	if err != nil {
		t.Fatal(err)
	}
	other()

	entries, err := os.ReadDir(r.cache)
	if err != nil || len(entries) != 2 {
		t.Fatalf("want the locks of two runs in the cache, got %v: %v", entries, err)
	}
	old := time.Now().Add(-2 * keep)
	for _, entry := range entries {
		if err := os.Chtimes(filepath.Join(r.cache, entry.Name()), old, old); err != nil {
			t.Fatal(err)
		}
	}
	r.prune()
	if left, _ := os.ReadDir(r.cache); len(left) != 1 {
		t.Fatalf("pruning kept %v, want the lock that is held and not the one that is free", left)
	}

	waiting, second := make(chan struct{}), make(chan func())
	go func() {
		release, _ := r.Drive(t.Context(), "task", func() { close(waiting) })
		second <- release
	}()
	select {
	case <-waiting:
	case release := <-second:
		release()
		t.Fatal("a second command drove the run while the first held it")
	}
	select {
	case <-second:
		t.Fatal("the second command went on before the first ended")
	case <-time.After(100 * time.Millisecond):
	}
	first()
	(<-second)()
}
