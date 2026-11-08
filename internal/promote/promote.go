// Package promote runs the evidence gate for an app in a tier and, when it
// passes, opens the promotion PR for the next tier.
//
// Each tier attests for itself: Check only touches the clusters it is given
// (from tiers.yaml locally, or just its own cluster in-cluster).
package promote

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/twallac10/platform-promoter/internal/checks"
	"github.com/twallac10/platform-promoter/internal/cluster"
	"github.com/twallac10/platform-promoter/internal/config"
	"github.com/twallac10/platform-promoter/internal/gate"
	"github.com/twallac10/platform-promoter/internal/ledger"
	"github.com/twallac10/platform-promoter/internal/release"
)

const Label = "promotion"

type Promoter struct {
	Fleet  *config.Fleet
	Ledger ledger.Ledger
	// Connect returns a Cluster for a kube context from tiers.yaml.
	Connect func(kubeContext string) (*cluster.Cluster, error)
	// Checks builds the check list for an app (default: checks.For).
	Checks func(config.App) ([]gate.Check, error)
	// Record writes Deployments, evidence comments and Kubernetes Events.
	// Off for dry runs; reading GitHub still happens.
	Record bool
	Out    io.Writer
	// Contexts, if set, restricts which of the tier's clusters this process
	// checks (in-cluster: only its own).
	Contexts []string
}

// Result of a gate run across a tier's clusters.
type Result struct {
	App           config.App
	Tier          config.Tier
	Release       *release.Release
	ReleaseCommit string
	Reports       []*gate.Report
	EvidenceURLs  []string
}

func (r *Result) Passed() bool {
	if len(r.Reports) == 0 {
		return false
	}
	for _, rep := range r.Reports {
		if !rep.Passed() {
			return false
		}
	}
	return true
}

func releasePath(tier string) string { return "tiers/" + tier + "/release.yaml" }

// Check runs the gate for app in tier on each cluster and records the outcome.
func (p *Promoter) Check(ctx context.Context, appName, tierName string) (*Result, error) {
	app, err := p.Fleet.App(appName)
	if err != nil {
		return nil, err
	}
	tier, err := p.Fleet.Tier(tierName)
	if err != nil {
		return nil, err
	}
	path := releasePath(tier.Name)
	commit, err := p.Ledger.LastCommit(ctx, app.Repo, path)
	if err != nil {
		return nil, err
	}
	raw, ok, err := p.Ledger.File(ctx, app.Repo, path, commit)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s has no %s", app.Repo, path)
	}
	rel, err := release.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", app.Repo, path, err)
	}
	res := &Result{App: app, Tier: tier, Release: rel, ReleaseCommit: commit}
	for _, kctx := range tier.Clusters {
		if len(p.Contexts) > 0 && !contains(p.Contexts, kctx) {
			continue
		}
		rep, url, err := p.checkCluster(ctx, app, tier, kctx, rel, commit)
		if err != nil {
			return res, fmt.Errorf("%s: %w", kctx, err)
		}
		res.Reports = append(res.Reports, rep)
		res.EvidenceURLs = append(res.EvidenceURLs, url)
	}
	if len(res.Reports) == 0 {
		return res, fmt.Errorf("no clusters of tier %s to check", tier.Name)
	}
	return res, nil
}

