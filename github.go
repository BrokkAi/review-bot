package reviewbot

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BrokkAi/review-bot/internal/osrun"
)

type PRRef struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}
type PullRequest struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"html_url"`
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
	Locked bool   `json:"locked"`
	Merged bool   `json:"merged"`
	Head   PRRef  `json:"head"`
	Base   PRRef  `json:"base"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}
type Discussion struct {
	ID   string `json:"id"`
	Body string `json:"body"`
	URL  string `json:"url,omitempty"`
	Path string `json:"path,omitempty"`
}
type RemoteReview struct {
	ID     int64  `json:"id"`
	Body   string `json:"body"`
	Commit string `json:"commit_id"`
	State  string `json:"state"`
	URL    string `json:"html_url"`
	User   struct {
		Login string `json:"login"`
	} `json:"user"`
}
type InlineComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}
type ReviewPayload struct {
	Commit   string          `json:"commit_id"`
	Event    string          `json:"event"`
	Body     string          `json:"body"`
	Comments []InlineComment `json:"comments,omitempty"`
}
type PullFile struct {
	Path     string `json:"filename"`
	Previous string `json:"previous_filename"`
	Patch    string `json:"patch"`
}

type reviewSource interface {
	files(context.Context, int) ([]PullFile, error)
	pulls(context.Context) ([]PullRequest, error)
	pull(context.Context, int) (PullRequest, error)
	discussion(context.Context, int) ([]Discussion, error)
	reviews(context.Context, int) ([]RemoteReview, error)
	create(context.Context, int, ReviewPayload) (*RemoteReview, error)
}
type githubClient struct{ config Config }

