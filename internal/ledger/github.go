package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/go-github/v76/github"
)

// GitHub implements Ledger against the GitHub REST API.
type GitHub struct {
	Owner  string
	Client *github.Client
	Branch string // default "main"
}

func NewGitHub(owner, token string) *GitHub {
	c := github.NewClient(newHTTPClient())
	if token != "" {
		c = c.WithAuthToken(token)
	}
	return &GitHub{Owner: owner, Client: c, Branch: "main"}
}

func (g *GitHub) branch() string {
	if g.Branch == "" {
		return "main"
	}
	return g.Branch
}

func (g *GitHub) LastCommit(ctx context.Context, repo, path string) (string, error) {
	cs, _, err := g.Client.Repositories.ListCommits(ctx, g.Owner, repo, &github.CommitsListOptions{
		SHA: g.branch(), Path: path, ListOptions: github.ListOptions{PerPage: 1},
	})
	if err != nil {
		return "", fmt.Errorf("last commit for %s/%s:%s: %w", g.Owner, repo, path, err)
	}
	if len(cs) == 0 {
		return "", fmt.Errorf("no commits touch %s in %s", path, repo)
	}
	return cs[0].GetSHA(), nil
}

func (g *GitHub) File(ctx context.Context, repo, path, ref string) ([]byte, bool, error) {
	fc, _, resp, err := g.Client.Repositories.GetContents(ctx, g.Owner, repo, path, &github.RepositoryContentGetOptions{Ref: ref})
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get %s/%s:%s@%s: %w", g.Owner, repo, path, ref, err)
	}
	s, err := fc.GetContent()
	if err != nil {
		return nil, false, err
	}
	return []byte(s), true, nil
}

func (g *GitHub) IsAncestorOrEqual(ctx context.Context, repo, a, b string) (bool, error) {
	if a == b || strings.HasPrefix(b, a) || strings.HasPrefix(a, b) {
		return true, nil
	}
	cmp, _, err := g.Client.Repositories.CompareCommits(ctx, g.Owner, repo, a, b, &github.ListOptions{PerPage: 1})
	if err != nil {
		return false, err
	}
	switch cmp.GetStatus() {
	case "ahead", "identical":
		return true, nil
	}
	return false, nil
}

func (g *GitHub) StartDeployment(ctx context.Context, repo, sha string, production bool, p Payload) (int64, error) {
	d, _, err := g.Client.Repositories.CreateDeployment(ctx, g.Owner, repo, &github.DeploymentRequest{
		Ref:                   github.Ptr(sha),
		Task:                  github.Ptr("gate"),
		AutoMerge:             github.Ptr(false),
		RequiredContexts:      &[]string{}, // the gate is the check; don't wait on commit statuses
		Payload:               p,
		Environment:           github.Ptr(p.Tier),
		Description:           github.Ptr(fmt.Sprintf("evidence gate: %s %s on %s", p.App, p.Version, p.Cluster)),
		ProductionEnvironment: github.Ptr(production),
		TransientEnvironment:  github.Ptr(false),
	})
	if err != nil {
		return 0, fmt.Errorf("create deployment in %s/%s (%s): %w", g.Owner, repo, p.Tier, err)
	}
	_, _, err = g.Client.Repositories.CreateDeploymentStatus(ctx, g.Owner, repo, d.GetID(), &github.DeploymentStatusRequest{
		State: github.Ptr("in_progress"), Description: github.Ptr("running evidence checks"), Environment: github.Ptr(p.Tier),
	})
	return d.GetID(), err
}

func (g *GitHub) FinishDeployment(ctx context.Context, repo, env string, id int64, success bool, description, logURL string) error {
	state := "failure"
	if success {
		state = "success"
	}
	if len(description) > 140 {
		description = description[:137] + "..."
	}
	req := &github.DeploymentStatusRequest{
		State: github.Ptr(state), Description: github.Ptr(description), Environment: github.Ptr(env),
		AutoInactive: github.Ptr(false),
	}
	if logURL != "" {
		req.LogURL = github.Ptr(logURL)
	}
	_, _, err := g.Client.Repositories.CreateDeploymentStatus(ctx, g.Owner, repo, id, req)
	return err
}

