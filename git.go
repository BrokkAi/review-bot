package reviewbot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BrokkAi/review-bot/internal/osrun"
)

type checkout struct{ config Config }
type Change struct {
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
	Status  string `json:"status"`
}
type Snapshot struct {
	PR           PullRequest  `json:"pr"`
	MergeBase    string       `json:"merge_base"`
	Diff         string       `json:"diff"`
	Changes      []Change     `json:"changes"`
	Discussion   []Discussion `json:"discussion"`
	Focus        string       `json:"focus,omitempty"`
	Instructions []string     `json:"instruction_files"`
}

func (g checkout) repository() string {
	return filepath.Join(g.config.StateDirectory, "repository.git")
}
func (g checkout) git(ctx context.Context, args ...string) (string, error) {
	return osrun.Run(ctx, "", map[string]string{"GIT_TERMINAL_PROMPT": "0"}, append([]string{"git", "--git-dir", g.repository()}, args...)...)
}
func (g checkout) open(ctx context.Context) error {
	if err := os.MkdirAll(g.config.StateDirectory, 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(g.config.Directory, 0700); err != nil {
		return err
	}
	if _, err := os.Stat(g.repository()); errors.Is(err, os.ErrNotExist) {
		if _, err = osrun.Run(ctx, "", nil, "git", "init", "--bare", g.repository()); err != nil {
			return err
		}
		if _, err = g.git(ctx, "remote", "add", "origin", g.config.Remote); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	bare, err := g.git(ctx, "rev-parse", "--is-bare-repository")
	if err != nil {
		return err
	}
	remote, err := g.git(ctx, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if bare != "true" || remote != g.config.Remote {
		return errors.New("private repository does not match configured origin")
	}
	_, err = g.git(ctx, "check-ref-format", "refs/heads/"+g.config.Branch)
	return err
}

var errStale = errors.New("PR revision changed or is no longer eligible")

func (g checkout) snapshot(ctx context.Context, p PullRequest, discussion []Discussion) (Snapshot, error) {
	s := Snapshot{PR: p, Discussion: discussion, Focus: g.config.Focus, Instructions: g.config.InstructionFiles}
	baseRef := fmt.Sprintf("refs/review-bot/%d/base", p.Number)
	headRef := fmt.Sprintf("refs/review-bot/%d/head", p.Number)
	// GitHub can retain an older base SHA on an open PR after the target
	// branch advances. Fetch that exact commit, not the current branch tip.
	// The pull ref must still match the reported head to reject fetch races.
	if !validCommit(p.Base.SHA) || !validCommit(p.Head.SHA) {
		return s, errStale
	}
	if _, err := g.git(ctx, "fetch", "--no-tags", "origin", "+"+p.Base.SHA+":"+baseRef, fmt.Sprintf("+refs/pull/%d/head:%s", p.Number, headRef)); err != nil {
		return s, err
	}
	for ref, want := range map[string]string{baseRef: p.Base.SHA, headRef: p.Head.SHA} {
		got, err := g.git(ctx, "rev-parse", ref)
		if err != nil {
			return s, err
		}
		if got != want {
			return s, errStale
		}
	}
	merge, err := g.git(ctx, "merge-base", p.Base.SHA, p.Head.SHA)
	if err != nil {
		return s, err
	}
	s.MergeBase = merge
	diff, err := g.git(ctx, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--find-renames", merge, p.Head.SHA, "--")
	if err != nil {
		return s, err
	}
	s.Diff = diff
	names, err := g.git(ctx, "diff", "--name-status", "-z", "--find-renames", merge, p.Head.SHA, "--")
	if err != nil {
		return s, err
	}
	if names != "" {
		parts := strings.Split(strings.TrimSuffix(names, "\x00"), "\x00")
		for len(parts) > 0 {
			if len(parts) < 2 {
				return s, errors.New("incomplete changed-file list")
			}
			c := Change{Status: parts[0], Path: parts[1]}
			parts = parts[2:]
			if strings.HasPrefix(c.Status, "R") || strings.HasPrefix(c.Status, "C") {
				if len(parts) == 0 {
					return s, errors.New("incomplete rename")
				}
				c.OldPath = c.Path
				c.Path = parts[0]
				parts = parts[1:]
			}
			if !validPath(c.Path) || c.OldPath != "" && !validPath(c.OldPath) {
				return s, errors.New("unsupported changed-file path")
			}
			s.Changes = append(s.Changes, c)
		}
	}
	return s, nil
}
func (g checkout) worktree(ctx context.Context, head string) (string, func() error, error) {
	dir, err := os.MkdirTemp(g.config.Directory, "review-")
	if err != nil {
		return "", nil, err
	}
	if err = os.Remove(dir); err != nil {
		return "", nil, err
	}
	if _, err = g.git(ctx, "worktree", "add", "--detach", "--", dir, head); err != nil {
		return "", nil, err
	}
	cleanup := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := g.git(ctx, "worktree", "remove", "--force", "--", dir)
		return err
	}
	return dir, cleanup, nil
}
func verifyWorktree(ctx context.Context, dir, head string) error {
	got, err := osrun.Run(ctx, dir, nil, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if got != head {
		return errors.New("agent changed review commit")
	}
	if _, err = osrun.Run(ctx, dir, nil, "git", "diff", "--exit-code", "HEAD", "--"); err != nil {
		return fmt.Errorf("agent modified tracked source: %w", err)
	}
	return nil
}

var hunk = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func diffAnchor(patch string, line int, side string) bool {
	old, newLine := 0, 0
	inHunk := false
	for _, text := range strings.Split(patch, "\n") {
		if m := hunk.FindStringSubmatch(text); m != nil {
			old, _ = strconv.Atoi(m[1])
			newLine, _ = strconv.Atoi(m[3])
			inHunk = true
			continue
		}
		if strings.HasPrefix(text, "diff --git ") {
			inHunk = false
			continue
		}
		if !inHunk || len(text) == 0 {
			continue
		}
		switch text[0] {
		case ' ':
			if side == "LEFT" && line == old || side == "RIGHT" && line == newLine {
				return true
			}
			old++
			newLine++
		case '-':
			if side == "LEFT" && line == old {
				return true
			}
			old++
		case '+':
			if side == "RIGHT" && line == newLine {
				return true
			}
			newLine++
		}
	}
	return false
}
func (g checkout) anchor(ctx context.Context, s Snapshot, f Finding) (bool, error) {
	var change *Change
	for _, c := range s.Changes {
		if c.Path == f.Path {
			copy := c
			change = &copy
			break
		}
	}
	if change == nil {
		return false, errors.New("finding is not anchored to a changed file")
	}
	ref := s.PR.Head.SHA
	path := f.Path
	if f.Side == "LEFT" {
		ref = s.MergeBase
		if change.OldPath != "" {
			path = change.OldPath
		}
	}
	source, err := osrun.RunRaw(ctx, "", nil, "git", "--git-dir", g.repository(), "show", ref+":"+path)
	if err != nil {
		return false, fmt.Errorf("finding source does not exist: %w", err)
	}
	lineCount := strings.Count(source, "\n")
	if source != "" && !strings.HasSuffix(source, "\n") {
		lineCount++
	}
	if f.Line > lineCount {
		return false, errors.New("finding line exceeds source file")
	}
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--find-renames", "--unified=3", s.MergeBase, s.PR.Head.SHA, "--", f.Path}
	if change.OldPath != "" {
		args = append(args, change.OldPath)
	}
	patch, err := g.git(ctx, args...)
	if err != nil {
		return false, err
	}
	return diffAnchor(patch, f.Line, f.Side), nil
}
