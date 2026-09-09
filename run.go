package reviewbot

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BrokkAi/acp-go/runner"
	"github.com/BrokkAi/review-bot/internal/osrun"
)

type engine struct {
	config  Config
	source  reviewSource
	actor   string
	log     *slog.Logger
	agent   func(Config) Agent
	now     func() time.Time
	observe func(Progress)
	current *Job
}

func Run(ctx context.Context, c Config, log *slog.Logger, once bool) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.GitHubRepo() == "" {
		return errors.New("GitHub repository required")
	}
	if log == nil {
		log = slog.Default()
	}
	unlock, err := lockConfig(c)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := ReadState(c)
	if err != nil {
		return err
	}
	if s == nil {
		s = newState(c)
	}
	source := githubClient{c}
	actor := ""
	if !c.DryRun {
		actor, err = source.actor(ctx)
		if err != nil {
			return err
		}
	}
	observe, _ := ctx.Value(progressKey{}).(func(Progress))
	e := engine{config: c, source: source, actor: actor, log: log, now: time.Now, observe: observe, agent: func(c Config) Agent { return agentProcess{c, log} }}
	e.report(s, "starting", "Loading saved reviews")
	for {
		err := e.step(ctx, s)
		var setup *runner.SetupError
		if once || ctx.Err() != nil || errors.As(err, &setup) {
			return err
		}
		if err != nil {
			log.Error("Review queue has paused jobs", "error", err)
		}
		e.current = nil
		e.report(s, "waiting", "Waiting for PR revisions")
		if err = pause(ctx, time.Duration(c.Poll)); err != nil {
			return err
		}
	}
}
func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func (e *engine) save(s *State) error {
	if err := writeState(e.config, s); err != nil {
		return err
	}
	e.report(s, "", "")
	return nil
}
func (e *engine) step(ctx context.Context, s *State) error {
	var failures []error
	// Reconcile publication even if a PR closed, advanced, or exhausted its attempts.
	for _, j := range s.Jobs {
		if j.Status == "posting" && !e.config.DryRun {
			e.current = j
			e.report(s, "reconciling", fmt.Sprintf("Checking review submission for PR #%d", j.PR.Number))
			if err := e.reconcile(ctx, s, j); err != nil {
				failures = append(failures, err)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	e.current = nil
	e.report(s, "fetching", "Loading open pull requests")
	prs, err := e.source.pulls(ctx)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	var queue []*Job
	for _, listed := range prs {
		p, err := e.source.pull(ctx, listed.Number)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !eligible(e.config, p) {
			continue
		}
		key := revisionKey(e.config, p)
		blocked := false
		var job *Job
		for _, j := range s.Jobs {
			if j.PR.Number == p.Number && j.Status == "posting" && !j.DryRun {
				blocked = true
			}
			if j.Key == key && j.DryRun == e.config.DryRun {
				job = j
			}
			if j.PR.Number == p.Number && j.Key != key && (j.Status == "pending" || j.Status == "failed") {
				j.Status = "stale"
			}
		}
		if blocked {
			continue
		}
		if job == nil {
			job = &Job{Key: key, PR: p, DryRun: e.config.DryRun, Status: "pending"}
			s.Jobs = append(s.Jobs, job)
		}
		if job.Status == "stale" {
			job.Status = "pending"
			job.Tries = 0
		}
		if job.Status == "submitted" || job.Status == "dry_run" || job.RetryAt.After(e.now()) {
			continue
		}
		if job.Tries >= e.config.Attempts {
			failures = append(failures, fmt.Errorf("PR #%d exhausted its review attempts; use retry", p.Number))
			continue
		}
		job.PR = p
		queue = append(queue, job)
	}
	if err := e.save(s); err != nil {
		return err
	}
	for _, j := range queue {
		if err := ctx.Err(); err != nil {
			return err
		}
		e.current = j
		// Recover completed reviews even when local state was lost. Only trust this account.
		if !e.config.DryRun {
			found, err := e.existing(ctx, j)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if found != nil {
				j.Payload = &ReviewPayload{Commit: j.PR.Head.SHA, Event: "COMMENT", Body: found.Body}
				j.Actor = e.actor
				if err = validatePublished(e.config, j, found); err != nil {
					failures = append(failures, err)
					continue
				}
				j.Status = "submitted"
				j.ReviewURL = found.URL
				if err = e.save(s); err != nil {
					return err
				}
				continue
			}
		}
		j.Tries++
		j.Status = "pending"
		j.Failure = ""
		j.Payload = nil
		j.Candidates = nil
		if err := e.save(s); err != nil {
			return err
		}
		e.report(s, "attempt", fmt.Sprintf("Reviewing PR #%d: %s", j.PR.Number, j.PR.Title))
		attemptCtx, cancel := context.WithTimeout(ctx, time.Duration(e.config.Timeout))
		err := e.review(attemptCtx, s, j)
		cancel()
		if err != nil {
			if errors.Is(err, errStale) {
				j.Status = "stale"
				j.Failure = err.Error()
			} else {
				var setup *runner.SetupError
				if errors.As(err, &setup) {
					j.Tries--
				}
				if j.Status != "posting" {
					j.Status = "failed"
					j.RetryAt = e.now().Add(time.Duration(e.config.RetryDelay))
				}
				j.Failure = err.Error()
				failures = append(failures, fmt.Errorf("PR #%d: %w", j.PR.Number, err))
				e.log.Error("Review paused", "pr", j.PR.Number, "error", err)
			}
			if saveErr := e.save(s); saveErr != nil {
				return errors.Join(err, saveErr)
			}
			var setup *runner.SetupError
			if errors.As(err, &setup) {
				return errors.Join(failures...)
			}
		}
	}
	e.current = nil
	e.report(s, "idle", "Review queue processed")
	return errors.Join(failures...)
}
func (e *engine) existing(ctx context.Context, j *Job) (*RemoteReview, error) {
	reviews, err := e.source.reviews(ctx, j.PR.Number)
	if err != nil {
		return nil, err
	}
	var found *RemoteReview
	actor := j.Actor
	if actor == "" {
		actor = e.actor
	}
	for _, r := range reviews {
		if strings.Contains(r.Body, marker(j.Key)) && r.User.Login == actor {
			if found != nil {
				return nil, errors.New("multiple reviews match this revision; inspect GitHub")
			}
			copy := r
			found = &copy
		}
	}
	return found, nil
}
func (e *engine) reconcile(ctx context.Context, s *State, j *Job) error {
	r, err := e.existing(ctx, j)
	if err != nil {
		return err
	}
	if r == nil {
		return errors.New("review submission outcome is unknown; marker not visible, refusing to repost")
	}
	if err = validatePublished(e.config, j, r); err != nil {
		return err
	}
	j.Status = "submitted"
	j.ReviewURL = r.URL
	j.Failure = ""
	return e.save(s)
}
func discussionDigest(d []Discussion) string {
	b, _ := json.Marshal(d)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
func (e *engine) session(ctx context.Context, g checkout, j *Job, prompt string) (text string, err error) {
	dir, cleanup, err := g.worktree(ctx, j.PR.Head.SHA)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, cleanup()) }()
	cfg := e.config
	cfg.Directory = dir
	text, err = e.agent(cfg).Execute(ctx, prompt)
	if err != nil {
		return "", err
	}
	if err = verifyWorktree(ctx, dir, j.PR.Head.SHA); err != nil {
		return "", err
	}
	if len(cfg.Verify) > 0 {
		if _, err = osrun.Run(ctx, dir, nil, cfg.Verify...); err != nil {
			return "", err
		}
		err = verifyWorktree(ctx, dir, j.PR.Head.SHA)
	}
	return text, err
}
func (e *engine) review(ctx context.Context, s *State, j *Job) error {
	g := checkout{e.config}
	if err := g.open(ctx); err != nil {
		return err
	}
	history, err := e.source.discussion(ctx, j.PR.Number)
	if err != nil {
		return err
	}
	e.report(s, "preparing", "Fetching the exact PR revision")
	snapshot, err := g.snapshot(ctx, j.PR, history)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	input, err := os.CreateTemp(e.config.StateDirectory, ".snapshot-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(input.Name())
	if _, err = input.Write(data); err != nil {
		input.Close()
		return err
	}
	if err = input.Close(); err != nil {
		return err
	}
	// The path is absolute because discovery and config loading canonicalize state paths.
	inputPath, err := filepath.Abs(input.Name())
	if err != nil {
		return err
	}
	e.report(s, "investigating", "Investigating introduced defects")
	text, err := e.session(ctx, g, j, investigationPrompt(inputPath, e.config.MaxFindings))
	if err != nil {
		return err
	}
	result, err := parseResult(text, e.config.MaxFindings)
	if err != nil {
		return err
	}
	j.Summary = result.Summary
	githubFiles, err := e.source.files(ctx, j.PR.Number)
	if err != nil {
		return err
	}
	if len(githubFiles) != len(snapshot.Changes) {
		return errors.New("GitHub changed-file list is incomplete; refusing a partial review")
	}
	githubPatches := map[string]string{}
	for _, file := range githubFiles {
		githubPatches[file.Path] = file.Patch
	}
	for _, change := range snapshot.Changes {
		if _, ok := githubPatches[change.Path]; !ok {
			return errors.New("GitHub changed files differ from the fetched revision")
		}
	}
	var comments []InlineComment
	var summaryFindings []string
	comparison := append([]Discussion{}, history...)
	for i, f := range result.Findings {
		if err = ctx.Err(); err != nil {
			return err
		}
		inline, err := g.anchor(ctx, snapshot, f)
		if err != nil {
			return err
		}
		e.report(s, "reviewing", fmt.Sprintf("Verifying finding %d of %d", i+1, len(result.Findings)))
		text, err = e.session(ctx, g, j, verificationPrompt(inputPath, f, comparison))
		if err != nil {
			return err
		}
		verdict, err := parseVerification(text, comparison)
		if err != nil {
			return err
		}
		j.Candidates = append(j.Candidates, Candidate{Finding: f, Verdict: verdict.Verdict, Reason: verdict.Reason})
		if verdict.Verdict == "confirmed" {
			body := findingBody(f, verdict.Reason)
			if inline && diffAnchor(githubPatches[f.Path], f.Line, f.Side) {
				comments = append(comments, InlineComment{Path: f.Path, Line: f.Line, Side: f.Side, Body: body})
			} else {
				ref := j.PR.Head.SHA
				path := f.Path
				if f.Side == "LEFT" {
					ref = snapshot.MergeBase
					for _, c := range snapshot.Changes {
						if c.Path == f.Path && c.OldPath != "" {
							path = c.OldPath
						}
					}
				}
				summaryFindings = append(summaryFindings, body+fmt.Sprintf("\n\n[Source](https://%s/%s/blob/%s/%s#L%d)", e.config.GitHub.Host, e.config.GitHubRepo(), ref, escapePath(path), f.Line))
			}
			comparison = append(comparison, Discussion{ID: fmt.Sprintf("candidate:%d", i+1), Body: body, Path: f.Path})
		}
		if err = e.save(s); err != nil {
			return err
		}
	}
	confirmed := len(comments) + len(summaryFindings)
	body := fmt.Sprintf("## Review-bot\n\nReviewed `%s` against base `%s`.\n\n%s\n\n", j.PR.Head.SHA, j.PR.Base.SHA, j.Summary)
	if confirmed == 0 {
		body += "No new verified findings. This is a review summary, not an approval.\n"
	} else {
		body += fmt.Sprintf("%d verified finding(s); %d inline.\n", confirmed, len(comments))
	}
	if skipped := len(result.Findings) - confirmed; skipped > 0 {
		body += fmt.Sprintf("%d candidate(s) excluded after independent verification or duplicate comparison.\n", skipped)
	}
	for _, finding := range summaryFindings {
		body += "\n---\n\n" + finding + "\n"
	}
	body += "\nAutomated review by review-bot.\n\n" + marker(j.Key)
	j.Payload = &ReviewPayload{Commit: j.PR.Head.SHA, Event: "COMMENT", Body: body, Comments: comments}
	if len(body) > 60000 {
		return errors.New("review summary exceeds GitHub payload budget")
	}
	for _, c := range comments {
		if len(c.Body) > 60000 {
			return errors.New("inline finding exceeds GitHub payload budget")
		}
	}
	e.report(s, "verifying", "Checking PR revision and discussion before publication")
	current, err := e.source.pull(ctx, j.PR.Number)
	if err != nil {
		return err
	}
	if !eligible(e.config, current) || revisionKey(e.config, current) != j.Key {
		return errStale
	}
	if current.Body != j.PR.Body {
		return errors.New("PR description changed during review; retry against current context")
	}
	latest, err := e.source.discussion(ctx, j.PR.Number)
	if err != nil {
		return err
	}
	if discussionDigest(latest) != discussionDigest(history) {
		return errors.New("PR discussion changed during review; retry against current history")
	}
	if e.config.DryRun {
		j.Status = "dry_run"
		j.Failure = ""
		if err = e.save(s); err != nil {
			return err
		}
		e.log.Info("Dry-run review", "pr", j.PR.Number, "body", jsonContext(j.Payload))
		e.report(s, "complete", fmt.Sprintf("PR #%d dry run saved", j.PR.Number))
		return nil
	}
	// Last metadata read is adjacent to durable publication intent. commit_id binds a race to the reviewed revision.
	current, err = e.source.pull(ctx, j.PR.Number)
	if err != nil {
		return err
	}
	if !eligible(e.config, current) || revisionKey(e.config, current) != j.Key {
		return errStale
	}
	j.Actor = e.actor
	j.Status = "posting"
	if err = e.save(s); err != nil {
		return err
	}
	e.report(s, "publishing", fmt.Sprintf("Posting review on PR #%d", j.PR.Number))
	published, err := e.source.create(ctx, j.PR.Number, *j.Payload)
	if err != nil {
		var rejected *rejectedCreateError
		if errors.As(err, &rejected) {
			j.Status = "failed"
		}
		return err
	}
	if err = validatePublished(e.config, j, published); err != nil {
		return err
	}
	j.Status = "submitted"
	j.ReviewURL = published.URL
	j.Failure = ""
	if err = e.save(s); err != nil {
		return err
	}
	e.report(s, "complete", fmt.Sprintf("PR #%d review published", j.PR.Number))
	return nil
}
func escapePath(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
func findingBody(f Finding, reason string) string {
	return fmt.Sprintf("### [%s] %s\n\n%s\n\n**Trigger:** %s\n\n**Evidence:**\n%s\n\n**Independent verification:** %s", f.Severity, f.Title, f.Explanation, f.Trigger, strings.Join(f.Evidence, "\n\n"), reason)
}
