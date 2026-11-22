package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/belevtsev/kitsu/v3/internal/decl"
	"github.com/belevtsev/kitsu/v3/internal/harness"
	"github.com/belevtsev/kitsu/v3/internal/localrunner"
	"github.com/belevtsev/kitsu/v3/internal/onboarding"
	"github.com/belevtsev/kitsu/v3/internal/roles"
	"github.com/belevtsev/kitsu/v3/internal/workflowrunner"
)

// localJobCommand is the supervisor that a local run starts for each of its
// jobs. A person has no use for it.
func localJobCommand() uxCommand {
	return uxCommand{path: "task run-job", args: "<directory>", hidden: true,
		summary: "Run one kept job of a local task run; kitsu task run --local starts this itself",
		run: func(ctx context.Context, _ Config, args []string) error {
			if len(args) != 1 {
				return usagef("task run-job takes the directory of one kept job")
			}
			return localrunner.Supervise(ctx, args[0], func(tree string, check workflowrunner.Check) (string, error) {
				return checkExecutable(tree, decl.Check{ID: check.ID, Run: check.Argv})
			})
		}}
}

// SignalContext is the context of the program. It ends on Ctrl+C, when the
// program is terminated and when its terminal closes. Ctrl+C stops the job of
// a local run. After the other two the job goes on: a chat's shell terminates
// a command that takes long, and the chat then waits with --resume.
func SignalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(context.Background())
	received := make(chan os.Signal, 1)
	signal.Notify(received, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		if <-received == os.Interrupt {
			cancel(nil)
		}
		cancel(localrunner.ErrLeft)
	}()
	return ctx, func() { signal.Stop(received); cancel(nil) }
}

// openWorkflowRunner picks the runner of a run: the one a saved task pinned,
// the local one for --local, and otherwise the registered service.
func openWorkflowRunner(ctx context.Context, cfg Config, s *session, o workflowOptions) (workflowrunner.Runner, error) {
	local, pinned := o.local, ""
	var unread error // why the saved task could not be read
	if o.resume != "" {
		l, err := readOnlyStageLedger(s.repo.GitDir)
		if err == nil {
			var task *harness.Task
			if task, err = readWorkflow(l, o.id); err == nil {
				local, pinned = task.Workflow.Profile == localrunner.Profile, task.RepositoryPolicy
			}
			l.close()
		}
		unread = err
	}
	switch {
	case local && o.resume == "" && (o.scope.Entry == "plan" || o.scope.Entry == "tests"):
		return nil, errors.New("a local run starts no architect and no test writer yet. Leave --entry out to start at the developer, or enter at review or verify")
	case local && os.Getenv("CODEX_SANDBOX")+os.Getenv("CODEX_SANDBOX_NETWORK_DISABLED") != "":
		// Codex sets these for the commands of a chat. There Kitsu cannot
		// write the records of a task, and an agent cannot sign in.
		return nil, errors.New("this command runs in the sandbox of a Codex chat, where a local run cannot write its records or start an agent. Run the same command in your own terminal")
	case local:
		return openLocalRunner(ctx, s, pinned)
	case cfg.workflowRunner != nil:
		return cfg.workflowRunner(ctx, s.repo.Root)
	}
	client, err := workflowrunner.Open(ctx, s.repo.Root)
	if errors.Is(err, workflowrunner.ErrUnregistered) && unread != nil {
		// With a runner service, the run itself reports the task. Without one,
		// "register a runner" would hide that the task is the problem.
		if errors.Is(unread, fs.ErrNotExist) {
			return nil, fmt.Errorf("there is no saved task %s", o.id)
		}
		return nil, fmt.Errorf("task %s cannot be resumed: %w", o.id, unread)
	}
	if err != nil {
		return nil, err
	}
	if client.Profile == localrunner.Profile {
		return nil, errors.New("a registered runner cannot use the profile name local")
	}
	return client, nil
}

