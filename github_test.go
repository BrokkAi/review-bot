package reviewbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeGH(t *testing.T) string {
	t.Helper()
	dir := canonicalTestDir(t)
	script := `#!/usr/bin/env python3
import json, os, sys, urllib.parse
args=sys.argv[1:]
url=args[3]
with open(os.environ['GH_TEST_LOG'],'a') as f: f.write(json.dumps(args)+'\n')
u=urllib.parse.urlparse(url); page=int(urllib.parse.parse_qs(u.query).get('page',['1'])[0])
if '--method' in args:
    with open(args[args.index('--input')+1]) as f: payload=json.load(f)
    with open(os.environ['GH_TEST_PAYLOAD'],'w') as f: json.dump(payload,f)
    print('HTTP/2.0 200 OK\r\nContent-Type: application/json\r\n\r\n'+json.dumps({'id':7,'body':payload['body'],'commit_id':payload['commit_id'],'state':'COMMENTED','html_url':'https://github.com/o/r/pull/1#pullrequestreview-7','user':{'login':'reviewer'}}))
elif u.path.endswith('/pulls'):
    print(json.dumps([{'number':n,'state':'open'} for n in (range(1,101) if page==1 else [101])]))
elif u.path.endswith('/pulls/1/files'):
    print(json.dumps([{'filename':'calc.go','patch':'@@ -1 +1 @@\n-old\n+new'}]))
elif u.path.endswith('/issues/1/comments'):
    print(json.dumps([{'id':1,'body':'discussion','html_url':'https://github.com/o/r/pull/1#issuecomment-1'}]))
elif u.path.endswith('/pulls/1/comments'):
    print(json.dumps([{'id':2,'body':'inline finding','path':'calc.go'}]))
elif u.path.endswith('/pulls/1/reviews'):
    print(json.dumps([{'id':3,'body':'prior review','state':'COMMENTED'}]))
elif u.path=='user':
    print(json.dumps({'login':'reviewer'}))
else:
    sys.exit(1)
`
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("GH_TEST_LOG", filepath.Join(dir, "calls"))
	t.Setenv("GH_TEST_PAYLOAD", filepath.Join(dir, "payload"))
	return dir
}
func TestGitHubPaginationDiscussionAndStructuredPublication(t *testing.T) {
	dir := fakeGH(t)
	c := DefaultConfig()
	c.Remote = "https://github.com/o/r.git"
	c.StateDirectory = dir
	g := githubClient{c}
	prs, err := g.pulls(context.Background())
	if err != nil || len(prs) != 101 {
		t.Fatalf("pagination: %d %v", len(prs), err)
	}
	history, err := g.discussion(context.Background(), 1)
	if err != nil || len(history) != 3 {
		t.Fatalf("discussion: %+v %v", history, err)
	}
	files, err := g.files(context.Background(), 1)
	if err != nil || len(files) != 1 || files[0].Path != "calc.go" {
		t.Fatal(files, err)
	}
	actor, err := g.actor(context.Background())
	if err != nil || actor != "reviewer" {
		t.Fatal(actor, err)
	}
	p := ReviewPayload{Commit: strings.Repeat("a", 40), Event: "COMMENT", Body: "@literal-file\n$(not shell) `literal`\n", Comments: []InlineComment{{Path: "calc.go", Line: 3, Side: "RIGHT", Body: "A\nB"}}}
	r, err := g.create(context.Background(), 1, p)
	if err != nil || r.Body != p.Body {
		t.Fatalf("create: %+v %v", r, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "payload"))
	if err != nil {
		t.Fatal(err)
	}
	var sent ReviewPayload
	if err = json.Unmarshal(data, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Event != "COMMENT" || sent.Comments[0].Body != "A\nB" {
		t.Fatalf("payload changed: %+v", sent)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if !strings.Contains(string(log), "base=master") {
		t.Fatal("PR listing did not filter base branch")
	}
}
func TestCreateResponseDistinguishesRejectionsFromUnknownOutcomes(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 422, 429} {
		_, err := createdResponse(fmt.Sprintf("HTTP/2.0 %d rejected\r\nX-Test: yes\r\n\r\n{}", status), errors.New("gh failed"))
		var rejected *rejectedCreateError
		if !errors.As(err, &rejected) {
			t.Fatalf("%d not known rejection: %v", status, err)
		}
	}
	for _, response := range []string{"", "junk", "HTTP/2.0 500 error\r\n\r\n{}", "HTTP/2.0 200 OK\r\n\r\nnot json", "HTTP/2.0 201 Created\r\n\r\n{}"} {
		_, err := createdResponse(response, nil)
		var rejected *rejectedCreateError
		if err == nil || errors.As(err, &rejected) {
			t.Fatalf("unknown response treated as safe retry: %q", response)
		}
	}
}
func TestEligibilityIncludesForksAndHonorsPRAndLabels(t *testing.T) {
	e, _, f, _, _ := fixture(t)
	p := f.prs[0]
	if !eligible(e.config, p) {
		t.Fatal("fork PR excluded")
	}
	c := e.config
	c.PR = 2
	if eligible(c, p) {
		t.Fatal("explicit PR ignored")
	}
	c = e.config
	c.Labels = []string{"ready"}
	if eligible(c, p) {
		t.Fatal("missing label accepted")
	}
	p.Labels = append(p.Labels, struct {
		Name string `json:"name"`
	}{"READY"})
	if !eligible(c, p) {
		t.Fatal("label match failed")
	}
	c.ExcludeLabels = []string{"ready"}
	if eligible(c, p) {
		t.Fatal("excluded label accepted")
	}
}
