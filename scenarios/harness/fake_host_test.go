package harness_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/belevtsev/kitsu/v3/internal/kitsutest"
)

// TestMain lets this test program stand in for a coding host. A copy of it
// that is named codex or claude plays the next turn written beside it.
func TestMain(m *testing.M) {
	if name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe"); name == "codex" || name == "claude" {
		os.Exit(playHost(name))
	}
	os.Exit(m.Run())
}

// hostTurn is what a scripted host does in one start.
type hostTurn struct {
	Write  map[string]string `json:"write,omitempty"`  // files, relative to its working directory
	Result map[string]any    `json:"result,omitempty"` // its structured answer
	Diff   bool              `json:"diff,omitempty"`   // run the comparison that the assignment names
	Wait   string            `json:"wait,omitempty"`   // answer only once this file exists, or its folder is gone
}

// hostCall is what a scripted host saw in one start.
type hostCall struct {
	Args   []string `json:"args"`
	Dir    string   `json:"dir"`
	Env    []string `json:"env"` // names only
	Prompt string   `json:"prompt"`
	Diff   string   `json:"diff,omitempty"`
}

// scriptHosts puts programs named codex and claude first on the PATH of r, so
// that no test reaches a host installed on this machine. Each start of one
// plays the next of its turns, and fails when none is left. The returned
// function reads what the starts of a host saw.
func scriptHosts(t *testing.T, r *kitsutest.Repo, turns map[string][]hostTurn) func(host string) []hostCall {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	program, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, host := range []string{"codex", "claude"} {
		name, script := host, turns[host]
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		data, _ := json.Marshal(script)
		for file, content := range map[string][]byte{name: program, host + ".turns.json": data} {
			if err := os.WriteFile(filepath.Join(dir, file), content, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	r.Setenv("PATH", dir+string(os.PathListSeparator)+kitsutest.Binary(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func(host string) []hostCall {
		t.Helper()
		var calls []hostCall
		file, err := os.Open(filepath.Join(dir, host+".calls.jsonl"))
		if err != nil {
			return nil
		}
		defer func() { _ = file.Close() }()
		for lines := json.NewDecoder(file); ; {
			var call hostCall
			if err := lines.Decode(&call); err != nil {
				return calls
			}
			calls = append(calls, call)
		}
	}
}

// playHost is one start of a scripted host. It answers in the event format of
// the real program: `codex exec --json` or `claude --print --output-format
// stream-json`.
func playHost(host string) int {
	beside := filepath.Dir(os.Args[0])
	var turns []hostTurn
	data, _ := os.ReadFile(filepath.Join(beside, host+".turns.json"))
	if json.Unmarshal(data, &turns) != nil {
		return 70
	}
	calls := filepath.Join(beside, host+".calls.jsonl")
	earlier, _ := os.ReadFile(calls)
	n := strings.Count(string(earlier), "\n")
	if n >= len(turns) {
		fmt.Fprintln(os.Stderr, "the scripted host has no turn left")
		return 71
	}
	turn := turns[n]
	prompt, _ := io.ReadAll(bufio.NewReader(os.Stdin))
	call := hostCall{Args: os.Args[1:], Prompt: string(prompt)}
	call.Dir, _ = os.Getwd()
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		call.Env = append(call.Env, name)
	}
	for name, content := range turn.Write {
		path := filepath.Join(call.Dir, filepath.FromSlash(name))
		if os.MkdirAll(filepath.Dir(path), 0o755) != nil || os.WriteFile(path, []byte(content), 0o644) != nil {
			return 72
		}
	}
	if turn.Diff {
		call.Diff = runNamedDiff(call.Prompt)
	}
	record, _ := json.Marshal(call)
	log, err := os.OpenFile(calls, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 73
	}
	_, _ = log.Write(append(record, '\n'))
	_ = log.Close()
	for turn.Wait != "" {
		if _, err := os.Stat(turn.Wait); err == nil {
			break
		}
		if _, err := os.Stat(filepath.Dir(turn.Wait)); err != nil {
			return 75 // the test ended without letting this host go
		}
		time.Sleep(50 * time.Millisecond)
	}

	session := fmt.Sprintf("%s-session-%d", host, n)
	result, _ := json.Marshal(turn.Result)
	usage := `{"input_tokens":100,"output_tokens":10}`
	if host == "claude" {
		// Claude Code reports the full name of the model family it was started with.
		model := "claude-" + os.Args[slices.Index(os.Args, "--model")+1] + "-scripted"
		fmt.Printf(`{"type":"system","subtype":"init","session_id":%q,"model":%q}`+"\n", session, model)
		fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"session_id":%q,"structured_output":%s,"usage":%s}`+"\n", session, result, usage)
		return 0
	}
	fmt.Printf(`{"type":"thread.started","thread_id":%q}`+"\n", session)
	fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"error","message":"a setting of the user is deprecated"}}`)
	fmt.Println(`{"type":"turn.started"}`)
	if i := slices.Index(os.Args, "--output-last-message"); i < 0 || os.WriteFile(os.Args[i+1], result, 0o644) != nil {
		return 74
	}
	fmt.Printf(`{"type":"turn.completed","usage":%s}`+"\n", usage)
	return 0
}

// runNamedDiff runs the command that the assignment names as review_diff, in
// the host's working directory, and returns its output or its failure.
func runNamedDiff(prompt string) string {
	_, context, _ := strings.Cut(prompt, "\nContext: ")
	var input struct {
		Diff []string `json:"review_diff"`
	}
	if err := json.NewDecoder(strings.NewReader(context)).Decode(&input); err != nil || len(input.Diff) == 0 {
		return "no review_diff in the assignment"
	}
	out, err := exec.Command(input.Diff[0], input.Diff[1:]...).CombinedOutput()
	if err != nil {
		return "failed: " + err.Error() + ": " + string(out)
	}
	return string(out)
}
