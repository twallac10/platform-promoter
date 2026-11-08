// Package ledger records gate outcomes and promotions on GitHub: a Deployment
// per gate run (environment = tier), an evidence comment on the commit, and
// the promotion PR. The interface keeps the promoter testable without GitHub.
package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

// Payload is stored on each GitHub Deployment. CI reads Digest to verify a
// promotion's source tier passed for the same chart.
type Payload struct {
	App           string `json:"app"`
	Tier          string `json:"tier"`
	Cluster       string `json:"cluster"`
	Digest        string `json:"digest"`
	Version       string `json:"version"`
	ReleaseCommit string `json:"releaseCommit"`
}

// Deployment is a recorded gate run.
type Deployment struct {
	ID      int64
	SHA     string
	Payload Payload
	State   string // latest status: in_progress, success, failure, ...
	LogURL  string // evidence comment
	URL     string
}

// Evidence is embedded (as JSON in an HTML comment) in the evidence comment,
// so the next run can read its baseline metrics back.
type Evidence struct {
	Metrics map[string]float64 `json:"metrics,omitempty"`
}

type Ledger interface {
	// LastCommit returns the newest commit on main that touched path.
	LastCommit(ctx context.Context, repo, path string) (string, error)
	// File returns path at ref (branch or SHA). ok=false if it does not exist.
	File(ctx context.Context, repo, path, ref string) (content []byte, ok bool, err error)
	// IsAncestorOrEqual reports whether a is b or an ancestor of b.
	IsAncestorOrEqual(ctx context.Context, repo, a, b string) (bool, error)

	StartDeployment(ctx context.Context, repo, sha string, production bool, p Payload) (int64, error)
	FinishDeployment(ctx context.Context, repo, environment string, id int64, success bool, description, logURL string) error
	// LatestDeployment returns the newest Deployment in environment for this
	// app+cluster (any state when onlySuccess is false). nil if none.
	LatestDeployment(ctx context.Context, repo, environment, app, cluster string, onlySuccess bool) (*Deployment, error)

	// PostEvidence comments the evidence on a commit and returns its URL.
	PostEvidence(ctx context.Context, repo, sha, markdown string, ev Evidence) (string, error)
	// ReadEvidence reads back the Evidence from a comment URL.
	ReadEvidence(ctx context.Context, repo, url string) (*Evidence, error)

	// OpenPromotionPR points branch at main + one commit setting path to
	// content, and opens (or updates) a PR with the given labels.
	OpenPromotionPR(ctx context.Context, pr PR) (string, error)
}

type PR struct {
	Repo, Branch, Path string
	Content            []byte
	Title, Body        string
	CommitMessage      string
	Labels             []string
	// AutoMerge enables GitHub auto-merge (squash): the PR merges itself once
	// the required promotion-gate check passes.
	AutoMerge bool
}

const evidenceMarker = "promoter-evidence"

var evidenceRE = regexp.MustCompile(`<!-- ` + evidenceMarker + ` (\{.*?\}) -->`)

// EmbedEvidence appends the machine-readable evidence to a markdown body.
func EmbedEvidence(markdown string, ev Evidence) string {
	b, _ := json.Marshal(ev)
	return fmt.Sprintf("%s\n<!-- %s %s -->\n", markdown, evidenceMarker, b)
}

// ExtractEvidence finds embedded evidence in a body.
func ExtractEvidence(body string) (*Evidence, error) {
	m := evidenceRE.FindStringSubmatch(body)
	if m == nil {
		return nil, fmt.Errorf("no %s marker", evidenceMarker)
	}
	var ev Evidence
	if err := json.Unmarshal([]byte(m[1]), &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}