// openLocalRunner builds the policy of a local run: the checks of kitsu.toml
// that the developer approved with kitsu check trust. pinned is the
// declaration a saved task started with.
func openLocalRunner(ctx context.Context, s *session, pinned string) (workflowrunner.Runner, error) {
	if err := localRunnerAllowed(s.repo.Root); err != nil {
		return nil, err
	}
	file, fingerprint, err := workingCheckPolicy(ctx, s.repo)
	if err != nil {
		return nil, err
	}
	if pinned != "" && pinned != fingerprint {
		return nil, errors.New("kitsu.toml changed since this task started. Restore it, or open a new task")
	}
	if file == nil || len(file.Checks) == 0 {
		return nil, errors.New("the checks of kitsu.toml gate a local run, and it declares none. Add one with kitsu check add")
	}
	if approval, err := trustFile(s.repo, ""); err != nil || string(approval.data) != fingerprint+"\n" {
		return nil, errors.New("approve the checks of kitsu.toml with kitsu check trust in your own terminal first")
	}
	config, err := roles.Load(s.repo.Root) // for its test paths: the checks alone need no host
	if err != nil {
		return nil, err
	}
	policy := workflowrunner.Policy{Declaration: fingerprint, TestPaths: config.Tests}
	limits := map[string]time.Duration{}
	for _, check := range file.Checks {
		policy.Checks = append(policy.Checks, workflowrunner.Check{ID: check.ID, Argv: check.Run})
		limits[check.ID] = check.Timeout
	}
	return localrunner.Open(ctx, s.repo.Root, policy, limits)
}

// savedWorkflowRunner is the runner that signed the evidence of a saved task.
func savedWorkflowRunner(root string, w *harness.Workflow) (workflowrunner.Runner, error) {
	if w.Profile != localrunner.Profile {
		return workflowrunner.Load(root)
	}
	if err := localRunnerAllowed(root); err != nil {
		return nil, err
	}
	return localrunner.Load(root)
}

// registeredRunner finds the runner service of a worktree. Tests replace it.
var registeredRunner = workflowrunner.Load

// localRunnerAllowed refuses the local runner wherever an administrator
// registered a runner service for the worktree, and wherever that cannot be
// told. A task of the service must never count on local evidence, which the
// user who made the change can sign: its record names its runner, and that
// record is a file in the checkout.
func localRunnerAllowed(root string) error {
	if _, err := registeredRunner(root); !errors.Is(err, workflowrunner.ErrUnregistered) {
		return errors.New("a runner service is registered for this worktree, or its registration cannot be read. The local runner is not available here: leave --local out")
	}
	return nil
}

// workflowStopReason is why a job ended without a result, when its runner
// says so. The local runner does.
func workflowStopReason(got workflowrunner.Receipt) string {
	var result struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(got.Result, &result) != nil || result.Error == "" {
		return ""
	}
	return ": " + result.Error
}

// localDefaults are the defaults of a new run on this machine. It starts at
// the developer and ends after an independent review, --commit delivers, and
// at most three reviews start. A run that enters at review or verify starts
// no agent that its flags did not ask for.
func (o *workflowOptions) localDefaults(f *flag.FlagSet) {
	if !o.local {
		return
	}
	set := map[string]bool{}
	f.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !set["entry"] {
		o.scope.Entry = "implement"
	}
	if !set["until"] && o.scope.Commit {
		o.scope.Until = "deliver"
	}
	if !set["require-review"] {
		o.scope.RequireReview = o.scope.Entry == "implement" && o.scope.Until != "plan" && o.scope.Until != "implement"
	}
	if o.reviews == 0 {
		o.reviews = 3
	}
}

// workflowRoleConfig is the role assignment of a new run. A local run in a
// repository without a roles file needs none: see localHosts.
func workflowRoleConfig(root string, local bool) (*roles.Config, error) {
	if _, err := os.Lstat(filepath.Join(root, roles.FileName)); !local || !errors.Is(err, fs.ErrNotExist) {
		return dailyRoleConfig(root)
	}
	var installed []string
	for _, host := range roles.Hosts {
		if _, native := onboarding.HostExecutable(host); native {
			installed = append(installed, host)
		}
	}
	preferred, _ := uxInstalledHost()
	developer, reviewer := localHosts(preferred, installed)
	if developer == "" {
		return dailyRoleConfig(root) // A run of the checks alone starts none.
	}
	assigned := func(host string) []byte { return fmt.Appendf(nil, "[roles.reviewer]\nhost = %q", host) }
	return roles.LoadWithWorkflowDefault(root, bytes.Replace(roles.WorkflowTemplate(developer), assigned(developer), assigned(reviewer), 1))
}

