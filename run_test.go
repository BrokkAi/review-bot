package reviewbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/runner"
	"github.com/BrokkAi/review-bot/internal/osrun"
)

func canonicalTestDir(t *testing.T) string {
	t.Helper()
	p, e := canonical(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func writeTestFile(t *testing.T, p, s string) {
	t.Helper()
	if e := os.WriteFile(p, []byte(s), 0600); e != nil {
		t.Fatal(e)
	}
}
func localGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	env := map[string]string{"GIT_AUTHOR_NAME": "Fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid", "GIT_COMMITTER_NAME": "Fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null"}
	out, err := osrun.Run(context.Background(), dir, env, append([]string{"git"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func finding() Finding {
	return Finding{Severity: "P1", Title: "Zero input divides by zero", Explanation: "The new division panics when n is zero", Trigger: "Call value(0)", Evidence: []string{"The changed return statement evaluates 10 / 0 for value(0)"}, Path: "calc.go", Line: 3, Side: "RIGHT"}
}

type fakeSource struct {
	filesOverride []PullFile
	extraFiles    []PullFile
	prs           []PullRequest
	history       []Discussion
	published     map[int][]RemoteReview
	creates       int
	lost          bool
	createErr     error
	onPull        func(int)
	onDiscussion  func()
	onCreate      func(ReviewPayload)
}

func (f *fakeSource) pulls(context.Context) ([]PullRequest, error) {
	return append([]PullRequest{}, f.prs...), nil
}
func (f *fakeSource) pull(_ context.Context, n int) (PullRequest, error) {
	if f.onPull != nil {
		f.onPull(n)
	}
	for _, p := range f.prs {
		if p.Number == n {
			return p, nil
		}
	}
	return PullRequest{}, errors.New("missing PR")
}
func (f *fakeSource) discussion(context.Context, int) ([]Discussion, error) {
	if f.onDiscussion != nil {
		f.onDiscussion()
	}
	return append([]Discussion{}, f.history...), nil
}
func (f *fakeSource) files(context.Context, int) ([]PullFile, error) {
	if f.filesOverride != nil {
		return f.filesOverride, nil
	}
	return append([]PullFile{{Path: "calc.go", Patch: "@@ -1,4 +1,4 @@\n package fixture\n func value(n int) int {\n- return n\n+ return 10 / n\n }"}}, f.extraFiles...), nil
}
func (f *fakeSource) reviews(_ context.Context, n int) ([]RemoteReview, error) {
	return append([]RemoteReview{}, f.published[n]...), nil
}
func (f *fakeSource) create(_ context.Context, n int, p ReviewPayload) (*RemoteReview, error) {
	f.creates++
	if f.onCreate != nil {
		f.onCreate(p)
	}
	if f.createErr != nil {
		return nil, f.createErr
	}
	r := RemoteReview{ID: int64(f.creates), Body: p.Body, Commit: p.Commit, State: "COMMENTED", URL: fmt.Sprintf("https://github.com/o/r/pull/%d#pullrequestreview-%d", n, f.creates)}
	r.User.Login = "reviewer"
	f.published[n] = append(f.published[n], r)
	if f.lost {
		return nil, errors.New("connection lost after accepted POST")
	}
	return &r, nil
}

type agentFunc func(context.Context, string) (string, error)

func (f agentFunc) Execute(ctx context.Context, p string) (string, error) { return f(ctx, p) }

type fakeAgent struct {
	findings                             []Finding
	calls, investigations, verifications int
	verdict                              string
	err                                  error
	onCall                               func(Config, string)
	dirs                                 map[string]bool
}

func (a *fakeAgent) factory(c Config) Agent {
	return agentFunc(func(ctx context.Context, p string) (string, error) {
		a.calls++
		if a.onCall != nil {
			a.onCall(c, p)
		}
		if a.err != nil {
			return "", a.err
		}
		if a.dirs[c.Directory] {
			return "", errors.New("verification reused a worktree")
		}
		a.dirs[c.Directory] = true
		if strings.Contains(p, "REVIEW_VERIFY {") {
			a.verifications++
			var data struct{ Discussion []Discussion }
			if err := json.Unmarshal([]byte(strings.Split(p, "Context (data):\n")[1]), &data); err != nil {
				return "", err
			}
			checked := []string{}
			for _, d := range data.Discussion {
				checked = append(checked, d.ID)
			}
			r := Verification{Verdict: a.verdict, Reason: "Checked exact changed expression and complete discussion", Checked: checked}
			if r.Verdict == "duplicate" && len(checked) > 0 {
				r.Duplicate = checked[0]
			}
			b, _ := json.Marshal(r)
			return "REVIEW_VERIFY " + string(b), nil
		}
		a.investigations++
		b, _ := json.Marshal(ReviewResult{Summary: "Inspected calc.go and source-level zero-input behavior; no live services exercised", Findings: a.findings})
		return "REVIEW_RESULT " + string(b), nil
	})
}
func fixture(t *testing.T) (*engine, *State, *fakeSource, *fakeAgent, string) {
	t.Helper()
	dir := canonicalTestDir(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "global-state"))
	source := filepath.Join(dir, "source")
	remote := filepath.Join(dir, "remote.git")
	localGit(t, dir, "init", "--bare", remote)
	localGit(t, dir, "init", "-b", "main", source)
	writeTestFile(t, filepath.Join(source, "calc.go"), "package fixture\nfunc value(n int) int {\n return n\n}\n")
	localGit(t, source, "add", "calc.go")
	localGit(t, source, "commit", "-m", "base")
	base := localGit(t, source, "rev-parse", "HEAD")
	localGit(t, source, "remote", "add", "origin", remote)
	localGit(t, source, "push", "origin", "main")
	writeTestFile(t, filepath.Join(source, "calc.go"), "package fixture\nfunc value(n int) int {\n return 10 / n\n}\n")
	localGit(t, source, "commit", "-am", "introduce regression")
	head := localGit(t, source, "rev-parse", "HEAD")
	localGit(t, source, "push", "origin", "HEAD:refs/pull/1/head", "HEAD:refs/pull/2/head")
	c := DefaultConfig()
	c.Remote = remote
	c.Branch = "main"
	c.GitHub.Repo = "o/r"
	c.Directory = filepath.Join(dir, "worktrees")
	c.StateDirectory = filepath.Join(dir, "state")
	p := PullRequest{Number: 1, Title: "Change value", State: "open", URL: "https://github.com/o/r/pull/1"}
	p.Base.Ref = "main"
	p.Base.SHA = base
	p.Base.Repo.FullName = "o/r"
	p.Head.Ref = "feature"
	p.Head.SHA = head
	p.Head.Repo.FullName = "fork/r"
	f := &fakeSource{prs: []PullRequest{p}, published: map[int][]RemoteReview{}}
	a := &fakeAgent{findings: []Finding{finding()}, verdict: "confirmed", dirs: map[string]bool{}}
	e := &engine{config: c, source: f, actor: "reviewer", agent: a.factory, log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now}
	return e, newState(c), f, a, source
}
func TestReviewPublishesOneVerifiedInlineReviewAndRecoversStateLoss(t *testing.T) {
	e, s, f, a, _ := fixture(t)
	f.onCreate = func(p ReviewPayload) {
		saved, err := ReadState(e.config)
		if err != nil || saved.Jobs[0].Status != "posting" {
			t.Fatalf("publication before durable intent: %v", err)
		}
		if p.Event != "COMMENT" || len(p.Comments) != 1 || p.Comments[0].Line != 3 {
			t.Fatalf("bad payload %+v", p)
		}
	}
	if err := e.step(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || a.calls != 2 || s.Jobs[0].Status != "submitted" {
		t.Fatalf("unexpected result %+v calls %d creates %d", s, a.calls, f.creates)
	}
	if err := e.step(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if err := e.step(context.Background(), newState(e.config)); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || a.calls != 2 {
		t.Fatal("revision reviewed or posted twice")
	}
	dirs, err := os.ReadDir(e.config.Directory)
	if err != nil || len(dirs) != 0 {
		t.Fatalf("worktrees left behind: %v %v", dirs, err)
	}
}
func TestAmbiguousPublicationReconcilesAfterPRCloses(t *testing.T) {
	e, s, f, _, _ := fixture(t)
	f.lost = true
	if e.step(context.Background(), s) == nil || s.Jobs[0].Status != "posting" {
		t.Fatal("unknown response was not retained")
	}
	f.prs[0].State = "closed"
	f.prs[0].Merged = true
	saved, err := ReadState(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.step(context.Background(), saved); err != nil {
		t.Fatal(err)
	}
	if saved.Jobs[0].Status != "submitted" || f.creates != 1 {
		t.Fatal("failed recovery or duplicate POST")
	}
}
func TestUnknownOutcomeBlocksOnlyThatPRAndRetryCannotRepost(t *testing.T) {
	e, s, f, _, _ := fixture(t)
	f.createErr = errors.New("request timeout")
	if e.step(context.Background(), s) == nil {
		t.Fatal("timeout ignored")
	}
	if err := Retry(e.config); err == nil {
		t.Fatal("retry cleared unknown submission")
	}
	p := f.prs[0]
	p.Number = 2
	p.URL = "https://github.com/o/r/pull/2"
	f.prs = append(f.prs, p)
	f.createErr = nil
	if e.step(context.Background(), s) == nil {
		t.Fatal("unreconciled job not reported")
	}
	if f.creates != 2 || s.Jobs[1].Status != "submitted" {
		t.Fatal("uncertain PR starved another PR")
	}
}
func TestDryRunDoesNotConsumeLiveReview(t *testing.T) {
	e, s, f, a, _ := fixture(t)
	e.config.DryRun = true
	if err := e.step(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if f.creates != 0 || s.Jobs[0].Status != "dry_run" {
		t.Fatal("dry run published")
	}
	e.config.DryRun = false
	if err := e.step(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || a.calls != 4 || len(s.Jobs) != 2 {
		t.Fatal("dry run suppressed actual publication")
	}
}
func TestPRTransitionsDuringReviewNeverPublish(t *testing.T) {
	for _, mode := range []string{"head", "base", "draft", "closed", "label", "history"} {
		t.Run(mode, func(t *testing.T) {
			e, s, f, a, _ := fixture(t)
			e.config.ExcludeLabels = []string{"skip-review"}
			a.onCall = func(_ Config, _ string) {
				switch mode {
				case "head":
					f.prs[0].Head.SHA = strings.Repeat("b", 40)
				case "base":
					f.prs[0].Base.SHA = strings.Repeat("b", 40)
				case "draft":
					f.prs[0].Draft = true
				case "closed":
					f.prs[0].State = "closed"
				case "label":
					f.prs[0].Labels = append(f.prs[0].Labels, struct {
						Name string `json:"name"`
					}{"skip-review"})
				case "history":
					f.history = []Discussion{{ID: "issue:1", Body: "Already reported"}}
				}
			}
			_ = e.step(context.Background(), s)
			if f.creates != 0 {
				t.Fatal("published superseded result")
			}
		})
	}
}
func TestRejectedFindingsAreExcludedAndNoFindingsReviewIsHonest(t *testing.T) {
	for _, verdict := range []string{"duplicate", "invalid", "uncertain", "empty"} {
		t.Run(verdict, func(t *testing.T) {
			e, s, f, a, _ := fixture(t)
			f.history = []Discussion{{ID: "inline:1", Body: "Existing report"}}
			if verdict == "empty" {
				a.findings = []Finding{}
			} else {
				a.verdict = verdict
			}
			if err := e.step(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			p := s.Jobs[0].Payload
			if len(p.Comments) != 0 || !strings.Contains(p.Body, "No new verified findings") {
				t.Fatalf("bad clean summary: %+v", p)
			}
		})
	}
}
func TestFailedPRDoesNotStarveAnotherAndSetupErrorsDoNotSpendAttempts(t *testing.T) {
	e, s, f, a, _ := fixture(t)
	p := f.prs[0]
	p.Number = 2
	f.prs = append(f.prs, p)
	a.onCall = func(_ Config, _ string) {
		if e.current.PR.Number == 1 {
			a.err = errors.New("agent failure")
		} else {
			a.err = nil
		}
	}
	if e.step(context.Background(), s) == nil {
		t.Fatal("failed PR not reported")
	}
	if f.creates != 1 || s.Jobs[0].Status != "failed" || s.Jobs[1].Status != "submitted" {
		t.Fatal("queue did not continue")
	}
	e2, s2, _, a2, _ := fixture(t)
	a2.err = &runner.SetupError{Err: errors.New("missing executable")}
	if e2.step(context.Background(), s2) == nil || s2.Jobs[0].Tries != 0 {
		t.Fatal("setup error consumed attempt")
	}
}
func TestRejectsTrackedMutationsAndCancelsAgent(t *testing.T) {
	e, s, f, a, _ := fixture(t)
	a.onCall = func(c Config, _ string) { writeTestFile(t, filepath.Join(c.Directory, "calc.go"), "edited") }
	if e.step(context.Background(), s) == nil || f.creates != 0 {
		t.Fatal("modified evidence was published")
	}
	e2, s2, f2, _, _ := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	e2.agent = func(Config) Agent {
		return agentFunc(func(ctx context.Context, _ string) (string, error) { cancel(); <-ctx.Done(); return "", ctx.Err() })
	}
	if e2.step(ctx, s2) == nil || f2.creates != 0 {
		t.Fatal("cancellation not honored")
	}
}
func TestNewHeadQueuesASeparateReview(t *testing.T) {
	e, s, f, a, source := fixture(t)
	if err := e.step(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "extra.txt"), "additional change")
	localGit(t, source, "add", "extra.txt")
	localGit(t, source, "commit", "-m", "next revision")
	head := localGit(t, source, "rev-parse", "HEAD")
	localGit(t, source, "push", "origin", "HEAD:refs/pull/1/head")
	f.prs[0].Head.SHA = head
	f.extraFiles = []PullFile{{Path: "extra.txt", Patch: "@@ -0,0 +1 @@\n+additional change"}}
	f.history = []Discussion{{ID: "inline:1", Body: "Zero input divides by zero"}}
	a.verdict = "duplicate"
	if err := e.step(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if f.creates != 2 || len(s.Jobs) != 2 || len(s.Jobs[1].Payload.Comments) != 0 {
		t.Fatal("new revision or dedup failed")
	}
}

func TestMissingGitHubAnchorUsesSummaryAndIncompleteFileListStops(t *testing.T) {
	e, s, f, _, _ := fixture(t)
	f.filesOverride = []PullFile{{Path: "calc.go"}}
	if err := e.step(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	p := s.Jobs[0].Payload
	if len(p.Comments) != 0 || !strings.Contains(p.Body, "[Source](https://github.com/o/r/blob/") || !strings.Contains(p.Body, "#L3)") {
		t.Fatalf("missing summary source: %+v", p)
	}
	e2, s2, f2, _, _ := fixture(t)
	f2.filesOverride = []PullFile{}
	if e2.step(context.Background(), s2) == nil || f2.creates != 0 {
		t.Fatal("partial changed-file list accepted")
	}
}
func TestKnownRejectionCanRetryAndExhaustionIsVisible(t *testing.T) {
	e, s, f, _, _ := fixture(t)
	e.config.Attempts = 1
	f.createErr = &rejectedCreateError{errors.New("permission rejected")}
	if e.step(context.Background(), s) == nil || s.Jobs[0].Status != "failed" {
		t.Fatal("rejection not recorded")
	}
	s.Jobs[0].RetryAt = time.Time{}
	if e.step(context.Background(), s) == nil || f.creates != 1 {
		t.Fatal("attempt budget ignored")
	}
	if err := Retry(e.config); err != nil {
		t.Fatal(err)
	}
	saved, err := ReadState(e.config)
	if err != nil {
		t.Fatal(err)
	}
	f.createErr = nil
	if err = e.step(context.Background(), saved); err != nil {
		t.Fatal(err)
	}
	if saved.Jobs[0].Status != "submitted" || f.creates != 2 {
		t.Fatal("known rejection could not recover")
	}
}
