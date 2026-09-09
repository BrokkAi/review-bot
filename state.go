package reviewbot

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Candidate struct {
	Finding Finding `json:"finding"`
	Verdict string  `json:"verdict"`
	Reason  string  `json:"reason"`
}
type Job struct {
	Key        string         `json:"key"`
	PR         PullRequest    `json:"pr"`
	DryRun     bool           `json:"dry_run"`
	Status     string         `json:"status"`
	Tries      int            `json:"tries"`
	RetryAt    time.Time      `json:"retry_at,omitempty"`
	Failure    string         `json:"failure,omitempty"`
	Summary    string         `json:"summary,omitempty"`
	Candidates []Candidate    `json:"candidates,omitempty"`
	Payload    *ReviewPayload `json:"payload,omitempty"`
	Actor      string         `json:"actor,omitempty"`
	ReviewURL  string         `json:"review_url,omitempty"`
}
type State struct {
	Format    int    `json:"format"`
	Remote    string `json:"remote"`
	Branch    string `json:"branch"`
	Directory string `json:"directory"`
	Repo      string `json:"repo"`
	Host      string `json:"host"`
	Jobs      []*Job `json:"jobs"`
}

func newState(c Config) *State {
	return &State{Format: 1, Remote: c.Remote, Branch: c.Branch, Directory: c.Directory, Repo: c.GitHubRepo(), Host: c.GitHub.Host, Jobs: []*Job{}}
}
func revisionKey(c Config, p PullRequest) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s/%s#%d:%s:%s:%s", strings.ToLower(c.GitHub.Host), strings.ToLower(c.GitHubRepo()), p.Number, p.Base.Ref, p.Base.SHA, p.Head.SHA))))
}
func marker(key string) string { return "<!-- review-bot:v1:" + key + " -->" }
func validCommit(s string) bool {
	return (len(s) == 40 || len(s) == 64) && strings.Trim(s, "0123456789abcdef") == ""
}
func ReadState(c Config) (*State, error) {
	data, err := os.ReadFile(filepath.Join(c.StateDirectory, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err = json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("invalid saved state: %w", err)
	}
	if s.Format != 1 || s.Remote != c.Remote || s.Branch != c.Branch || s.Directory != c.Directory || s.Repo != c.GitHubRepo() || s.Host != c.GitHub.Host {
		return nil, errors.New("state version or repository identity differs from configuration")
	}
	seen := map[string]bool{}
	for _, j := range s.Jobs {
		if j == nil || j.Key != revisionKey(c, j.PR) || !validCommit(j.PR.Head.SHA) || !validCommit(j.PR.Base.SHA) || j.PR.Number < 1 || j.Tries < 0 {
			return nil, errors.New("invalid saved review job")
		}
		key := fmt.Sprintf("%s:%t", j.Key, j.DryRun)
		if seen[key] {
			return nil, errors.New("duplicate saved review job")
		}
		seen[key] = true
		switch j.Status {
		case "pending", "failed", "posting", "submitted", "dry_run", "stale":
		default:
			return nil, errors.New("invalid review job status")
		}
		if j.Status == "posting" || j.Status == "submitted" {
			if j.DryRun || j.Actor == "" || j.Payload == nil || j.Payload.Commit != j.PR.Head.SHA || j.Payload.Event != "COMMENT" || !strings.Contains(j.Payload.Body, marker(j.Key)) {
				return nil, errors.New("invalid saved publication intent")
			}
		}
	}
	return &s, nil
}
func writeState(c Config, s *State) error {
	if err := os.MkdirAll(c.StateDirectory, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(c.StateDirectory, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(c.StateDirectory, "state.json")); err != nil {
		return err
	}
	d, err := os.Open(c.StateDirectory)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another process holds %s: %w", path, err)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
func lockConfig(c Config) (func(), error) {
	base, err := stateHome()
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.ToLower(c.GitHub.Host+"/"+c.GitHubRepo()))))
	var releases []func()
	for _, path := range []string{filepath.Join(base, "review-bot", "locks", key+".lock"), filepath.Join(c.StateDirectory, "daemon.lock"), c.Directory + ".review-bot.lock"} {
		release, err := lockFile(path)
		if err != nil {
			for _, f := range releases {
				f()
			}
			return nil, err
		}
		releases = append(releases, release)
	}
	return func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}, nil
}
func Retry(c Config) error {
	release, err := lockConfig(c)
	if err != nil {
		return err
	}
	defer release()
	s, err := ReadState(c)
	if err != nil {
		return err
	}
	if s == nil {
		return errors.New("no saved reviews to retry")
	}
	count := 0
	for _, j := range s.Jobs {
		if c.PR > 0 && c.PR != j.PR.Number || j.DryRun != c.DryRun {
			continue
		}
		if j.Status == "posting" {
			return errors.New("review publication is uncertain; run once to reconcile, never blindly repost")
		}
		if j.Status == "failed" {
			j.Tries = 0
			j.RetryAt = time.Time{}
			j.Status = "pending"
			count++
		}
	}
	if count == 0 {
		return errors.New("no failed reviews to retry")
	}
	return writeState(c, s)
}
