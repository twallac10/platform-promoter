package controller

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/twallac10/platform-promoter/api/v1alpha1"
	"github.com/twallac10/platform-promoter/internal/config"
	"github.com/twallac10/platform-promoter/internal/gate"
	"github.com/twallac10/platform-promoter/internal/ledger"
	"github.com/twallac10/platform-promoter/internal/promote"
	"github.com/twallac10/platform-promoter/internal/release"
)

const (
	dA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func rel(d string) []byte {
	return []byte("kind: OCIRepository\nmetadata:\n  annotations:\n    platform.twallac10.io/chart-version: \"2.0.0\"\nspec:\n  ref:\n    digest: \"" + d + "\"\n")
}

type fakeLedger struct {
	ledger.Ledger
	files map[string][]byte
	dep   *ledger.Deployment
}

func (f *fakeLedger) LastCommit(context.Context, string, string) (string, error) {
	return "c0ffee", nil
}
func (f *fakeLedger) File(_ context.Context, _, path, _ string) ([]byte, bool, error) {
	b, ok := f.files[path]
	return b, ok, nil
}
func (f *fakeLedger) LatestDeployment(context.Context, string, string, string, string, bool) (*ledger.Deployment, error) {
	return f.dep, nil
}

type fakeGater struct {
	pass             bool
	checks, promotes int
}

func (g *fakeGater) result() *promote.Result {
	st := gate.Pass
	if !g.pass {
		st = gate.Fail
	}
	r, _ := release.Parse(rel(dB))
	return &promote.Result{Release: r, Reports: []*gate.Report{{Results: []gate.Result{{Check: "canary scraped", Status: st, Reason: "canary target not discovered"}}}},
		EvidenceURLs: []string{"https://github.com/x/y/commit/c#commitcomment-1"}}
}
func (g *fakeGater) Check(context.Context, string, string) (*promote.Result, error) {
	g.checks++
	return g.result(), nil
}
func (g *fakeGater) Promote(context.Context, string, string) (string, *promote.Result, error) {
	g.promotes++
	res := g.result()
	if !g.pass {
		return "", res, &promote.GateFailedError{Tier: "dev", Result: res}
	}
	return "https://github.com/o/platform-kyverno/pull/7", res, nil
}

var now = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func flux(kind, ns, ready string, since time.Time, rev string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	if kind == "Kustomization" {
		u.SetGroupVersionKind(kustomizationGVK)
	} else {
		u.SetGroupVersionKind(helmReleaseGVK)
	}
	u.SetNamespace(ns)
	u.SetName("kyverno")
	u.Object["status"] = map[string]any{
		"lastAppliedRevision": rev,
		"conditions": []any{map[string]any{"type": "Ready", "status": ready, "reason": "R", "message": "m",
			"lastTransitionTime": since.Format(time.RFC3339)}},
	}
	return u
}

type setup struct {
	r   *Reconciler
	c   client.Client
	led *fakeLedger
	g   *fakeGater
}

func newSetup(t *testing.T, tier, ksReady string, ksSince time.Time) *setup {
	t.Helper()
	fleet, err := config.Parse(
		[]byte("tiers:\n- {name: dev, clusters: [kind-tier-dev]}\n- {name: stg, clusters: [kind-tier-stg]}\n"),
		[]byte("owner: o\napps:\n- {name: kyverno, repo: platform-kyverno, namespace: kyverno}\n"))
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(kustomizationGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(helmReleaseGVK, &unstructured.Unstructured{})
	pg := &v1.PromotionGate{ObjectMeta: metav1.ObjectMeta{Namespace: "promoter", Name: "kyverno", Generation: 1}, Spec: v1.PromotionGateSpec{App: "kyverno"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1.PromotionGate{}).
		WithObjects(pg, flux("Kustomization", "flux-system", ksReady, ksSince, "main@sha1:abc"), flux("HelmRelease", "kyverno", "True", ksSince, "")).Build()
	led := &fakeLedger{files: map[string][]byte{"tiers/dev/release.yaml": rel(dB), "tiers/stg/release.yaml": rel(dA)}}
	g := &fakeGater{pass: true}
	r := &Reconciler{Client: c, Ledger: led, Gater: g, Tier: tier, Cluster: "kind-tier-" + tier, Namespace: "promoter",
		Resync: 10 * time.Minute, Now: func() time.Time { return now },
		Fleet: func(context.Context) (*config.Fleet, error) { return fleet, nil }}
	return &setup{r, c, led, g}
}

func (s *setup) reconcile(t *testing.T) (*v1.PromotionGate, ctrl.Result) {
	t.Helper()
	res, err := s.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "promoter", Name: "kyverno"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var pg v1.PromotionGate
	if err := s.c.Get(context.Background(), types.NamespacedName{Namespace: "promoter", Name: "kyverno"}, &pg); err != nil {
		t.Fatal(err)
	}
	return &pg, res
}

func TestNewReleaseIsGatedAndPromoted(t *testing.T) {
	s := newSetup(t, "dev", "True", now.Add(-time.Minute))
	pg, res := s.reconcile(t)
	if s.g.promotes != 1 || pg.Status.Phase != v1.PhasePassed || pg.Status.Promotion == nil || pg.Status.Promotion.TargetTier != "stg" {
		t.Fatalf("promotes=%d status=%+v", s.g.promotes, pg.Status)
	}
	if !meta.IsStatusConditionTrue(pg.Status.Conditions, v1.ConditionGatePassed) {
		t.Error("GatePassed condition not true")
	}
	if pg.Status.LastGate == nil || len(pg.Status.LastGate.Results) != 1 || pg.Status.Release.Version != "2.0.0" {
		t.Errorf("last gate %+v", pg.Status.LastGate)
	}
	if res.RequeueAfter != 10*time.Minute {
		t.Errorf("requeue %s", res.RequeueAfter)
	}
}

func TestWaitsForFluxThenGatesWhenStuck(t *testing.T) {
	s := newSetup(t, "dev", "False", now.Add(-time.Minute))
	pg, res := s.reconcile(t)
	if s.g.promotes != 0 || pg.Status.Phase != v1.PhaseWaiting {
		t.Fatalf("should wait for Flux: %+v", pg.Status)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > StuckAfter {
		t.Errorf("requeue %s", res.RequeueAfter)
	}
	s = newSetup(t, "dev", "False", now.Add(-StuckAfter-time.Minute))
	s.g.pass = false
	pg, _ = s.reconcile(t)
	if s.g.promotes != 1 || pg.Status.Phase != v1.PhaseFailed {
		t.Fatalf("stuck release must be gated so the failure is recorded: %+v", pg.Status)
	}
	if meta.IsStatusConditionTrue(pg.Status.Conditions, v1.ConditionGatePassed) {
		t.Error("GatePassed must be false")
	}
}

func TestFailedGateRetryThrottle(t *testing.T) {
	s := newSetup(t, "dev", "True", now.Add(-time.Hour))
	s.g.pass = false
	pg, _ := s.reconcile(t) // fails, records status
	if pg.Status.Phase != v1.PhaseFailed || s.g.promotes != 1 {
		t.Fatalf("%+v", pg.Status)
	}
	s.led.dep = &ledger.Deployment{State: "failure", Payload: ledger.Payload{Digest: dB, ReleaseCommit: "c0ffee"}}
	// Same Flux state, a minute later: no retry.
	now = now.Add(time.Minute)
	pg, res := s.reconcile(t)
	if s.g.promotes != 1 || pg.Status.Phase != v1.PhaseFailed {
		t.Fatalf("retried too soon: %+v", pg.Status)
	}
	if res.RequeueAfter > RetryFailedIdle {
		t.Errorf("requeue %s", res.RequeueAfter)
	}
	// Flux applied something new and 2m passed: retry.
	var ks unstructured.Unstructured
	ks.SetGroupVersionKind(kustomizationGVK)
	s.c.Get(context.Background(), types.NamespacedName{Namespace: "flux-system", Name: "kyverno"}, &ks)
	unstructured.SetNestedField(ks.Object, "main@sha1:def", "status", "lastAppliedRevision")
	s.c.Update(context.Background(), &ks)
	now = now.Add(RetryFailedOnChange)
	s.reconcile(t)
	if s.g.promotes != 2 {
		t.Fatalf("expected a retry after a Flux change, promotes=%d", s.g.promotes)
	}
}

func TestValuesOnlyChangeIsGated(t *testing.T) {
	s := newSetup(t, "dev", "True", now.Add(-time.Hour))
	// Same digest, but gated at an older release commit: re-gate.
	s.led.dep = &ledger.Deployment{State: "success", Payload: ledger.Payload{Digest: dB, ReleaseCommit: "0ld0ld"}}
	s.reconcile(t)
	if s.g.promotes != 1 {
		t.Fatal("values-only change was not gated")
	}
}

func TestAlreadyPromoted(t *testing.T) {
	s := newSetup(t, "dev", "True", now.Add(-time.Hour))
	s.led.dep = &ledger.Deployment{State: "success", Payload: ledger.Payload{Digest: dB, ReleaseCommit: "c0ffee"}}
	s.led.files["tiers/stg/release.yaml"] = rel(dB)
	pg, _ := s.reconcile(t)
	if s.g.promotes != 0 || pg.Status.Phase != v1.PhasePromoted {
		t.Fatalf("%+v", pg.Status)
	}
}

func TestLastTierOnlyChecks(t *testing.T) {
	s := newSetup(t, "stg", "True", now.Add(-time.Hour))
	s.led.files["tiers/stg/release.yaml"] = rel(dB)
	pg, _ := s.reconcile(t)
	if s.g.checks != 1 || s.g.promotes != 0 || pg.Status.Phase != v1.PhasePassed {
		t.Fatalf("checks=%d promotes=%d %+v", s.g.checks, s.g.promotes, pg.Status)
	}
}

func TestSuspend(t *testing.T) {
	s := newSetup(t, "dev", "True", now.Add(-time.Hour))
	var pg v1.PromotionGate
	s.c.Get(context.Background(), types.NamespacedName{Namespace: "promoter", Name: "kyverno"}, &pg)
	pg.Spec.Suspend = true
	s.c.Update(context.Background(), &pg)
	got, _ := s.reconcile(t)
	if s.g.promotes != 0 || got.Status.Phase != v1.PhaseSuspended {
		t.Fatalf("%+v", got.Status)
	}
}

func TestMapFlux(t *testing.T) {
	s := newSetup(t, "dev", "True", now)
	for _, tc := range []struct {
		obj  *unstructured.Unstructured
		want int
	}{
		{flux("Kustomization", "flux-system", "True", now, ""), 1},
		{flux("HelmRelease", "kyverno", "True", now, ""), 1},
		{flux("HelmRelease", "other", "True", now, ""), 0},
	} {
		if got := s.r.mapFlux(context.Background(), tc.obj); len(got) != tc.want {
			t.Errorf("%s/%s: %v", tc.obj.GetKind(), tc.obj.GetNamespace(), got)
		}
	}
}