// localHosts picks the hosts of a local run without a roles file. The
// developer works in the host Kitsu was installed for, or else in the first
// installed one. The reviewer works in the other host where that is
// installed, so that a second model reads the change.
func localHosts(preferred string, installed []string) (developer, reviewer string) {
	if len(installed) == 0 {
		return "", ""
	}
	developer = installed[0]
	if slices.Contains(installed, preferred) {
		developer = preferred
	}
	reviewer = developer
	for _, host := range installed {
		if host != developer {
			reviewer = host
		}
	}
	return developer, reviewer
}

// savedWorkflow reads a saved task for a line of output, or returns nil.
func savedWorkflow(s *session, id string) *harness.Task {
	l, err := readOnlyStageLedger(s.repo.GitDir)
	if err != nil {
		return nil
	}
	defer l.close()
	task, _ := readWorkflow(l, id)
	return task
}

// driveWorkflow makes this command the only one that drives the local run of
// a task. A second command waits here until the first has ended, and then
// goes on from where the run stands: a chat starts one while its shell has
// left the first running. The returned function lets the next command in,
// and tells a command that was stopped how the run goes on.
func driveWorkflow(ctx context.Context, s *session, o workflowOptions, result *error) func() {
	r, local := s.workflowRunner.(*localrunner.Runner)
	if !local {
		return func() {}
	}
	release, err := r.Drive(ctx, o.id, func() {
		fmt.Fprintf(s.cfg.Err, "Kitsu task %s: another command runs this task. Waiting for it to end.\n", o.id)
	})
	if err != nil && ctx.Err() == nil {
		fmt.Fprintf(s.cfg.Err, "kitsu: this run has no lock (%v). Start no second command for task %s.\n", err, o.id)
	}
	return func() {
		release()
		if *result != nil && errors.Is(context.Cause(ctx), localrunner.ErrLeft) {
			*result = failf("this command was stopped. A running job goes on: wait for it with --resume")
		}
	}
}

// announceWorkflow is what a new run says before its first job. A local run
// names its stages. With --json it says the task's name alone: a chat reads
// every line, and its starter has the command that resumes.
func announceWorkflow(ctx context.Context, s *session, o workflowOptions) {
	if o.resume != "" {
		return
	}
	if o.local && o.json {
		fmt.Fprintln(s.cfg.Err, "Kitsu task", o.id)
	} else {
		fmt.Fprintf(s.cfg.Err, "Kitsu task %s. Resume with kitsu task run --resume %s\n", o.id, o.id)
	}
	task := savedWorkflow(s, o.id)
	switch {
	case !o.local:
		fmt.Fprintln(s.cfg.Err, roleWorkflowCost+" Supervised runs are not measured.")
	case !o.json && task != nil:
		fmt.Fprintln(s.cfg.Err, "Runner: local. The approved checks of kitsu.toml and each agent run as you, in a private copy of your change. An agent is a fresh Codex or Claude Code process and uses tokens of your account.")
		required, _ := repositoryRequiresReview(ctx, s, task, task.Start)
		fmt.Fprintln(s.cfg.Err, localStages(task, s.workflowRunner.Offer().Policy.Checks, required))
	}
}

// localStages lists what a local run starts, in order. required says that
// kitsu.toml asks for a review of every change.
func localStages(t *harness.Task, checks []workflowrunner.Check, required bool) string {
	agent := func(role string) string {
		assigned := t.Workflow.Roles[role]
		return fmt.Sprintf("%s (%s, %s)", role, assigned.Host, cmp.Or(assigned.Model.Model, "its default model"))
	}
	ids := make([]string, len(checks))
	for i, check := range checks {
		ids[i] = check.ID
	}
	scope, stages := t.Scope, []string{}
	review := required || scope.RequireReview || scope.Entry == "review" || scope.Until == "review"
	if scope.Entry == "implement" {
		stages = append(stages, agent("developer"))
	} else if scope.Entry == "review" {
		stages, review = append(stages, agent("reviewer")), false
	}
	if scope.Until == "verify" || scope.Until == "deliver" {
		stages = append(stages, "checks ("+strings.Join(ids, ", ")+")")
	}
	if review {
		stages = append(stages, agent("reviewer"))
	}
	if scope.Commit {
		stages = append(stages, "commit")
	}
	return fmt.Sprintf("Stages: %s. At most %d repairs and %d reviews.", strings.Join(stages, ", then "), t.Workflow.ReworkMax, t.ReviewLimit)
}

