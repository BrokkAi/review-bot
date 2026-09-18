package main

import (
	bot "github.com/BrokkAi/review-bot"
	"github.com/BrokkAi/review-bot/internal/worker"
	"strings"
	"testing"
)

func TestReviewResultRequiresSubmittedExactRevision(t *testing.T) {
	req := worker.Request{PR: 7, BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40)}
	job := &bot.Job{Status: "stale", Failure: "PR revision changed or is no longer eligible"}
	job.PR.Number, job.PR.Base.SHA, job.PR.Head.SHA = req.PR, req.BaseSHA, req.HeadSHA
	job.Candidates = []bot.Candidate{{Verdict: "confirmed", Finding: bot.Finding{Title: "old concern", Severity: "P2"}}}
	saved := &bot.State{Jobs: []*bot.Job{job}}
	got := reviewResult(saved, req)
	if got.Complete || got.Status != "stale" || got.Detail != job.Failure || len(got.Findings) != 0 {
		t.Fatalf("stale result: %+v", got)
	}
	job.Status = "submitted"
	got = reviewResult(saved, req)
	if !got.Complete || len(got.Findings) != 1 {
		t.Fatalf("submitted result: %+v", got)
	}
	for id, text := range got.Findings {
		if got.Severities[id] != "P2" || !strings.HasPrefix(text, "[P2] ") {
			t.Fatalf("finding severity not reported: %q %+v", text, got.Severities)
		}
	}
	job.PR.Head.SHA = strings.Repeat("c", 40)
	got = reviewResult(saved, req)
	if got.Complete || len(got.Findings) != 0 {
		t.Fatalf("wrong revision certified: %+v", got)
	}
	job.PR.Head.SHA, job.DryRun = req.HeadSHA, true
	if reviewResult(saved, req).Complete {
		t.Fatal("dry run certified")
	}
}
