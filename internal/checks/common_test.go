package checks

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/twallac10/platform-promoter/internal/config"
	"github.com/twallac10/platform-promoter/internal/gate"
)

var app = config.App{Name: "demo", Namespace: "demo"}

func ks(ready string, rev string) runtime.Object {
	return u(map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
		"metadata": map[string]any{"name": "demo", "namespace": "flux-system", "generation": int64(2)},
		"status": map[string]any{"observedGeneration": int64(2), "conditions": readyCond(ready),
			"lastAppliedRevision": rev},
	})
}

func TestFluxKustomization(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t, app, []runtime.Object{ks("True", "main@sha1:bbbbbbbbbb")}, nil)
	expect(t, FluxKustomization{}.Run(ctx, env), gate.Pass)

	env = newEnv(t, app, []runtime.Object{ks("True", "main@sha1:bbbbbbbbbb")}, nil,
		func(e *gate.Env) { e.Repo = fakeRepo{ancestor: false} })
	expect(t, FluxKustomization{}.Run(ctx, env), gate.Pending)

	env = newEnv(t, app, []runtime.Object{ks("False", "main@sha1:bbbbbbbbbb")}, nil)
	expect(t, FluxKustomization{}.Run(ctx, env), gate.Pending)
}

func ociAndHR(artifactRev, ociDigest, hrReady, reason string, testOn bool, hooks map[string]any) []runtime.Object {
	hist := map[string]any{"status": "deployed", "chartVersion": "1.0.0", "ociDigest": ociDigest, "configDigest": "sha256:c"}
	if hooks != nil {
		hist["testHooks"] = hooks
	}
	cond := readyCond(hrReady)
	cond[0].(map[string]any)["reason"] = reason
	return []runtime.Object{
		u(map[string]any{"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "OCIRepository",
			"metadata": map[string]any{"name": "demo", "namespace": "demo"},
			"status":   map[string]any{"artifact": map[string]any{"revision": artifactRev}}}),
		u(map[string]any{"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
			"metadata": map[string]any{"name": "demo", "namespace": "demo", "generation": int64(1)},
			"spec":     map[string]any{"test": map[string]any{"enable": testOn}},
			"status": map[string]any{"observedGeneration": int64(1), "conditions": cond,
				"lastAttemptedConfigDigest": "sha256:c", "history": []any{hist}}}),
	}
}

func TestChartDigest(t *testing.T) {
	ctx := context.Background()
	expect(t, ChartDigest{}.Run(ctx, newEnv(t, app, ociAndHR(testDigest, testDigest, "True", "Succeeded", false, nil), nil)), gate.Pass)
	// Source not yet at the digest: wait.
	expect(t, ChartDigest{}.Run(ctx, newEnv(t, app, ociAndHR("sha256:other", testDigest, "True", "Succeeded", false, nil), nil)), gate.Pending)
	// Upgrade failed: no point waiting.
	expect(t, ChartDigest{}.Run(ctx, newEnv(t, app, ociAndHR(testDigest, testDigest, "False", "UpgradeFailed", false, nil), nil)), gate.Fail)
}

func TestHelmTests(t *testing.T) {
	ctx := context.Background()
	expect(t, HelmTests{}.Run(ctx, newEnv(t, app, ociAndHR(testDigest, testDigest, "True", "", false, nil), nil)), gate.Skip)
	ok := map[string]any{"t1": map[string]any{"phase": "Succeeded"}}
	expect(t, HelmTests{}.Run(ctx, newEnv(t, app, ociAndHR(testDigest, testDigest, "True", "", true, ok), nil)), gate.Pass)
	bad := map[string]any{"t1": map[string]any{"phase": "Failed"}}
	expect(t, HelmTests{}.Run(ctx, newEnv(t, app, ociAndHR(testDigest, testDigest, "True", "", true, bad), nil)), gate.Fail)
}

func pod(name, image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "demo"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}

func TestImageTags(t *testing.T) {
	ctx := context.Background()
	withTags := func(e *gate.Env) { e.Release.Values = map[string]any{"image": map[string]any{"tag": "v1.2.3"}} }
	env := newEnv(t, app, []runtime.Object{pod("a", "reg.io/x/app:v1.2.3"), pod("b", "reg.io:5000/x/app:v1.2.3@sha256:abc")}, nil, withTags)
	expect(t, ImageTags{}.Run(ctx, env), gate.Pass)

	env = newEnv(t, app, []runtime.Object{pod("a", "reg.io/x/app:v1.2.3"), pod("b", "reg.io/x/app:v9")}, nil, withTags)
	r := ImageTags{}.Run(ctx, env)
	expect(t, r, gate.Fail)

	expect(t, ImageTags{}.Run(ctx, newEnv(t, app, nil, nil)), gate.Skip)
}

func TestImageTag(t *testing.T) {
	for in, want := range map[string]string{
		"nginx": "latest", "nginx:1.2": "1.2", "localhost:5000/a/b": "latest",
		"localhost:5000/a/b:v1": "v1", "a/b:v1@sha256:00": "v1",
	} {
		if got := imageTag(in); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", in, got, want)
		}
	}
}
