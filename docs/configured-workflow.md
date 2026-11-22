# Run a configured role workflow

Kitsu coordinates Codex and Claude Code from one saved task. For supervised execution, an administrator registers a disposable runner for this worktree. The runner starts each assigned specialist, verifies the accepted files in isolation, and signs the result. Kitsu imports authorized edits, records evidence, and resumes the next step.

Keep the [`kitsu-team` plugin](release-install.md#role-coordination) installed and [set up a trusted runner](workflow-runner.md) before using `task run`. Use the [daily task skill](task-harness.md) for ordinary in-chat work.

## Start and resume from either host

**Codex** · “Use Kitsu’s managed skill and the registered runner to fix CSV quoting and verify the exported value.”

**Claude Code** · `/kitsu-team:managed Use the registered runner to fix CSV quoting and verify the exported value.`

Name the registered runner in the request. Without it the `managed` skill first tries a [run on your own machine](#run-a-task-on-your-own-machine), which is refused where a runner is registered.

Resume in the original worktree. In Codex, ask “Resume configured Kitsu task `TASK_ID`.” In Claude Code, enter `/kitsu-team:managed Resume configured Kitsu task TASK_ID`. Both use the same saved assignments and runner.

```sh
kitsu task run "Fix CSV quoting" --until verify
kitsu task run --resume TASK_ID
```

## Configure the roles

In your repository, run `kitsu role init` to create version 2 of `kitsu-roles.toml` from the install choice. Supply `--host codex` or `--host claude` to seed from a different host. An existing version 1 or 2 file requires an intentional edit.

```sh
kitsu role init
kitsu role set architect --host codex --model gpt-6.1-sol --reasoning max
kitsu role set developer --host claude --model sonnet --reasoning high
kitsu role set reviewer --host codex
kitsu role list
```

You can ask either plugin to make the same changes and display the result. Use repository-scoped commands before starting the task. The settings saved with an active daily run are used on resume.

To edit by hand, use one `[roles.<role>]` table per specialist below. The optional `git` role is available for manual handoffs. Kitsu creates an explicitly requested local commit after supervised verification. Omitting `model` uses the role’s host preset. Omitting `effort` uses the host’s reasoning default. Omit `skills` to inherit personal skill selections, or use `skills = []` for an empty selection. `kitsu role set ROLE --reasoning ''` restores the host’s reasoning default without changing the model.

```toml
version = 2

[roles.lookup]
host = "claude"
model = "haiku"
effort = "medium"

[roles.spec-writer]
host = "codex"
model = "gpt-6.1-sol"
effort = "high"

[roles.architect]
host = "codex"
model = "gpt-6.1-sol"
effort = "max"

[roles.quality-engineer]
host = "claude"
model = "sonnet"
effort = "high"

[roles.developer]
host = "claude"
model = "sonnet"
effort = "high"

[roles.reviewer]
host = "codex"
model = "gpt-6.1-sol"
effort = "max"

[roles.git]
host = "claude"
model = "haiku"
effort = "low"
```


The roles file’s optional test paths must match the reviewed runner policy. Use the repository’s own test paths and approved programs.

## Verify or deliver an existing change

Supply the reviewed assertion when opening at verification. The intake assertion stays required during repairs. A test writer can add a separate reviewed assertion. The runner also runs every required repository check.

```sh
kitsu task run "Verify CSV quoting" --id csv --entry verify --until verify \
  --expect "a comma within a field is quoted" -- ./scripts/check-csv
```

Add `--repair` when you authorize source changes. Use `--until deliver --commit` for a local commit and repeat `--include PATH` to authorize pre-existing edits. A configured commit requires standard Kitsu Git hooks or no local commit hooks. Kitsu verifies the exact staged bytes before committing. Add `--require-review` when you need an independent reviewer.

Kitsu applies the configured rework limit, records test and code corrections, and verifies again when assertions or content change. Inspect progress with `kitsu task next TASK_ID --json`.

## Run a task on your own machine

`--local` runs the jobs of a task on your machine, with no runner service. Kitsu decides every stage in code and starts each agent itself: a developer, then the checks, then a reviewer.

```sh
kitsu check trust
kitsu task run --local "Fix CSV quoting" --id csv
kitsu task run --local "Fix CSV quoting" --commit
kitsu task run --local "Verify CSV quoting" --id csv-check --entry verify
kitsu task run --resume csv
```

- **Stages.** A run starts at the developer and ends after the checks and an independent review. A failed check or a review finding goes back to the developer, at most as often as `--rework-limit` allows (2 by default). At most `--review-limit` reviews start (3 by default). `--commit` also delivers one commit.
- **Other entries.** `--entry review` and `--entry verify` start from your own change, and `--entry verify` starts no reviewer unless you add `--require-review`. `--require-review=false` leaves the review out of a run that starts at the developer. A `kitsu.toml` with `require_review = true` requires the review in every case. A local run starts no architect and no test writer, so `--entry plan` and `--entry tests` are refused.
- **Agents.** Each one is a fresh headless `codex exec` or `claude --print` process that uses tokens of your account. The developer may edit files in its copy and gets no wider permission; the reviewer runs in the host's read-only mode. Claude Code loads your user settings and none of a project.
- **Hosts.** A repository with [role assignments](roles.md) uses the hosts and models of its developer and reviewer. Without a roles file you configure nothing: the developer works in the host you installed Kitsu for, or else in the one found on `PATH`, and the reviewer in the other host when that is installed too, so that a second model reads the change.
- **What you see.** Before the first job the run lists its stages with the host and model of each. Each job prints one line when it starts. At the end a table lists the time of each job and the tokens each agent's host reported.
- **From a chat.** The `managed` skill of `kitsu-team` starts a local run with one command. If the chat's shell leaves that command running in the background, the chat waits for it. If the shell ends the command, the chat continues the run with `--resume`. In Claude Code, enter `/kitsu-team:managed <request>`. In Codex, ask for Kitsu’s `managed` skill. With `--json` the run prints the line that names the task and a reply of one line: the next action, the verification, the review findings and the tokens of all agents. For a finished run the two are under 300 bytes together. A paused or blocked run also says why. `kitsu task show ID --json` has the rest. A local run is refused in the sandbox of a Codex chat, because Kitsu cannot write the records of a task there and an agent cannot sign in. Run the same command in your own terminal.
- **Recorded model and effort.** Claude Code names the model it runs. Codex names none, and neither host reports its effort. For those, the task records what the host was started with.
- **Environment.** An agent inherits your home, `PATH`, locale, proxy settings, the host's configuration directory and its API key variable. Other variables are not passed on: a token of another service, or a gateway or cloud provider that you set in your shell. Set those in the host's own settings file.
- **Gate.** The checks of `kitsu.toml` gate the run. Approve them once with `kitsu check trust` in your own terminal. `--expect` is optional, and its command must be one of the declared checks.
- **Private copy.** Each job runs in a copy of your change under your user cache directory. The copy is a Git repository with one commit that can read the objects of your project. It has none of your ignored files, dependency folders or host configuration such as `CLAUDE.md`.
- **Returned changes.** Kitsu imports the files that differ from the copy the developer was given. A new file that Git ignores in your project is left behind. A result that touches `kitsu.toml`, the role assignments, host configuration, Git files, the hook directory or `openspec/` is refused as a whole, and your worktree stays as it was.
- **Tests and check scripts.** The developer writes the tests, so it may also change an existing test, a script that a check runs or a tool's configuration. Only the commands in `kitsu.toml` are fixed. The developer is told to change those files only where the request needs it, and the reviewer is told to report a weakened one. Read the diff when that matters.
- **Stopped command.** A job outlives the command that started it. After the command is killed or terminated, or its terminal closes, `--resume` waits for the same job and returns its result. A command that was terminated says so. Only Ctrl+C stops the job. `--resume TASK` then reports the stop, and `--resume TASK --retry` starts the job again.
- **Two commands.** One command drives a run at a time. A second command for the same task says that it waits, and goes on from where the run stands once the first has ended.
- **Changed policy.** A `kitsu.toml` that changed since the task started refuses the resume.
- **Registered worktrees.** Where an administrator registered a runner for the worktree, `--local` is refused and local results do not count.

**What a local run proves.** The checks and the agents ran as you, with your network and your files, and a key in your user configuration directory signs the receipt. Any program that runs as you can use that key. The private copy and the import rules stop careless damage. They do not stop an agent that sets out to cheat. A local result therefore tells you what the checks returned on the exact change. It does not prove that to someone else. Kitsu marks such a result `runner: local`. Use a [trusted runner](workflow-runner.md) when the result must hold against the person or agent who made the change.

## Recover a stopped run

Resume the same task after an interruption. Kitsu asks the runner for the existing attempt and consumes its retained result. Resolve a reported blocker before resuming with `--retry`. Use `--release ATTEMPT` only after the runner operator authorizes release and the runner confirms the whole job has stopped.

Completed records from the earlier preview remain historical. Start a supervised task with the original request and intake assertion. Reconcile outstanding legacy workers with the operator before reusing their checkout.
