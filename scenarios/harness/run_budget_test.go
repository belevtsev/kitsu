package harness_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/belevtsev/kitsu/v3/internal/kitsutest"
)

// The numbers in this file are what a local run costs: agent starts for the
// developer's account, and bytes for a chat that started the run. They only
// go down.
const (
	chatBytes    = 300 // everything a chat reads from a run: the --json reply and the line that names the task
	starterWords = 150 // everything a chat reads to start one: the managed skill of the role plugin, after its head

	// The two commands of a chat: one starts a run and one continues it.
	chatStart    = `kitsu task run --local "<complete request>" --json`
	chatContinue = `kitsu task run --resume <task> --json`
)

// chatArgs is the arguments of a command of a chat, with a value in its
// placeholder.
func chatArgs(command, placeholder, value string) []string {
	args := strings.Fields(strings.Replace(command, placeholder, "\x00", 1))[1:]
	args[slices.Index(args, "\x00")] = value
	return args
}

// budgetRepo is a repository as kitsu init leaves it, with approved checks
// and no roles file. Both hosts are installed, so by default the developer
// works in Claude Code and the reviewer in Codex.
func budgetRepo(t *testing.T) *kitsutest.Repo {
	t.Helper()
	r := kitsutest.Demo(t)
	r.Setenv("LOCALAPPDATA", filepath.Join(r.Home, "cache"))
	approveCIRepo(t, r)
	return r
}

func TestALocalRunStartsNoMoreAgentsThanItsPathNeeds(t *testing.T) {
	greeting := hostTurn{Write: map[string]string{"src/hello.txt": "hello\n"}, Result: completed}
	finding := hostTurn{Diff: true, Result: map[string]any{"decision": "findings", "note": "One defect.", "findings": []string{"code: the greeting has no name"}}}
	for name, path := range map[string]struct {
		developer, reviewer []hostTurn
	}{
		"approved at once": {[]hostTurn{greeting}, []hostTurn{{Diff: true, Result: approved}}},
		"one finding": {
			[]hostTurn{greeting, {Write: map[string]string{"src/hello.txt": "hello, world\n"}, Result: completed}},
			[]hostTurn{finding, {Diff: true, Result: approved}},
		},
		"one failed check": {
			[]hostTurn{{Write: map[string]string{"src/app.txt": "broken\n", "src/hello.txt": "hello\n"}, Result: completed}, {Write: map[string]string{"src/app.txt": "ok\n"}, Result: completed}},
			[]hostTurn{{Diff: true, Result: approved}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := budgetRepo(t)
			calls := scriptHosts(t, r, map[string][]hostTurn{"claude": path.developer, "codex": path.reviewer})
			got := r.Kitsu("task", "run", "--local", "Add a greeting", "--id", "budget", "--json")
			if got.Code != 0 || !strings.Contains(got.Stdout, `"next":"done"`) {
				t.Fatalf("the run did not end verified and reviewed, exit %d:\n%s", got.Code, got.Output())
			}
			// Every turn of the script was played, and a start beyond it fails the run.
			if len(calls("claude")) != len(path.developer) || len(calls("codex")) != len(path.reviewer) {
				t.Errorf("want %d developer and %d reviewer starts, got %d and %d", len(path.developer), len(path.reviewer), len(calls("claude")), len(calls("codex")))
			}
			tokens := fmt.Sprintf(`"tokens":%d`, 110*(len(path.developer)+len(path.reviewer))) // each scripted start reports 100 and 10
			if len(got.Output()) > chatBytes || strings.Count(got.Output(), "\n") != 2 || !strings.Contains(got.Stdout, tokens) {
				t.Errorf("a chat reads %d bytes, want at most %d in two lines, with %s:\n%s", len(got.Output()), chatBytes, tokens, got.Output())
			}
		})
	}
}

