# Brokk Review Bot

An autonomous pull request reviewer in Go. **`brv`** watches GitHub PRs,
investigates the exact change through Agent Client Protocol, independently
verifies each finding, and posts a non-blocking review with inline comments
and a summary. It never approves, requests changes, merges, or pushes fixes.

The bot uses the shared [acp-go](https://github.com/BrokkAi/acp-go) runner.
Its CLI, terminal dashboard, private state, and release tooling follow
[bug-bot](https://github.com/BrokkAi/bug-bot) and
[issue-bot](https://github.com/BrokkAi/issue-bot).

## Install and run

Requires Linux or macOS, Git, authenticated GitHub CLI (`gh`), and an
authenticated ACP coding agent. Codex through `codex-acp` is the default. If the
adapter is missing, the bot uses `npx --yes @agentclientprotocol/codex-acp`, which
requires Node.js and may download the adapter on first use. Explicit agent
commands are used as configured.

```sh
npm install -g @brokkai/review-bot
cd /path/to/repository
brv
```

You can also install a native release without Node.js or a Go toolchain:

```sh
curl -fsSL https://raw.githubusercontent.com/BrokkAi/review-bot/master/install.sh | sh
```

To build from source, install Go 1.27.1 and run:

```sh
go install github.com/BrokkAi/review-bot/cmd/brv@latest
cd /path/to/repository
brv
```

From this source checkout, `make build` creates `bin/brv`.
Go installs into `GOBIN`, or `$(go env GOPATH)/bin`; add it to your `PATH`.

```sh
brv /path/to/repository
brv https://github.com/OWNER/REPO.git
brv --once --pr 123 --dry-run
brv --branch release/1.x --label ready --exclude-label no-review
brv --model YOUR_MODEL --effort high
brv --agent another-acp-agent --agent-arg=--stdio
brv status
brv retry --pr 123 --once
brv version
```

No configuration file is needed. The bot detects `origin` and the remote's
default branch, including when started from a subdirectory. `--pr` selects one
PR but still respects the branch, draft, and label filters. Repeated `--label`
flags require every named label; any `--exclude-label` match excludes a PR.
Labels are compared without case sensitivity.

By default, each poll processes open, non-draft, unlocked PRs targeting the
default branch, oldest first. Fork and bot-authored PRs are eligible. Polling
starts immediately and repeats every five minutes. `--once` processes the
eligible queue once and exits. It can post reviews; add `--dry-run` to inspect
proposed review payloads without writing to GitHub. A dry run does not consume
the live review for that revision.

## How reviews work

1. Read PR metadata, existing discussion, inline comments, and review summaries.
   Fetch the exact base and PR head through the target repository's PR refs.
2. Inspect the complete merge-base-to-head diff and surrounding code in a
   detached worktree owned by the bot's private bare repository.
3. Ask the agent for concrete defects introduced by the PR: correctness,
   security, data loss, or demonstrated performance regressions. Style,
   speculative concerns, feature requests, and unrelated existing bugs are
   excluded. The default maximum is ten findings, never a quota.
   P1/P2 mark defects worth blocking a merge over; P3 is advisory and never
   blocks. The Town worker forwards only confirmed P1/P2 findings, so an
   advisory-only review lets Town certify clean and merge.
4. Verify each candidate in a fresh agent session and worktree. Compare its
   cause and triggering conditions against every discussion entry and findings
   already accepted in this batch. Unsupported, uncertain, and duplicate
   candidates are excluded.
5. Validate source locations against both the local diff and GitHub's diff
   patches. Verified findings without a valid inline anchor appear in the
   summary with links to the exact source commit.
6. Recheck PR eligibility, description, base/head commits, and discussion.
   Submit one `COMMENT` review bound to the head SHA. A review with no new
   verified findings reports coverage and limitations, not approval.

New head or base commits create a new review revision. Earlier reviews remain
as history. Already reported, unchanged root causes are not repeated; a
verified new regression after a fix can be reported with an explanation.

Tests and temporary reproductions may run in isolated worktrees. Tracked-source
or commit changes invalidate the evidence. Each verification gets a fresh
worktree, so it cannot rely on an investigator's local reproduction artifacts.
The default two-hour timeout covers the entire attempt, including verification.
`--timeout`, `--attempts`, and `--max-findings` customize those limits.

Publication and agent work use the caller's operating-system permissions.
Private worktrees isolate Git state; they are not an OS sandbox. Review untrusted
repositories in an appropriately isolated environment. The agent is instructed
to keep GitHub writes under daemon control; this is not a credential sandbox.
The authenticated GitHub account needs repository access and permission to
write pull request reviews. Reviews are posted under that account and identify
themselves as automated.

## Recovery and limits

Before posting, the bot saves the full payload, authenticated author, and a
revision marker. If a response is lost, it looks for that author's marked
review before doing anything else, even if the PR has since closed or changed.
When the outcome remains uncertain, it retains the pending submission and
refuses to repost. Inspect GitHub and saved state; `retry` cannot clear an
uncertain publication.

Confirmed failures retry after fifteen minutes, up to three attempts by
default. Failed or uncertain PRs do not prevent other PRs from being processed.
After correcting a failure, stop the daemon and use `brv retry --pr NUMBER` to
reset that PR's exhausted attempts. Agent setup failures do not spend attempts.

One daemon per repository across machines is the supported deployment model.
Local locks prevent simultaneous instances on one account, including instances
watching different branches. GitHub does not provide an atomic revision-check
and review-creation operation: a PR can change immediately after the final
check, but the review still identifies the exact commit it inspected.

Incomplete GitHub pagination, unavailable exact refs, malformed agent output,
changed discussion, and oversized command output stop that attempt. Command
stdout is bounded at 8 MiB. Very large PRs exceeding GitHub's changed-file
listing limit are rejected rather than partially reviewed. The bot does not
wait for CI, respond conversationally to threads, resolve old comments, or run
as a webhook service.

## Terminal dashboard and state

An interactive terminal displays queue results, active PR and commit,
investigation/verification/publication stages, model, effort, attempts, tools,
and activity. The dashboard adapts to resized and narrow terminals.

- `1`, `2`, `3` or `Tab`: overview, reviews, and activity.
- Arrow keys or `j`/`k`: browse reviews or scroll activity.
- `Enter`: inspect a review's findings, evidence, summary, and failures.
- `Esc`: return; `Page Up`/`Page Down`, `g`/`G`: scroll details.
- `q` or `Ctrl+C`: cancel work, save progress, and restore the terminal.

`--plain` selects scrolling logs; `--json` selects structured logs. They are
mutually exclusive. Redirected output, piped input, or `TERM=dumb` also select
scrolling logs. `NO_COLOR` disables colors. Status, version, and help do not
open the dashboard.

State defaults to `$XDG_STATE_HOME/review-bot` or `~/.local/state/review-bot`,
with separate repository/branch directories. Review payloads, findings, and
history live in `state.json`; private ACP transcripts are in `sessions/`.
Normal session cleanup removes temporary worktrees. A process killed without
cleanup can leave a worktree in the bot's own workspace; subsequent attempts
use new worktrees. `status` prints saved state without launching an agent.

## Brokk Town worker service

`brv worker --socket PATH` serves one-shot PR review operations to Brokk
Town over a private Unix-domain socket. The socket is mode `0600`; the endpoint is
private to the local service, and the process exits after Town requests shutdown.

Worker protocol v1 uses standard-library HTTP with JSON messages:

- `GET /v1/initialize` returns the protocol range, bot identity, release version,
  and capabilities. Town requires `exact-revision-review` as well as common `run` and
  `progress` capabilities.
- `POST /v1/runs` accepts one strict JSON task and responds with contiguous
  newline-delimited JSON events: `progress`, optional typed `result`,
  and `error`, `canceled`, or `complete`.
- `POST /v1/shutdown` asks the service to stop after the current stream.

Version and capability negotiation happen before work starts. Town does not read
this bot's private state files; issue and review outcomes are explicit protocol
results when applicable, while GitHub remains the durable source for receipts.
The schemas are independent of the Unix HTTP transport, allowing an authenticated
TLS transport to be added later without changing worker semantics.

## Optional configuration

`brv --config /path/to/review-bot.json` loads an explicit JSON configuration;
paths are relative to that file. Unknown keys are rejected. No config file is
implicitly loaded or created. See [review-bot.example.json](review-bot.example.json).
CLI flags override corresponding file values. `--model` and `--effort` must be
acknowledged by the agent; unsupported explicit choices fail visibly.

The optional `verify` array is an operator-supplied command run after each
agent session in its worktree. It must succeed without changing tracked source.
A custom `github.host` supports GitHub Enterprise instances exposing the same
REST endpoints and PR Git refs; use `github.repo` when the clone remote is a
local mirror.

## Development and distribution

```sh
make check build
python3 -m unittest discover -s scripts -p '*_test.py'
node --test --test-isolation=none npm/brv.test.cjs
```

Tests use temporary Git repositories, simulated GitHub APIs and agents, and
terminal fixtures. They do not post live reviews. See
[CONTRIBUTING.md](CONTRIBUTING.md) for development guidance.

Release tooling builds Linux/macOS amd64/arm64 archives and npm packages under
`@brokkai/review-bot`. Tagged releases publish all four native archives and all
five npm packages from the same commit. npm publication uses GitHub Actions
trusted publishing with the `packages-publish` environment and no stored npm
token. Release archives and packages include the license, notice, and dependency
license report.

See [RELEASING.md](RELEASING.md) for release checks and trusted publisher setup.

## License and community

Licensed under [Apache-2.0](LICENSE). [NOTICE](NOTICE) identifies project
attribution. [licenses/README.md](licenses/README.md) describes dependency
terms and generated notices included in every package.

Follow the [Code of Conduct](CODE_OF_CONDUCT.md). Report vulnerabilities
privately as described in [SECURITY.md](SECURITY.md).
