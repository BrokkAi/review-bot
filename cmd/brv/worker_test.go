package main

import (
	"strings"
	"testing"

	bot "github.com/BrokkAi/review-bot"
)

func workerFinding(severity, title string) bot.Finding {
	return bot.Finding{
		Severity: severity, Title: title,
		Explanation: "The new division panics when n is zero", Trigger: "Call value(0)",
		Evidence: []string{"The changed return statement evaluates 10 / 0 for value(0)"},
		Path:     "calc.go", Line: 3, Side: "RIGHT",
	}
}

func TestWorkerForwardsOnlyMergeBlocking(t *testing.T) {
	const base, head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	job := &bot.Job{PR: bot.PullRequest{Number: 1}, Status: "submitted"}
	job.PR.Head.SHA = head
	job.PR.Base.SHA = base
	job.Candidates = []bot.Candidate{
		{Finding: workerFinding("P1", "Zero input divides by zero"), Verdict: "confirmed", Reason: "reproduced"},
		{Finding: workerFinding("P3", "Comment wording nit"), Verdict: "confirmed", Reason: "reproduced"},
		{Finding: workerFinding("P1", "Zero input divides by zero"), Verdict: "duplicate", Reason: "same root cause"},
		{Finding: workerFinding("P2", "Possible overflow"), Verdict: "uncertain", Reason: "could not reproduce"},
		{Finding: workerFinding("P1", "Stale claim"), Verdict: "invalid", Reason: "intended behavior"},
	}
	complete, findings := reviewFindings([]*bot.Job{job}, 1, base, head)
	if !complete {
		t.Fatal("matching submitted revision was not marked complete")
	}
	if len(findings) != 1 {
		t.Fatalf("worker forwarded %d findings, want 1 merge-blocking", len(findings))
	}
	for _, body := range findings {
		if !strings.Contains(body, "Zero input divides by zero") || !strings.Contains(body, "[P1]") {
			t.Fatalf("wrong finding forwarded: %q", body)
		}
	}
}

func TestWorkerEmptyWhenNothingBlocks(t *testing.T) {
	const base, head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	job := &bot.Job{PR: bot.PullRequest{Number: 1}, Status: "submitted"}
	job.PR.Head.SHA = head
	job.PR.Base.SHA = base
	job.Candidates = []bot.Candidate{
		{Finding: workerFinding("P3", "Comment wording nit"), Verdict: "confirmed", Reason: "reproduced"},
	}
	complete, findings := reviewFindings([]*bot.Job{job}, 1, base, head)
	if !complete || len(findings) != 0 {
		t.Fatalf("advisory-only revision must complete with no blocking findings, got complete=%t n=%d", complete, len(findings))
	}
}