func (p *Promoter) checkCluster(ctx context.Context, app config.App, tier config.Tier, kctx string, rel *release.Release, commit string) (*gate.Report, string, error) {
	cl, err := p.Connect(kctx)
	if err != nil {
		return nil, "", err
	}
	// The Deployment's ref is what this cluster actually applied, if known.
	ref := commit
	if ks, err := cl.Get(ctx, cluster.Kustomization, "flux-system", app.Name); err == nil {
		if sha := checks.AppliedSHA(ks); sha != "" {
			ref = sha
		}
	}
	baseline := map[string]float64{}
	if prev, err := p.Ledger.LatestDeployment(ctx, app.Repo, tier.Name, app.Name, kctx, true); err == nil && prev != nil && prev.LogURL != "" {
		if ev, err := p.Ledger.ReadEvidence(ctx, app.Repo, prev.LogURL); err == nil {
			baseline = ev.Metrics
		}
	}
	payload := ledger.Payload{App: app.Name, Tier: tier.Name, Cluster: kctx, Digest: rel.Digest, Version: rel.Version, ReleaseCommit: commit}
	var depID int64
	if p.Record {
		if depID, err = p.Ledger.StartDeployment(ctx, app.Repo, ref, p.Fleet.IsLast(tier.Name), payload); err != nil {
			return nil, "", err
		}
	}
	cs, err := p.checks(app)
	if err != nil {
		return nil, "", err
	}
	env := &gate.Env{
		App: app, Tier: tier, Gate: p.Fleet.Gate, Cluster: cl,
		Release: rel, ReleaseCommit: commit, Repo: repoAdapter{p.Ledger, app.Repo},
		Baseline: baseline,
	}
	p.printf("gate: %s in %s on %s (chart %s %s), %d checks, waiting up to %s for data...\n",
		app.Name, tier.Name, kctx, rel.Version, rel.ShortDigest(), len(cs), p.Fleet.Gate.MaxWait.D())
	rep := &gate.Report{App: app.Name, Tier: tier.Name, Cluster: kctx, Digest: rel.Digest, Version: rel.Version, Started: time.Now()}
	rep.Results = gate.Run(ctx, env, cs, gate.Options{MaxWait: p.Fleet.Gate.MaxWait.D(), PollInterval: p.Fleet.Gate.PollInterval.D()})
	rep.Finished = time.Now()
	rep.Metrics = env.Metrics()
	if p.Out != nil {
		rep.Text(p.Out)
	}

	var url string
	if p.Record {
		if url, err = p.Ledger.PostEvidence(ctx, app.Repo, ref, rep.Markdown(), ledger.Evidence{Metrics: rep.Metrics}); err != nil {
			p.printf("warning: %v\n", err)
		}
		if err := p.Ledger.FinishDeployment(ctx, app.Repo, tier.Name, depID, rep.Passed(), rep.Summary(), url); err != nil {
			return rep, url, err
		}
		p.event(ctx, cl, app, rep)
	}
	return rep, url, nil
}

// event surfaces the outcome on the app's HelmRelease as a Kubernetes Event.
func (p *Promoter) event(ctx context.Context, cl *cluster.Cluster, app config.App, rep *gate.Report) {
	typ, reason := corev1.EventTypeNormal, "PromotionGatePassed"
	if !rep.Passed() {
		typ, reason = corev1.EventTypeWarning, "PromotionGateFailed"
	}
	ref := corev1.ObjectReference{
		APIVersion: "helm.toolkit.fluxcd.io/v2", Kind: "HelmRelease", Namespace: app.Namespace, Name: app.Name,
	}
	if err := cl.Event(ctx, ref, typ, reason, fmt.Sprintf("%s %s: %s", rep.Version, rep.Digest, rep.Summary())); err != nil {
		p.printf("warning: could not record Kubernetes Event: %v\n", err)
	}
}