// announceWorkflowStage is the progress line of one job.
func announceWorkflowStage(s *session, o workflowOptions, a *harness.WorkflowAttempt) {
	switch local := s.workflowRunner.Offer().Profile == localrunner.Profile; {
	case local && o.json:
	case local && a.Role != "":
		fmt.Fprintf(s.cfg.Err, "Kitsu task %s · %s · %s on %s\n", o.id, a.Action, a.Role, a.Host)
	default:
		fmt.Fprintf(s.cfg.Err, "Kitsu task %s · %s\n", o.id, a.Action)
	}
}

// printWorkflowReply is the --json reply of a run. A chat reads all of it, so
// a local run answers in one line with what the chat reports. The rest is in
// kitsu task show ID --json.
func printWorkflowReply(s *session, o workflowOptions, reply map[string]any) error {
	if s.workflowRunner.Offer().Profile != localrunner.Profile {
		return printJSON(s.cfg, reply)
	}
	for key := range reply {
		if !slices.Contains([]string{"task", "next", "verified", "repository_checks", "delivery", "review_findings", "workflow_problem", "diagnostic", "pause", "resume"}, key) {
			delete(reply, key)
		}
	}
	if t := savedWorkflow(s, o.id); t != nil {
		var tokens int64 // of every agent the run started: input, cached or not, and output
		for _, a := range t.Workflow.Attempts {
			tokens += a.Usage["input_tokens"] + a.Usage["output_tokens"]
		}
		reply["tokens"] = tokens
	}
	return json.NewEncoder(s.cfg.Out).Encode(reply)
}

// printWorkflowUsage lists what each job of a local run took: its time and,
// for an agent, the tokens its host reported.
func printWorkflowUsage(s *session, id string) {
	t := savedWorkflow(s, id)
	if t == nil || t.Workflow.Profile != localrunner.Profile ||
		!slices.ContainsFunc(t.Workflow.Attempts, func(a harness.WorkflowAttempt) bool { return a.Role != "" }) {
		return
	}
	w := tabwriter.NewWriter(s.cfg.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "STAGE\tAGENT\tTIME\tINPUT TOKENS\tOF THEM CACHED\tOUTPUT TOKENS")
	var elapsed time.Duration
	var input, cached, output int64
	for _, a := range t.Workflow.Attempts {
		elapsed += a.Elapsed
		if a.Role == "" {
			fmt.Fprintf(w, "%s\tchecks\t%s\n", a.Action, a.Elapsed.Round(time.Second/10))
			continue
		}
		in, hit, out := a.Usage["input_tokens"], a.Usage["cached_input_tokens"], a.Usage["output_tokens"]
		input, cached, output = input+in, cached+hit, output+out
		fmt.Fprintf(w, "%s\t%s on %s\t%s\t%s\t%s\t%s\n", a.Action, a.Role, a.Host, a.Elapsed.Round(time.Second/10), commas(in), commas(hit), commas(out))
	}
	fmt.Fprintf(w, "total\t\t%s\t%s\t%s\t%s\n", elapsed.Round(time.Second/10), commas(input), commas(cached), commas(output))
	_ = w.Flush()
}

// directAssignment is what the developer of a run without an architect and a
// test writer is told, in place of the instructions of a planned run.
//
// Such a developer owns the tests, and nothing but the protected paths limits
// what it returns. Both workers are therefore told not to let a check pass by
// weakening it.
func directAssignment(task *harness.Task, planned string) string {
	switch role := task.Workflow.Active.Role; {
	case task.Direct() && role == "developer":
		return "Implement or repair the request, with the tests that prove it. Fix the code, not the gate: change an existing test, a script that a check runs or the configuration of a tool only where the request needs it, and say so in note. Preserve pre-existing unrelated work, HEAD, and the index. Kitsu runs verification and performs any authorized commit."
	case task.Direct() && role == "reviewer":
		return planned + " A test, a script that a check runs or the configuration of a tool that was weakened or removed without the request asking for it is a code: finding."
	}
	return planned
}

// directCorrections sends every finding of such a run back to its developer,
// who owns the tests and the design too.
func directCorrections(task *harness.Task, planned []string) []string {
	if task.Direct() && len(planned) > 0 {
		return []string{"repair"}
	}
	return planned
}

// localRunnerMark tells a result of the local runner from one of a registered
// runner service wherever the result is shown.
func localRunnerMark(t *harness.Task) string {
	if t.Workflow.Profile == localrunner.Profile {
		return " (runner: local)"
	}
	return ""
}
