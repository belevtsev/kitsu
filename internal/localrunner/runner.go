// Package localrunner runs the jobs of a configured task on the developer's
// own machine: its checks, and the headless Codex or Claude Code agent of a
// role. A job runs under a detached supervisor, in a private copy of the
// candidate, and ends in a receipt signed with a key of this user.
//
// The receipt of a local job proves less than one of a registered runner
// service: the job ran as the developer, who also holds the key. Every local
// receipt names the profile "local", so that nothing reads it as more.
package localrunner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	"github.com/belevtsev/kitsu/v3/internal/fslock"
	"github.com/belevtsev/kitsu/v3/internal/proc"
	"github.com/belevtsev/kitsu/v3/internal/store"
	"github.com/belevtsev/kitsu/v3/internal/workflowrunner"
)

// Profile is the execution profile of every local job and receipt.
const Profile = "local"

// ErrLeft is the cause of a context whose command was terminated or lost its
// terminal. The job of that command goes on, for a later command to wait for.
var ErrLeft = errors.New("the command was stopped")

// The files of one job, in its own directory of the user's cache.
const (
	attemptFile = "attempt.json" // what to run; written once, before the supervisor starts
	filesFile   = "files.json"   // the candidate, until the job ends
	receiptFile = "receipt.json" // the signed result
	cancelFile  = "cancel"       // asks the supervisor to stop the job
	lockFile    = "lock"         // held by the supervisor for its whole life
	treeDir     = "tree"         // the private copy of the candidate, until the job ends

	poll = 200 * time.Millisecond
	keep = 7 * 24 * time.Hour
)

var attemptID = regexp.MustCompile(`^[A-Za-z0-9]{16,64}$`)

// Runner is the local runner of one worktree.
type Runner struct {
	project string
	key     ed25519.PrivateKey
	cache   string
	offer   workflowrunner.Capabilities
	limits  map[string]time.Duration
}

var _ workflowrunner.Runner = (*Runner)(nil)

// attempt is one job as its supervisor reads it. Key is the digest of the job
// that the task saved. The candidate's files are kept apart, in filesFile.
type attempt struct {
	Project string                      `json:"project"`
	Offer   workflowrunner.Capabilities `json:"offer"`
	Limits  map[string]time.Duration    `json:"limits,omitempty"`
	Key     string                      `json:"key"`
	Job     workflowrunner.Job          `json:"job"`
}

// Load opens the local runner of the worktree at root, to verify receipts it
// signed earlier.
func Load(root string) (*Runner, error) { return load(context.Background(), root, false) }

// Open opens it to run jobs under policy. limits holds the time each check of
// the policy may take; a check without one is bounded by its job alone.
func Open(ctx context.Context, root string, policy workflowrunner.Policy, limits map[string]time.Duration) (*Runner, error) {
	r, err := load(ctx, root, true)
	if err != nil {
		return nil, err
	}
	r.offer = workflowrunner.Capabilities{Platform: runtime.GOOS + "/" + runtime.GOARCH, Profile: Profile, Policy: policy}
	r.limits = limits
	r.prune()
	return r, nil
}

func load(ctx context.Context, root string, create bool) (*Runner, error) {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	config, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(config, "kitsu", "local-runner.key")
	seed, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && create {
		seed, err = newKey(ctx, path)
	}
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.Join(err, fmt.Errorf("the local runner has no usable signing key at %s", path))
	}
	return &Runner{project: root, key: ed25519.NewKeyFromSeed(seed), cache: filepath.Join(cache, "kitsu", "local-runner")}, nil
}

// newKey makes this user's signing key on the first local run. The lock makes
// two first runs at once agree on one key.
func newKey(ctx context.Context, path string) ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock, err := fslock.Acquire(ctx, path+".lock", fslock.Options{Wait: 10 * time.Second})
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Close() }()
	if seed, err := os.ReadFile(path); !errors.Is(err, fs.ErrNotExist) {
		return seed, err
	}
	seed := make([]byte, ed25519.SeedSize)
	_, _ = rand.Read(seed)
	return seed, store.WriteFile(path, seed, 0o600)
}

// Authority names this user's key and the worktree. A task pins it, so that
// evidence signed for another worktree or with another key does not count.
func (r *Runner) Authority() string {
	return workflowrunner.Digest([]any{Profile, r.project, []byte(r.key.Public().(ed25519.PublicKey))})
}

func (r *Runner) Offer() workflowrunner.Capabilities { return r.offer }