func (g *GitHub) LatestDeployment(ctx context.Context, repo, env, app, clusterName string, onlySuccess bool) (*Deployment, error) {
	opt := &github.DeploymentsListOptions{Environment: env, Task: "gate", ListOptions: github.ListOptions{PerPage: 50}}
	for page := 0; page < 4; page++ {
		ds, resp, err := g.Client.Repositories.ListDeployments(ctx, g.Owner, repo, opt)
		if err != nil {
			return nil, err
		}
		for _, d := range ds { // newest first
			var p Payload
			if err := json.Unmarshal(d.Payload, &p); err != nil || p.App != app || p.Cluster != clusterName {
				continue
			}
			sts, _, err := g.Client.Repositories.ListDeploymentStatuses(ctx, g.Owner, repo, d.GetID(), &github.ListOptions{PerPage: 1})
			if err != nil {
				return nil, err
			}
			out := &Deployment{ID: d.GetID(), SHA: d.GetSHA(), Payload: p, URL: d.GetURL()}
			if len(sts) > 0 {
				out.State, out.LogURL = sts[0].GetState(), sts[0].GetLogURL()
			}
			if onlySuccess && out.State != "success" {
				continue
			}
			return out, nil
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return nil, nil
}

func (g *GitHub) PostEvidence(ctx context.Context, repo, sha, markdown string, ev Evidence) (string, error) {
	c, _, err := g.Client.Repositories.CreateComment(ctx, g.Owner, repo, sha, &github.RepositoryComment{
		Body: github.Ptr(EmbedEvidence(markdown, ev)),
	})
	if err != nil {
		return "", fmt.Errorf("comment evidence on %s: %w", sha, err)
	}
	return c.GetHTMLURL(), nil
}

var commentIDRE = regexp.MustCompile(`#commitcomment-(\d+)$`)

func (g *GitHub) ReadEvidence(ctx context.Context, repo, url string) (*Evidence, error) {
	m := commentIDRE.FindStringSubmatch(url)
	if m == nil {
		return nil, fmt.Errorf("not a commit comment URL: %s", url)
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	c, _, err := g.Client.Repositories.GetComment(ctx, g.Owner, repo, id)
	if err != nil {
		return nil, err
	}
	return ExtractEvidence(c.GetBody())
}

func (g *GitHub) OpenPromotionPR(ctx context.Context, pr PR) (string, error) {
	base := g.branch()
	mainRef, _, err := g.Client.Git.GetRef(ctx, g.Owner, pr.Repo, "heads/"+base)
	if err != nil {
		return "", fmt.Errorf("get %s: %w", base, err)
	}
	head := mainRef.GetObject().GetSHA()
	// (Re)point the branch at main so the PR is exactly one file change on top.
	ref := "heads/" + pr.Branch
	if _, resp, err := g.Client.Git.GetRef(ctx, g.Owner, pr.Repo, ref); err != nil {
		if resp == nil || resp.StatusCode != http.StatusNotFound {
			return "", err
		}
		if _, _, err := g.Client.Git.CreateRef(ctx, g.Owner, pr.Repo, github.CreateRef{Ref: "refs/" + ref, SHA: head}); err != nil {
			return "", fmt.Errorf("create branch %s: %w", pr.Branch, err)
		}
	} else if _, _, err := g.Client.Git.UpdateRef(ctx, g.Owner, pr.Repo, ref, github.UpdateRef{SHA: head, Force: github.Ptr(true)}); err != nil {
		return "", fmt.Errorf("reset branch %s: %w", pr.Branch, err)
	}
	fc, _, resp, err := g.Client.Repositories.GetContents(ctx, g.Owner, pr.Repo, pr.Path, &github.RepositoryContentGetOptions{Ref: pr.Branch})
	opts := &github.RepositoryContentFileOptions{
		Message: github.Ptr(pr.CommitMessage), Content: pr.Content, Branch: github.Ptr(pr.Branch),
	}
	switch {
	case err == nil:
		opts.SHA = fc.SHA
		_, _, err = g.Client.Repositories.UpdateFile(ctx, g.Owner, pr.Repo, pr.Path, opts)
	case resp != nil && resp.StatusCode == http.StatusNotFound:
		_, _, err = g.Client.Repositories.CreateFile(ctx, g.Owner, pr.Repo, pr.Path, opts)
	}
	if err != nil {
		return "", fmt.Errorf("commit %s on %s: %w", pr.Path, pr.Branch, err)
	}
	prs, _, err := g.Client.PullRequests.List(ctx, g.Owner, pr.Repo, &github.PullRequestListOptions{
		State: "open", Head: g.Owner + ":" + pr.Branch, Base: base,
	})
	if err != nil {
		return "", err
	}
	var p *github.PullRequest
	if len(prs) > 0 {
		p, _, err = g.Client.PullRequests.Edit(ctx, g.Owner, pr.Repo, prs[0].GetNumber(), &github.PullRequest{
			Title: github.Ptr(pr.Title), Body: github.Ptr(pr.Body),
		})
	} else {
		p, _, err = g.Client.PullRequests.Create(ctx, g.Owner, pr.Repo, &github.NewPullRequest{
			Title: github.Ptr(pr.Title), Head: github.Ptr(pr.Branch), Base: github.Ptr(base), Body: github.Ptr(pr.Body),
		})
	}
	if err != nil {
		return "", fmt.Errorf("open PR: %w", err)
	}
	if len(pr.Labels) > 0 {
		// Adding a label that doesn't exist yet creates it.
		if _, _, err := g.Client.Issues.AddLabelsToIssue(ctx, g.Owner, pr.Repo, p.GetNumber(), pr.Labels); err != nil {
			return p.GetHTMLURL(), fmt.Errorf("label PR: %w", err)
		}
	}
	if pr.AutoMerge {
		if err := g.enableAutoMerge(ctx, p.GetNodeID()); err != nil {
			return p.GetHTMLURL(), fmt.Errorf("enable auto-merge: %w", err)
		}
	}
	return p.GetHTMLURL(), nil
}

// enableAutoMerge uses GraphQL (REST has no endpoint for it). Requires
// "Allow auto-merge" on the repo (task gh:envs sets it).
func (g *GitHub) enableAutoMerge(ctx context.Context, nodeID string) error {
	q := map[string]any{
		"query":     `mutation($id: ID!) { enablePullRequestAutoMerge(input: {pullRequestId: $id, mergeMethod: SQUASH}) { clientMutationId } }`,
		"variables": map[string]any{"id": nodeID},
	}
	u := strings.TrimSuffix(g.Client.BaseURL.String(), "/")
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/api/v3"), "/") + "/graphql"
	if strings.Contains(u, "api.github.com") {
		u = "https://api.github.com/graphql"
	}
	req, err := g.Client.NewRequest(http.MethodPost, u, q)
	if err != nil {
		return err
	}
	var out struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if _, err := g.Client.Do(ctx, req, &out); err != nil {
		return err
	}
	if len(out.Errors) > 0 {
		// Already enabled / clean status are not problems.
		if m := out.Errors[0].Message; !strings.Contains(m, "already") && !strings.Contains(m, "clean status") {
			return errors.New(m)
		}
	}
	return nil
}

// IsNotFound reports a GitHub 404.
func IsNotFound(err error) bool {
	var e *github.ErrorResponse
	return errors.As(err, &e) && e.Response != nil && e.Response.StatusCode == http.StatusNotFound
}
