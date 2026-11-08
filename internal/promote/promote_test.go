package promote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/twallac10/platform-promoter/internal/cluster"
	"github.com/twallac10/platform-promoter/internal/config"
	"github.com/twallac10/platform-promoter/internal/gate"
	"github.com/twallac10/platform-promoter/internal/ledger"
)

const (
	digA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func releaseYAML(digest, version string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: kyverno
  namespace: kyverno
  annotations:
    platform.twallac10.io/chart-version: "%s"
spec:
  ref:
    digest: "%s"
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: kyverno
  namespace: kyverno
spec:
  values:
    image: {tag: v1}
`, version, digest))
}

// fakeLedger is an in-memory GitHub.
type fakeLedger struct {
	files       map[string][]byte // "path@ref"
	deployments []*ledger.Deployment
	comments    []string
	prs         []ledger.PR
}

func (f *fakeLedger) LastCommit(_ context.Context, _, path string) (string, error) {
	return "c0ffee1", nil
}
func (f *fakeLedger) File(_ context.Context, _, path, ref string) ([]byte, bool, error) {
	if ref == "c0ffee1" {
		ref = "main"
	}
	b, ok := f.files[path+"@"+ref]
	return b, ok, nil
}
func (f *fakeLedger) IsAncestorOrEqual(context.Context, string, string, string) (bool, error) {
	return true, nil
}
func (f *fakeLedger) StartDeployment(_ context.Context, _, sha string, prod bool, p ledger.Payload) (int64, error) {
	d := &ledger.Deployment{ID: int64(len(f.deployments) + 1), SHA: sha, Payload: p, State: "in_progress"}
	f.deployments = append(f.deployments, d)
	return d.ID, nil
}
func (f *fakeLedger) FinishDeployment(_ context.Context, _, _ string, id int64, ok bool, desc, url string) error {
	d := f.deployments[id-1]
	d.State, d.LogURL = "failure", url
	if ok {
		d.State = "success"
	}
	f.comments = append(f.comments, "status: "+desc)
	return nil
}
func (f *fakeLedger) LatestDeployment(_ context.Context, _, env, app, cl string, onlySuccess bool) (*ledger.Deployment, error) {
	for i := len(f.deployments) - 1; i >= 0; i-- {
		d := f.deployments[i]
		if d.Payload.Tier == env && d.Payload.App == app && d.Payload.Cluster == cl && (!onlySuccess || d.State == "success") {
			return d, nil
		}
	}
	return nil, nil
}
func (f *fakeLedger) PostEvidence(_ context.Context, _, sha, md string, ev ledger.Evidence) (string, error) {
	f.comments = append(f.comments, md)
	return fmt.Sprintf("https://github.com/x/y/commit/%s#commitcomment-%d", sha, len(f.comments)), nil
}
func (f *fakeLedger) ReadEvidence(context.Context, string, string) (*ledger.Evidence, error) {
	return &ledger.Evidence{}, nil
}
func (f *fakeLedger) OpenPromotionPR(_ context.Context, pr ledger.PR) (string, error) {
	f.prs = append(f.prs, pr)
	return "https://github.com/twallac10/platform-kyverno/pull/1", nil
}

type fixed struct {
	name string
	st   gate.Status
}

func (c fixed) Name() string { return c.name }
func (c fixed) Run(context.Context, *gate.Env) gate.Result {
	return gate.Result{Status: c.st, Value: "v", Reason: map[bool]string{true: "", false: "canary target not discovered"}[c.st == gate.Pass]}
}

func setup(t *testing.T, st gate.Status) (*Promoter, *fakeLedger, *kubefake.Clientset) {
	t.Helper()
	fleet, err := config.Parse(
		[]byte("tiers:\n- {name: dev, clusters: [kind-tier-dev]}\n- {name: stg, clusters: [kind-tier-stg]}\n- {name: prd, clusters: [kind-tier-prd], requiresApproval: true}\n"),
		[]byte("owner: twallac10\ngate: {maxWait: 1s, pollInterval: 1ms}\napps:\n- {name: kyverno, repo: platform-kyverno, namespace: kyverno}\n"))
	if err != nil {
		t.Fatal(err)
	}
	fl := &fakeLedger{files: map[string][]byte{
		"tiers/dev/release.yaml@main": releaseYAML(digB, "2.0.0"),
		"tiers/stg/release.yaml@main": releaseYAML(digA, "1.0.0"),
	}}
	kube := kubefake.NewSimpleClientset()
	p := &Promoter{
		Fleet: fleet, Ledger: fl, Record: true,
		Connect: func(ctx string) (*cluster.Cluster, error) {
			return &cluster.Cluster{Name: ctx, Kube: kube, Dynamic: dynfake.NewSimpleDynamicClient(runtime.NewScheme())}, nil
		},
		Checks: func(config.App) ([]gate.Check, error) {
			return []gate.Check{fixed{"chart digest deployed", gate.Pass}, fixed{"canary scraped", st}}, nil
		},
	}
	return p, fl, kube
}

func TestPromoteOpensPROnlyForTargetRelease(t *testing.T) {
	p, fl, kube := setup(t, gate.Pass)
	url, _, err := p.Promote(context.Background(), "kyverno", "dev")
	if err != nil || url == "" {
		t.Fatalf("url=%q err=%v", url, err)
	}
	if len(fl.prs) != 1 {
		t.Fatalf("prs = %d", len(fl.prs))
	}
	pr := fl.prs[0]
	if pr.Path != "tiers/stg/release.yaml" {
		t.Errorf("PR touches %s", pr.Path)
	}
	if !bytes.Equal(pr.Content, fl.files["tiers/dev/release.yaml@main"]) {
		t.Error("PR content is not a verbatim copy of the source tier's release.yaml")
	}
	if pr.Branch != "promote/stg/bbbbbbbbbbbb" || len(pr.Labels) != 1 || pr.Labels[0] != "promotion" {
		t.Errorf("branch %s labels %v", pr.Branch, pr.Labels)
	}
	for _, want := range []string{"| canary scraped | ✅ pass", digB, `"to":"stg"`} {
		if !strings.Contains(pr.Body, want) {
			t.Errorf("PR body missing %q:\n%s", want, pr.Body)
		}
	}
	d := fl.deployments[0]
	if d.State != "success" || d.Payload.Tier != "dev" || d.Payload.Digest != digB {
		t.Errorf("deployment %+v", d)
	}
	evs, _ := kube.CoreV1().Events("kyverno").List(context.Background(), metav1.ListOptions{})
	if len(evs.Items) != 1 || evs.Items[0].Reason != "PromotionGatePassed" {
		t.Errorf("events %+v", evs.Items)
	}
}

func TestFailedGateOpensNoPRAndRecordsReason(t *testing.T) {
	p, fl, kube := setup(t, gate.Fail)
	_, _, err := p.Promote(context.Background(), "kyverno", "dev")
	var gf *GateFailedError
	if !errors.As(err, &gf) {
		t.Fatalf("err = %v", err)
	}
	if len(fl.prs) != 0 {
		t.Fatal("PR opened despite failed gate")
	}
	if d := fl.deployments[0]; d.State != "failure" {
		t.Errorf("deployment state %s", d.State)
	}
	if !strings.Contains(strings.Join(fl.comments, "\n"), "status: 1 failed: canary scraped: canary target not discovered") {
		t.Errorf("failure reason not on the deployment status: %v", fl.comments)
	}
	evs, _ := kube.CoreV1().Events("kyverno").List(context.Background(), metav1.ListOptions{})
	if len(evs.Items) != 1 || evs.Items[0].Type != "Warning" || evs.Items[0].Reason != "PromotionGateFailed" {
		t.Errorf("events %+v", evs.Items)
	}
}

func TestPendingAtTimeoutIsAFailure(t *testing.T) {
	p, fl, _ := setup(t, gate.Pending)
	_, _, err := p.Promote(context.Background(), "kyverno", "dev")
	var gf *GateFailedError
	if !errors.As(err, &gf) || len(fl.prs) != 0 {
		t.Fatalf("err=%v prs=%d", err, len(fl.prs))
	}
}

func TestNothingToPromote(t *testing.T) {
	p, fl, _ := setup(t, gate.Pass)
	fl.files["tiers/stg/release.yaml@main"] = fl.files["tiers/dev/release.yaml@main"]
	url, _, err := p.Promote(context.Background(), "kyverno", "dev")
	if err != nil || url != "" || len(fl.prs) != 0 {
		t.Fatalf("url=%q err=%v prs=%d", url, err, len(fl.prs))
	}
}

func TestLastTierCannotPromote(t *testing.T) {
	p, _, _ := setup(t, gate.Pass)
	if _, _, err := p.Promote(context.Background(), "kyverno", "prd"); err == nil {
		t.Fatal("expected error promoting from the last tier")
	}
}

func TestDryRunRecordsNothing(t *testing.T) {
	p, fl, _ := setup(t, gate.Pass)
	p.Record = false
	res, err := p.Check(context.Background(), "kyverno", "dev")
	if err != nil || !res.Passed() || len(fl.deployments) != 0 {
		t.Fatalf("err=%v deployments=%d", err, len(fl.deployments))
	}
}