// Run keeps the job for its supervisor and waits for the result.
func (r *Runner) Run(ctx context.Context, job workflowrunner.Job) (workflowrunner.Signed, workflowrunner.Receipt, error) {
	dir, err := r.dir(job.Attempt)
	if err == nil && (job.Profile != Profile || job.Policy != workflowrunner.Digest(r.offer.Policy)) {
		err = errors.New("the job was built for another runner or policy")
	}
	if err != nil {
		return workflowrunner.Signed{}, workflowrunner.Receipt{}, err
	}
	key := workflowrunner.Digest(job)
	if _, err := os.Stat(filepath.Join(dir, attemptFile)); errors.Is(err, fs.ErrNotExist) {
		record := attempt{Project: r.project, Offer: r.offer, Limits: r.limits, Key: key, Job: job}
		record.Job.Files = nil
		if err = os.MkdirAll(dir, 0o700); err == nil {
			err = writeJSON(dir, filesFile, job.Files)
		}
		if err == nil { // The record comes last: a job with one is complete.
			err = writeJSON(dir, attemptFile, record)
		}
		if err != nil {
			return workflowrunner.Signed{}, workflowrunner.Receipt{}, err
		}
	}
	return r.Resume(ctx, job.Attempt, job.Binding, key)
}

// Resume waits for the result of a kept job. Unless the result is already
// there, it starts a supervisor first: one that finds the job taken ends
// soon, and one that finds it abandoned runs it again. When ctx ends, Resume
// asks the supervisor to stop, unless the cause is ErrLeft.
func (r *Runner) Resume(ctx context.Context, id, binding, job string) (workflowrunner.Signed, workflowrunner.Receipt, error) {
	dir, err := r.dir(id)
	if err != nil {
		return workflowrunner.Signed{}, workflowrunner.Receipt{}, err
	}
	if _, err := os.Stat(filepath.Join(dir, attemptFile)); errors.Is(err, fs.ErrNotExist) {
		got := r.receipt(id, binding, job, "stopped", "the local runner no longer holds this job")
		return r.sign(got), got, nil
	}
	var exited <-chan struct{} // stays nil for a job whose result is already kept
	if _, err := os.Stat(filepath.Join(dir, receiptFile)); err != nil {
		if exited, err = start(dir); err != nil {
			return workflowrunner.Signed{}, workflowrunner.Receipt{}, fmt.Errorf("start the supervisor of the job: %w", err)
		}
	}
	for alive := true; ; {
		signed, got, err := r.Status(ctx, id, binding)
		switch {
		case err != nil:
			return signed, got, err
		case got.Job != job:
			return signed, got, errors.New("the kept result names other job inputs")
		case got.Status != "running":
			// The supervisor removes the private copy before it ends.
			for range 10 {
				if !busy(dir) {
					break
				}
				time.Sleep(poll)
			}
			return signed, got, nil
		case !alive:
			return signed, got, errors.New("the local job ended without a result")
		}
		select {
		case <-ctx.Done():
			if errors.Is(context.Cause(ctx), ErrLeft) {
				return workflowrunner.Signed{}, workflowrunner.Receipt{}, ErrLeft
			}
			_ = os.WriteFile(filepath.Join(dir, cancelFile), nil, 0o600)
			return workflowrunner.Signed{}, workflowrunner.Receipt{}, errors.New("this command ended before the job did, and asked the job to stop")
		case <-exited:
			// The supervisor this call started is gone. Either the result is
			// there, or an earlier supervisor still has the job.
			exited, alive = nil, busy(dir)
		case <-time.After(poll):
			if exited == nil {
				alive = busy(dir)
			}
		}
	}
}

// Status returns the kept result of a job, or a receipt that says it runs.
func (r *Runner) Status(_ context.Context, id, binding string) (workflowrunner.Signed, workflowrunner.Receipt, error) {
	var signed workflowrunner.Signed
	dir, err := r.dir(id)
	if err == nil {
		err = readJSON(dir, receiptFile, &signed)
	}
	if errors.Is(err, fs.ErrNotExist) {
		var a attempt
		if err = readJSON(dir, attemptFile, &a); err == nil {
			signed = r.sign(r.receipt(id, a.Job.Binding, a.Key, "running", ""))
		}
	}
	if err != nil {
		return signed, workflowrunner.Receipt{}, fmt.Errorf("read local job %s: %w", id, err)
	}
	got, err := r.Verify(signed, id, binding)
	return signed, got, err
}

