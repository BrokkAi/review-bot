package reviewbot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiffAnchorsHandleBothSidesAndMultipleHunks(t *testing.T) {
	patch := "diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -2,3 +2,3 @@\n context\n-old\n+new\n context\n@@ -20 +20,2 @@\n old\n+added\n"
	for _, point := range []struct {
		line int
		side string
		want bool
	}{{3, "LEFT", true}, {3, "RIGHT", true}, {21, "RIGHT", true}, {21, "LEFT", false}, {10, "RIGHT", false}} {
		if got := diffAnchor(patch, point.line, point.side); got != point.want {
			t.Fatalf("anchor %+v = %v", point, got)
		}
	}
}
func TestSnapshotsUseExactForkRefsAndRejectFetchRaces(t *testing.T) {
	e, _, f, _, source := fixture(t)
	g := checkout{e.config}
	if err := g.open(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := localGit(t, source, "status", "--porcelain")
	s, err := g.snapshot(context.Background(), f.prs[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.MergeBase != f.prs[0].Base.SHA || len(s.Changes) != 1 || !strings.Contains(s.Diff, "+ return 10 / n") {
		t.Fatalf("wrong snapshot: %+v", s)
	}
	if before != localGit(t, source, "status", "--porcelain") {
		t.Fatal("operator checkout changed")
	}
	p := f.prs[0]
	p.Head.SHA = strings.Repeat("b", 40)
	if _, err = g.snapshot(context.Background(), p, nil); err != errStale {
		t.Fatalf("fetch race accepted: %v", err)
	}
}
func TestRenameDeletedAndOutOfDiffLocations(t *testing.T) {
	e, _, f, _, source := fixture(t)
	// Build a richer branch at the existing base, entirely inside this test's repository.
	localGit(t, source, "switch", "--detach", f.prs[0].Base.SHA)
	content := "\n\n" + strings.Repeat("stable line\n", 30)
	writeTestFile(t, filepath.Join(source, "old.txt"), content)
	writeTestFile(t, filepath.Join(source, "deleted.txt"), "removed\n")
	localGit(t, source, "add", "old.txt", "deleted.txt")
	localGit(t, source, "commit", "-m", "source files")
	base := localGit(t, source, "rev-parse", "HEAD")
	localGit(t, source, "push", "origin", "HEAD:refs/heads/main")
	if err := os.Rename(filepath.Join(source, "old.txt"), filepath.Join(source, "new.txt")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(source, "new.txt"), "\n\nchanged line\n"+strings.Repeat("stable line\n", 29))
	if err := os.Remove(filepath.Join(source, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	localGit(t, source, "add", "--", "old.txt", "new.txt", "deleted.txt")
	localGit(t, source, "commit", "-m", "rename and delete")
	head := localGit(t, source, "rev-parse", "HEAD")
	localGit(t, source, "push", "--force", "origin", "HEAD:refs/pull/1/head")
	p := f.prs[0]
	p.Base.SHA = base
	p.Head.SHA = head
	g := checkout{e.config}
	if err := g.open(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := g.snapshot(context.Background(), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path, side string
		line       int
		inline     bool
	}{{"new.txt", "RIGHT", 3, true}, {"new.txt", "LEFT", 3, true}, {"new.txt", "RIGHT", 30, false}, {"deleted.txt", "LEFT", 1, true}} {
		finding := finding()
		finding.Path = test.path
		finding.Side = test.side
		finding.Line = test.line
		got, err := g.anchor(context.Background(), s, finding)
		if err != nil || got != test.inline {
			t.Fatalf("%+v = %v, %v", test, got, err)
		}
	}
}

func TestFindingCannotPointPastTrailingNewline(t *testing.T) {
	e, _, f, _, _ := fixture(t)
	g := checkout{e.config}
	if err := g.open(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := g.snapshot(context.Background(), f.prs[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate := finding()
	candidate.Line = 5
	if _, err = g.anchor(context.Background(), s, candidate); err == nil {
		t.Fatal("accepted nonexistent line after final newline")
	}
}

func TestSnapshotUsesReportedBaseWhenTargetBranchAdvanced(t *testing.T) {
	e, _, f, _, source := fixture(t)
	p := f.prs[0]
	localGit(t, source, "switch", "--detach", p.Base.SHA)
	writeTestFile(t, filepath.Join(source, "unrelated.txt"), "target branch advanced\n")
	localGit(t, source, "add", "unrelated.txt")
	localGit(t, source, "commit", "-m", "advance target branch")
	localGit(t, source, "push", "origin", "HEAD:refs/heads/main")
	g := checkout{e.config}
	if err := g.open(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := g.snapshot(context.Background(), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.MergeBase != p.Base.SHA || len(snapshot.Changes) != 1 || snapshot.Changes[0].Path != "calc.go" {
		t.Fatalf("wrong revision reviewed: %+v", snapshot)
	}
}