// A person at a terminal is told what will start before it starts, and what
// each start took afterwards.
func TestALocalRunNamesItsStagesAndWhatEachTook(t *testing.T) {
	r := budgetRepo(t)
	scriptHosts(t, r, map[string][]hostTurn{
		"claude": {{Write: map[string]string{"src/hello.txt": "hello\n"}, Result: completed}},
		"codex":  {{Diff: true, Result: approved}},
	})
	got := r.Kitsu("task", "run", "--local", "Add a greeting", "--id", "told")
	if got.Code != 0 || !strings.Contains(got.Stdout, "next done") {
		t.Fatalf("the run did not end verified and reviewed, exit %d:\n%s", got.Code, got.Output())
	}
	for _, want := range []string{
		"Stages: developer (claude, sonnet), then checks (unit, lint), then reviewer (codex, gpt-6.1-sol). At most 2 repairs and 3 reviews.",
		"Kitsu task told · implement · developer on claude\n", "Kitsu task told · verify\n", "Kitsu task told · review · reviewer on codex\n",
	} {
		if !strings.Contains(got.Stderr, want) {
			t.Errorf("the run did not say %q:\n%s", want, got.Stderr)
		}
	}
	rows := strings.Split(got.Stdout, "\n")
	if len(rows) < 5 || !strings.HasPrefix(rows[0], "STAGE") || strings.Join(strings.Fields(rows[1])[:4], " ") != "implement developer on claude" ||
		strings.Fields(rows[2])[1] != "checks" || strings.Join(strings.Fields(rows[4])[2:], " ") != "200 0 20" {
		t.Errorf("the run did not list what each stage took, with 100 input and 10 output tokens for each agent:\n%s", got.Stdout)
	}
}

func TestALocalRunThatOnlyVerifiesStartsNoReviewer(t *testing.T) {
	r := budgetRepo(t)
	calls := scriptHosts(t, r, nil)
	got := r.Kitsu("task", "run", "--local", "Verify the demo", "--id", "checked", "--entry", "verify")
	if got.Code != 0 || !strings.Contains(got.Stdout, "next done") || !strings.Contains(got.Stderr, "Stages: checks (unit, lint). ") {
		t.Fatalf("a run that enters at verify did not end with its checks alone, exit %d:\n%s", got.Code, got.Output())
	}
	if len(calls("claude"))+len(calls("codex")) != 0 {
		t.Error("a run that enters at verify started an agent")
	}
}

// kitsu.toml can require a review of every change. A run cannot leave that
// review out, so its stage list names it.
func TestALocalRunNamesTheReviewThatTheRepositoryRequires(t *testing.T) {
	r := kitsutest.Demo(t)
	r.Write("kitsu.toml", strings.Replace(kitsutest.DemoDeclaration, "version = 1\n", "version = 1\nrequire_review = true\n", 1))
	r.Git("commit", "-qam", "Require a review")
	r.Setenv("LOCALAPPDATA", filepath.Join(r.Home, "cache"))
	approveCIRepo(t, r)
	calls := scriptHosts(t, r, map[string][]hostTurn{
		"claude": {{Write: map[string]string{"src/hello.txt": "hello\n"}, Result: completed}},
		"codex":  {{Diff: true, Result: approved}},
	})
	got := r.Kitsu("task", "run", "--local", "Add a greeting", "--id", "required", "--require-review=false")
	if got.Code != 0 || !strings.Contains(got.Stdout, "next done") || len(calls("codex")) != 1 {
		t.Fatalf("the required review did not run, exit %d, %d reviewer starts:\n%s", got.Code, len(calls("codex")), got.Output())
	}
	if !strings.Contains(got.Stderr, "then checks (unit, lint), then reviewer (codex, gpt-6.1-sol).") {
		t.Errorf("the stage list leaves out the review that kitsu.toml requires:\n%s", got.Stderr)
	}
}

// A chat that started a run is told why it paused and how to go on.
func TestALocalRunTellsAChatWhyItPaused(t *testing.T) {
	r := budgetRepo(t)
	scriptHosts(t, r, map[string][]hostTurn{
		"claude": {{Write: map[string]string{"src/hello.txt": "hello\n"}, Result: completed}, {Write: map[string]string{"src/hello.txt": "hello, world\n"}, Result: completed}},
		"codex":  {{Diff: true, Result: map[string]any{"decision": "findings", "note": "One defect.", "findings": []string{"code: the greeting has no name"}}}},
	})
	got := r.Kitsu("task", "run", "--local", "Add a greeting", "--id", "paused", "--review-limit", "1", "--json")
	for _, want := range []string{`"next":"paused"`, `"reason":"review_limit"`, `"resume":["kitsu","task","next","paused","--brief"]`} {
		if got.Code == 0 || !strings.Contains(got.Stdout, want) {
			t.Fatalf("a run at its review limit did not reply with %s, exit %d:\n%s", want, got.Code, got.Output())
		}
	}
}

