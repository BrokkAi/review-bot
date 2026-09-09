package reviewbot

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type Progress struct {
	Phase, Task, Commit  string
	Attempt, MaxAttempts int
	WakeAt               time.Time
	Failure              string
	Reviews              []ReviewProgress
	Counts               ReviewCounts
}
type ReviewProgress struct{ ID, Title, Status, URL, Body string }
type ReviewCounts struct{ Found, Posted, Duplicates, Pending, DryRun, Skipped int }
type progressKey struct{}

// WithProgress receives owned snapshots synchronously; observers must return promptly.
func WithProgress(ctx context.Context, observe func(Progress)) context.Context {
	return context.WithValue(ctx, progressKey{}, observe)
}
func (e *engine) report(s *State, phase, task string) {
	if e.observe == nil {
		return
	}
	p := Progress{Phase: phase, Task: task, MaxAttempts: e.config.Attempts}
	if j := e.current; j != nil {
		p.Commit = j.PR.Head.SHA
		p.Attempt = j.Tries
		p.Failure = j.Failure
		p.WakeAt = j.RetryAt
	}
	if phase == "waiting" {
		p.WakeAt = e.now().Add(time.Duration(e.config.Poll))
	}
	for i, j := range s.Jobs {
		p.Counts.Found++
		switch j.Status {
		case "submitted":
			p.Counts.Posted++
		case "dry_run":
			p.Counts.DryRun++
		case "pending", "posting":
			p.Counts.Pending++
		case "failed":
			if j.Tries < e.config.Attempts {
				p.Counts.Pending++
			} else {
				p.Counts.Skipped++
			}
		default:
			p.Counts.Skipped++
		}
		for _, c := range j.Candidates {
			if c.Verdict == "duplicate" {
				p.Counts.Duplicates++
			}
		}
		if i < len(s.Jobs)-200 {
			continue
		}
		var details strings.Builder
		fmt.Fprintf(&details, "PR #%d: %s\n%s\n\nHead: %s\nBase: %s\nStatus: %s\n\n%s\n", j.PR.Number, j.PR.Title, j.PR.URL, j.PR.Head.SHA, j.PR.Base.SHA, j.Status, j.Summary)
		for _, c := range j.Candidates {
			fmt.Fprintf(&details, "\n[%s] %s (%s)\n%s:%d %s\n%s\n\n%s\n", c.Finding.Severity, c.Finding.Title, c.Verdict, c.Finding.Path, c.Finding.Line, c.Finding.Side, c.Finding.Explanation, c.Reason)
		}
		if j.Payload != nil {
			fmt.Fprintf(&details, "\nReview summary\n%s\n", j.Payload.Body)
		}
		if j.Failure != "" {
			fmt.Fprintf(&details, "\nFailure\n%s", j.Failure)
		}
		p.Reviews = append(p.Reviews, ReviewProgress{ID: fmt.Sprintf("%s:%t", j.Key, j.DryRun), Title: fmt.Sprintf("#%d %s", j.PR.Number, j.PR.Title), Status: j.Status, URL: j.ReviewURL, Body: details.String()})
	}
	e.observe(p)
}