func (g githubClient) path(s string) string { return "repos/" + g.config.GitHubRepo() + s }
func (g githubClient) request(ctx context.Context, path string, fields ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return osrun.Run(ctx, "", nil, append([]string{"gh", "api", "--hostname", g.config.GitHub.Host, path}, fields...)...)
}
func (g githubClient) api(ctx context.Context, path string, v any) error {
	text, err := g.request(ctx, path)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(text), v)
}
func pages[T any](ctx context.Context, g githubClient, path string, q url.Values) ([]T, error) {
	var all []T
	for page := 1; page <= 10000; page++ {
		q.Set("per_page", "100")
		q.Set("page", strconv.Itoa(page))
		var items []T
		if err := g.api(ctx, path+"?"+q.Encode(), &items); err != nil {
			return nil, err
		}
		if items == nil {
			return nil, errors.New("expected a GitHub array response")
		}
		all = append(all, items...)
		if len(items) < 100 {
			return all, nil
		}
	}
	return nil, errors.New("GitHub pagination limit exceeded; refusing incomplete history")
}
func (g githubClient) pulls(ctx context.Context) ([]PullRequest, error) {
	if g.config.PR > 0 {
		p, e := g.pull(ctx, g.config.PR)
		return []PullRequest{p}, e
	}
	items, err := pages[PullRequest](ctx, g, g.path("/pulls"), url.Values{"state": {"open"}, "base": {g.config.Branch}, "sort": {"created"}, "direction": {"asc"}})
	seen := map[int]bool{}
	for _, p := range items {
		if p.Number < 1 || seen[p.Number] {
			return nil, errors.New("invalid or unstable PR pagination")
		}
		seen[p.Number] = true
	}
	return items, err
}
func (g githubClient) pull(ctx context.Context, n int) (PullRequest, error) {
	var p PullRequest
	err := g.api(ctx, g.path(fmt.Sprintf("/pulls/%d", n)), &p)
	return p, err
}
func (g githubClient) files(ctx context.Context, n int) ([]PullFile, error) {
	files, err := pages[PullFile](ctx, g, g.path(fmt.Sprintf("/pulls/%d/files", n)), url.Values{})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, f := range files {
		if !validPath(f.Path) || seen[f.Path] {
			return nil, errors.New("invalid or unstable GitHub file list")
		}
		seen[f.Path] = true
	}
	return files, nil
}
func (g githubClient) reviews(ctx context.Context, n int) ([]RemoteReview, error) {
	return pages[RemoteReview](ctx, g, g.path(fmt.Sprintf("/pulls/%d/reviews", n)), url.Values{})
}
func (g githubClient) discussion(ctx context.Context, n int) ([]Discussion, error) {
	type comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		URL  string `json:"html_url"`
		Path string `json:"path"`
	}
	var out []Discussion
	for _, kind := range []string{"issue", "inline"} {
		path := g.path(fmt.Sprintf("/issues/%d/comments", n))
		if kind == "inline" {
			path = g.path(fmt.Sprintf("/pulls/%d/comments", n))
		}
		comments, err := pages[comment](ctx, g, path, url.Values{})
		if err != nil {
			return nil, err
		}
		for _, c := range comments {
			out = append(out, Discussion{ID: fmt.Sprintf("%s:%d", kind, c.ID), Body: c.Body, URL: c.URL, Path: c.Path})
		}
	}
	reviews, err := g.reviews(ctx, n)
	if err != nil {
		return nil, err
	}
	for _, r := range reviews {
		if r.State != "PENDING" {
			out = append(out, Discussion{ID: fmt.Sprintf("review:%d", r.ID), Body: r.Body, URL: r.URL})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	for i, d := range out {
		if strings.HasSuffix(d.ID, ":0") || i > 0 && out[i-1].ID == d.ID {
			return nil, errors.New("incomplete or unstable PR discussion")
		}
	}
	return out, nil
}
func eligible(c Config, p PullRequest) bool {
	if p.Number < 1 || p.State != "open" || p.Draft || p.Locked || p.Merged || p.Base.Ref != c.Branch || !strings.EqualFold(p.Base.Repo.FullName, c.GitHubRepo()) || !validCommit(p.Base.SHA) || !validCommit(p.Head.SHA) {
		return false
	}
	if c.PR > 0 && p.Number != c.PR {
		return false
	}
	labels := map[string]bool{}
	for _, l := range p.Labels {
		labels[strings.ToLower(l.Name)] = true
	}
	for _, l := range c.Labels {
		if !labels[strings.ToLower(l)] {
			return false
		}
	}
	for _, l := range c.ExcludeLabels {
		if labels[strings.ToLower(l)] {
			return false
		}
	}
	return true
}
func (g githubClient) actor(ctx context.Context) (string, error) {
	var u struct {
		Login string `json:"login"`
	}
	err := g.api(ctx, "user", &u)
	if err == nil && u.Login == "" {
		err = errors.New("GitHub did not identify the authenticated account")
	}
	return u.Login, err
}
func (g githubClient) create(ctx context.Context, n int, p ReviewPayload) (*RemoteReview, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	// Structured input preserves newlines and avoids gh's @file field interpretation.
	f, err := os.CreateTemp(g.config.StateDirectory, ".review-payload-*")
	if err != nil {
		return nil, &rejectedCreateError{err}
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return nil, &rejectedCreateError{err}
	}
	if err = f.Close(); err != nil {
		return nil, &rejectedCreateError{err}
	}
	out, runErr := g.request(ctx, g.path(fmt.Sprintf("/pulls/%d/reviews", n)), "--method", "POST", "--include", "--input", f.Name())
	return createdResponse(out, runErr)
}

type rejectedCreateError struct{ error }

func (e *rejectedCreateError) Unwrap() error { return e.error }
func createdResponse(out string, runErr error) (*RemoteReview, error) {
	reader := textproto.NewReader(bufio.NewReader(strings.NewReader(out)))
	line, err := reader.ReadLine()
	if err != nil {
		return nil, errors.Join(runErr, err)
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return nil, errors.New("missing review HTTP status")
	}
	_, _, ok := http.ParseHTTPVersion(fields[0])
	status, err := strconv.Atoi(fields[1])
	if !ok || err != nil || len(fields[1]) != 3 {
		return nil, errors.New("invalid review HTTP status")
	}
	if _, err = reader.ReadMIMEHeader(); err != nil {
		return nil, err
	}
	switch status {
	case 400, 401, 403, 404, 405, 409, 410, 411, 413, 414, 415, 422, 429:
		return nil, &rejectedCreateError{errors.Join(fmt.Errorf("GitHub rejected review (HTTP %d)", status), runErr)}
	}
	if runErr != nil {
		return nil, runErr
	}
	if status != 200 {
		return nil, fmt.Errorf("unexpected review HTTP status %d", status)
	}
	data, err := io.ReadAll(reader.R)
	if err != nil {
		return nil, err
	}
	var r RemoteReview
	err = json.Unmarshal(data, &r)
	return &r, err
}
func validatePublished(c Config, j *Job, r *RemoteReview) error {
	if r == nil || r.ID < 1 || r.Commit != j.PR.Head.SHA || r.State != "COMMENTED" || r.User.Login != j.Actor || r.Body != j.Payload.Body {
		return errors.New("published review does not match saved publication intent")
	}
	u, err := url.Parse(r.URL)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, c.GitHub.Host) || u.User != nil || u.RawQuery != "" || u.RawPath != "" || u.Path != fmt.Sprintf("/%s/pull/%d", c.GitHubRepo(), j.PR.Number) || u.Fragment != fmt.Sprintf("pullrequestreview-%d", r.ID) {
		return errors.New("published review URL does not match target PR")
	}
	return nil
}