// The starter is all a chat reads to start a run: at most starterWords words,
// the two commands that the test below runs, and files that the skill ships.
func TestTheChatStarterNamesOneCommandToStartARunAndOneToContinueIt(t *testing.T) {
	skill := filepath.Join("..", "..", "plugins", "kitsu-team", "skills", "managed")
	data, err := os.ReadFile(filepath.Join(skill, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, starter, _ := strings.Cut(strings.TrimPrefix(strings.ReplaceAll(string(data), "\r\n", "\n"), "---\n"), "\n---\n")
	if words := len(strings.Fields(starter)); words == 0 || words > starterWords {
		t.Errorf("the starter has %d words after its head, want at most %d", words, starterWords)
	}
	var commands []string
	for i, span := range strings.Split(starter, "`") {
		if i%2 == 1 && strings.HasPrefix(span, "kitsu ") {
			commands = append(commands, span)
		}
	}
	if want := []string{chatStart, chatContinue}; !slices.Equal(commands, want) {
		t.Errorf("the starter names the commands %q, want one to start a run and one to continue it: %q", commands, want)
	}
	for _, word := range strings.Fields(starter) {
		if file := strings.Trim(word, ",.;"); strings.HasSuffix(file, ".md") && !strings.Contains(file, "internal/") {
			if _, err := os.Stat(filepath.Join(skill, filepath.FromSlash(file))); err != nil {
				t.Errorf("the starter names a file that the skill does not ship: %v", err)
			}
		}
	}
}

// A chat starts a run with one command and continues it with one. The shell
// of a chat ends a command that takes long in its own way, and in none of
// them is the agent that was working started again.
func TestAChatStartsARunWithOneCommandAndContinuesItWithOne(t *testing.T) {
	for name, end := range map[string]func(*exec.Cmd) error{
		"killed":       func(c *exec.Cmd) error { return c.Process.Kill() },                  // a hard stop
		"terminated":   func(c *exec.Cmd) error { return c.Process.Signal(syscall.SIGTERM) }, // Codex 0.171.0 at the timeout of its shell
		"left running": nil,                                                                  // Claude Code 2.1.329 at the timeout of its shell
	} {
		t.Run(name, func(t *testing.T) {
			if name == "terminated" && runtime.GOOS == "windows" {
				t.Skip("Windows has no signal that asks a program to end")
			}
			r := budgetRepo(t)
			gate := filepath.Join(t.TempDir(), "go")
			calls := scriptHosts(t, r, map[string][]hostTurn{
				"claude": {{Write: map[string]string{"src/hello.txt": "hello\n"}, Result: completed, Wait: gate}},
				"codex":  {{Diff: true, Result: approved}},
			})
			// The output goes to a file: a pipe would stay open for as long as the job runs.
			output := filepath.Join(t.TempDir(), "output")
			file, err := os.Create(output)
			if err != nil {
				t.Fatal(err)
			}
			start := r.Command(filepath.Join(kitsutest.Binary(t), "kitsu"), chatArgs(chatStart, `"<complete request>"`, "Add a greeting")...)
			start.Stdout, start.Stderr = file, file
			if err := start.Start(); err != nil {
				t.Fatal(err)
			}
			ended := make(chan struct{})
			go func() { _ = start.Wait(); _ = file.Close(); close(ended) }()
			// Whatever happens below, the developer is let go and the first
			// command has ended before the folders go. After a failure the
			// job of the developer gets a moment to end as well.
			t.Cleanup(func() {
				_ = os.WriteFile(gate, nil, 0o600)
				<-ended
				if t.Failed() {
					time.Sleep(2 * time.Second)
				}
			})
			for deadline := time.Now().Add(time.Minute); len(calls("claude")) == 0 && time.Now().Before(deadline); {
				time.Sleep(50 * time.Millisecond)
			}
			if end != nil {
				if err := end(start); err != nil {
					t.Fatal(err)
				}
				<-ended
			}
			first, _ := os.ReadFile(output)
			line, rest, _ := strings.Cut(string(first), "\n")
			task, named := strings.CutPrefix(line, "Kitsu task ")
			// Only a command that was asked to end says more: how the run goes on.
			if len(calls("claude")) != 1 || !named || strings.Contains(rest, "--resume") != (name == "terminated") || strings.Count(rest, "\n") > 1 {
				t.Fatalf("the first command started %d developers and printed, want a first line that names the task:\n%s", len(calls("claude")), first)
			}
			time.AfterFunc(time.Second, func() { _ = os.WriteFile(gate, nil, 0o600) }) // the developer ends while the second command waits
			got := r.Kitsu(chatArgs(chatContinue, "<task>", task)...)
			if got.Code != 0 || !strings.Contains(got.Stdout, `"next":"done"`) || !strings.Contains(got.Stdout, `"tokens":220`) {
				t.Fatalf("the second command did not end the run verified and reviewed, exit %d:\n%s", got.Code, got.Output())
			}
			<-ended
			all, _ := os.ReadFile(output)
			if end != nil {
				// The first command was stopped: the chat read its lines and the reply of the second.
				if read := len(all) + len(got.Output()); read > chatBytes || strings.Count(got.Output(), "\n") != 1 {
					t.Errorf("a chat reads %d bytes over both commands, want at most %d, and one line from the second:\n%s%s", read, chatBytes, all, got.Output())
				}
			} else if !strings.Contains(string(all), `"next":"done"`) || len(all) > chatBytes || len(got.Output()) > chatBytes || !strings.Contains(got.Stderr, "another command runs this task") {
				// The first command went on: a chat waits for it and reads its
				// output alone. One that asks again is told to wait, and gets the
				// same reply.
				t.Errorf("want the run ended by the command that was left running and by the one that waited for it, each in at most %d bytes:\n%s%s", chatBytes, all, got.Output())
			}
			if len(calls("claude")) != 1 || len(calls("codex")) != 1 {
				t.Errorf("want one developer and one reviewer start over both commands, got %d and %d", len(calls("claude")), len(calls("codex")))
			}
		})
	}
}

// Ctrl+C is the one end of a command that stops its job: the developer wants
// the agent to stop. A later command says so and starts no agent on its own.
func TestALocalRunThatIsInterruptedStopsItsAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this test cannot send Ctrl+C to one program on Windows")
	}
	r := budgetRepo(t)
	gate := filepath.Join(t.TempDir(), "never")
	calls := scriptHosts(t, r, map[string][]hostTurn{"claude": {{Result: completed, Wait: gate}}})
	start := r.Command(filepath.Join(kitsutest.Binary(t), "kitsu"), "task", "run", "--local", "Add a greeting", "--id", "interrupted", "--json")
	if err := start.Start(); err != nil {
		t.Fatal(err)
	}
	// A developer that was not stopped is let go after a while, so that a
	// run that goes on fails this test instead of holding it.
	release := time.AfterFunc(20*time.Second, func() { _ = os.WriteFile(gate, nil, 0o600) })
	t.Cleanup(func() { release.Stop(); _ = os.WriteFile(gate, nil, 0o600) })
	for deadline := time.Now().Add(time.Minute); len(calls("claude")) == 0 && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if err := start.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	_ = start.Wait()
	got := r.Kitsu("task", "run", "--resume", "interrupted", "--json")
	if got.Code == 0 || !strings.Contains(got.Output(), "the job was stopped before it ended") || len(calls("claude")) != 1 || len(calls("codex")) != 0 {
		t.Fatalf("an interrupted run went on, exit %d, %d developer and %d reviewer starts:\n%s", got.Code, len(calls("claude")), len(calls("codex")), got.Output())
	}
}