// Promote runs the gate on the source tier and, if it passes on every
// cluster, opens a PR that copies the source tier's release.yaml to the next
// tier. Returns the PR URL ("" if nothing to promote).
func (p *Promoter) Promote(ctx context.Context, appName, fromTier string) (string, *Result, error) {
	to, ok, err := p.Fleet.Next(fromTier)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return "", nil, fmt.Errorf("%s is the last tier; nothing to promote to", fromTier)
	}
	res, err := p.Check(ctx, appName, fromTier)
	if err != nil {
		return "", res, err
	}
	if !res.Passed() {
		return "", res, &GateFailedError{Tier: fromTier, Result: res}
	}
	app := res.App
	// Every cluster of the source tier must have attested success for this
	// digest; with several clusters, other promoters may have recorded theirs.
	from, _ := p.Fleet.Tier(fromTier)
	for _, kctx := range from.Clusters {
		d, err := p.Ledger.LatestDeployment(ctx, app.Repo, fromTier, app.Name, kctx, false)
		if err != nil {
			return "", res, err
		}
		if d == nil || d.State != "success" || d.Payload.Digest != res.Release.Digest || d.Payload.ReleaseCommit != res.ReleaseCommit {
			p.printf("waiting on %s: no successful gate for %s yet; no PR\n", kctx, res.Release.ShortDigest())
			return "", res, nil
		}
	}
	// Promote exactly what was checked; refuse if main moved underneath us.
	src, _, err := p.Ledger.File(ctx, app.Repo, releasePath(fromTier), "main")
	if err != nil {
		return "", res, err
	}
	if !bytes.Equal(src, res.Release.Raw) {
		return "", res, fmt.Errorf("%s changed on main since the gate ran; re-run", releasePath(fromTier))
	}
	dst, _, err := p.Ledger.File(ctx, app.Repo, releasePath(to.Name), "main")
	if err != nil {
		return "", res, err
	}
	if bytes.Equal(dst, src) {
		p.printf("%s already has this release; nothing to promote\n", to.Name)
		return "", res, nil
	}
	url, err := p.Ledger.OpenPromotionPR(ctx, ledger.PR{
		Repo:          app.Repo,
		Branch:        fmt.Sprintf("promote/%s/%s", to.Name, res.Release.ShortDigest()),
		Path:          releasePath(to.Name),
		Content:       src,
		Title:         fmt.Sprintf("promote(%s): %s %s from %s", to.Name, app.Name, res.Release.Version, fromTier),
		Body:          PRBody(res, to),
		CommitMessage: fmt.Sprintf("promote(%s): %s %s (%s) from %s", to.Name, app.Name, res.Release.Version, res.Release.ShortDigest(), fromTier),
		Labels:        []string{Label},
		AutoMerge:     true, // merges once promotion-gate passes (after approval where required)
	})
	return url, res, err
}

// GateFailedError means the evidence did not pass; no PR is opened.
type GateFailedError struct {
	Tier   string
	Result *Result
}

func (e *GateFailedError) Error() string {
	var parts []string
	for _, r := range e.Result.Reports {
		if !r.Passed() {
			parts = append(parts, r.Cluster+": "+r.Summary())
		}
	}
	return fmt.Sprintf("gate failed in %s, no PR opened: %s", e.Tier, strings.Join(parts, "; "))
}

// PRBody is the promotion PR description: what moves, and the evidence.
func PRBody(res *Result, to config.Tier) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Promote **%s** `%s` from **%s** to **%s**.\n\n", res.App.Name, res.Release.Version, res.Tier.Name, to.Name)
	fmt.Fprintf(&b, "- chart digest: `%s`\n", res.Release.Digest)
	fmt.Fprintf(&b, "- change: `%s` := `%s` (verbatim copy; tier config untouched)\n", releasePath(to.Name), releasePath(res.Tier.Name))
	fmt.Fprintf(&b, "- source release commit: %s\n", res.ReleaseCommit)
	if to.RequiresApproval {
		fmt.Fprintf(&b, "- **%s requires approval**: the `promotion-gate` check waits for an environment reviewer.\n", to.Name)
	}
	b.WriteString("\n## Evidence\n\n")
	for i, rep := range res.Reports {
		b.WriteString(rep.Markdown())
		if i < len(res.EvidenceURLs) && res.EvidenceURLs[i] != "" {
			fmt.Fprintf(&b, "Recorded: %s\n", res.EvidenceURLs[i])
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "<!-- promotion: {\"app\":%q,\"from\":%q,\"to\":%q,\"digest\":%q} -->\n", res.App.Name, res.Tier.Name, to.Name, res.Release.Digest)
	b.WriteString("\n_Opened by platform-promoter._\n")
	return b.String()
}

func (p *Promoter) checks(app config.App) ([]gate.Check, error) {
	if p.Checks != nil {
		return p.Checks(app)
	}
	return checks.For(app)
}

func (p *Promoter) printf(f string, a ...any) {
	if p.Out != nil {
		fmt.Fprintf(p.Out, f, a...)
	}
}

type repoAdapter struct {
	l    ledger.Ledger
	repo string
}

func (r repoAdapter) IsAncestorOrEqual(ctx context.Context, a, b string) (bool, error) {
	return r.l.IsAncestorOrEqual(ctx, r.repo, a, b)
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
