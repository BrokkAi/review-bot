# Repository guidance

Review-bot reviews pull requests and publishes non-blocking comments. Keep
investigation and independent verification separate. Only the daemon owns
GitHub publication; do not add automatic approvals, change requests, fixes,
or merges without an explicit feature request.

Use the shared acp-go runner and internal/osrun subprocess helpers. Preserve
bounded output, process-group cancellation, and responsive terminal rendering.
Do work off the terminal event loop and send owned progress snapshots.

Keep exact base/head revisions, complete discussion, valid diff locations,
and durable publication intent central to review handling. Never blindly
retry a GitHub write with an uncertain outcome. A failed PR must not starve
other queued PRs. Dry-run must not consume live publication state.

Use focused behavior tests with temporary Git repositories, fake GitHub
responses and ACP agents. Run `go test -race ./...`, `go vet ./...`, the Python
release tests, and relevant npm/terminal tests. Go tests exercise subprocesses,
pipes and terminal devices; use an environment that permits those facilities.
Run `python3 scripts/licenses.py` for dependency and packaging changes.
For release work, follow RELEASING.md. Do not publish live review fixtures.

After requested implementation and validation, commit the changed files on
the current branch. Push when the user requests it. Do not create a release
tag or publish registry packages without a release request.