// Verify accepts a receipt that this user's key signed for this worktree and
// reservation. With a policy open, the receipt must also be of that policy.
func (r *Runner) Verify(signed workflowrunner.Signed, id, binding string) (workflowrunner.Receipt, error) {
	var got workflowrunner.Receipt
	if !ed25519.Verify(r.key.Public().(ed25519.PublicKey), signed.Receipt, signed.Signature) {
		return got, errors.New("the receipt was not signed by this user's local runner")
	}
	if err := strict(signed.Receipt, &got); err != nil {
		return got, err
	}
	if got.Project != r.project || got.Attempt != id || got.Binding != binding || got.Profile != Profile ||
		r.offer.Profile != "" && (got.Platform != r.offer.Platform || got.Policy != workflowrunner.Digest(r.offer.Policy)) {
		return got, errors.New("the local receipt does not match the saved reservation and policy")
	}
	return got, nil
}

// receipt describes a job in the given state. A job that ended without a
// result says why in reason, and may be retried at once: the developer is the
// operator of this runner.
func (r *Runner) receipt(id, binding, job, status, reason string) workflowrunner.Receipt {
	got := workflowrunner.Receipt{Project: r.project, Platform: r.offer.Platform, Profile: Profile,
		Policy: workflowrunner.Digest(r.offer.Policy), Attempt: id, Binding: binding, Job: job,
		Status: status, Stopped: status != "running"}
	if reason != "" {
		got.Release = true
		got.Result, _ = json.Marshal(map[string]string{"error": reason})
	}
	return got
}

func (r *Runner) sign(got workflowrunner.Receipt) workflowrunner.Signed {
	data, _ := json.Marshal(got)
	return workflowrunner.Signed{Receipt: data, Signature: ed25519.Sign(r.key, data)}
}

func (r *Runner) dir(id string) (string, error) {
	if !attemptID.MatchString(id) {
		return "", errors.New("the local runner needs an attempt ID of letters and digits")
	}
	return filepath.Join(r.cache, id), nil
}

// Drive makes this command the only one that drives the run of a task in
// this worktree. A second command calls waiting and stays here until the
// first has ended; it then finds the run where the first left it. The
// returned function lets the next command in. After an error no lock is held.
func (r *Runner) Drive(ctx context.Context, task string, waiting func()) (func(), error) {
	dir := filepath.Join(r.cache, "run-"+workflowrunner.Digest([]any{r.project, task})[:16])
	for wait := fslock.NoWait; ; wait = 0 {
		_ = os.MkdirAll(dir, 0o700)
		lock, err := fslock.Acquire(ctx, filepath.Join(dir, lockFile), fslock.Options{Wait: wait})
		switch {
		case err == nil:
			return func() { _ = lock.Close() }, nil
		case errors.Is(err, fslock.ErrBusy):
			waiting()
		case !errors.Is(err, fslock.ErrReplaced): // a pruned lock is taken again
			return func() {}, err
		}
	}
}

// prune forgets the jobs that ended more than a week ago. A task keeps every
// receipt it relies on in its own ledger.
func (r *Runner) prune() {
	entries, _ := os.ReadDir(r.cache)
	for _, entry := range entries {
		dir := filepath.Join(r.cache, entry.Name())
		if info, err := entry.Info(); err != nil || time.Since(info.ModTime()) <= keep {
			continue
		}
		// The folder goes while its lock is held: a command that waited for
		// the lock then finds it replaced and takes a new one.
		lock, err := fslock.Acquire(context.Background(), filepath.Join(dir, lockFile), fslock.Options{Wait: fslock.NoWait})
		if !errors.Is(err, fslock.ErrBusy) {
			_ = os.RemoveAll(dir)
		}
		if err == nil {
			_ = lock.Close()
			_ = os.Remove(dir) // Windows keeps a folder until the last file in it is closed
		}
	}
}

// busy says whether a supervisor holds the job in dir.
func busy(dir string) bool {
	lock, err := fslock.Acquire(context.Background(), filepath.Join(dir, lockFile), fslock.Options{Wait: fslock.NoWait})
	if err == nil {
		_ = lock.Close()
	}
	return errors.Is(err, fslock.ErrBusy)
}

// start launches the supervisor of the job in dir with this program, detached
// from this command: it outlives the command, and a terminal's interrupt does
// not reach it. The returned channel closes when that supervisor ends.
func start(dir string) (<-chan struct{}, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	log, err := os.OpenFile(filepath.Join(dir, "supervisor.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = log.Close() }()
	return proc.Detach(proc.Spec{Path: self, Args: []string{"task", "run-job", dir}, Dir: dir, Env: os.Environ(), Stdout: log, Stderr: log})
}

func writeJSON(dir, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return store.WriteFile(filepath.Join(dir, name), data, 0o600)
}

func readJSON(dir, name string, into any) error {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	return strict(data, into)
}

func strict(data []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("a record of the local runner must hold one JSON value")
	}
	return nil
}
