package checks

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/twallac10/platform-promoter/internal/cluster"
	"github.com/twallac10/platform-promoter/internal/gate"
)

// FluxKustomization: the fleet's Kustomization for this app is Ready and has
// applied a revision at or after the commit that last changed this tier's
// release.yaml.
type FluxKustomization struct{}

func (FluxKustomization) Name() string { return "flux kustomization ready at release commit" }

func (FluxKustomization) Run(ctx context.Context, env *gate.Env) gate.Result {
	q := fmt.Sprintf("flux get kustomization %s -n flux-system", env.App.Name)
	ks, err := env.Cluster.Get(ctx, cluster.Kustomization, "flux-system", env.App.Name)
	if err != nil {
		return pending("kustomization: "+err.Error(), q)
	}
	applied := AppliedSHA(ks)
	st, reason, msg, _ := cluster.Condition(ks, "Ready")
	if st != "True" || !cluster.Reconciled(ks) {
		return pending(fmt.Sprintf("not Ready (%s: %s)", reason, msg), q)
	}
	if applied == "" {
		return pending("no lastAppliedRevision yet", q)
	}
	ok, err := env.Repo.IsAncestorOrEqual(ctx, env.ReleaseCommit, applied)
	if err != nil {
		return pending("compare commits: "+err.Error(), q)
	}
	if !ok {
		return pending(fmt.Sprintf("applied %s does not contain release commit %s yet", short(applied), short(env.ReleaseCommit)), q)
	}
	return pass(fmt.Sprintf("Ready @ %s ⊇ %s", short(applied), short(env.ReleaseCommit)), q)
}

// AppliedSHA extracts the Git SHA from a Kustomization's lastAppliedRevision
// ("main@sha1:<sha>").
func AppliedSHA(ks *unstructured.Unstructured) string {
	rev, _, _ := unstructured.NestedString(ks.Object, "status", "lastAppliedRevision")
	if i := strings.LastIndex(rev, ":"); i >= 0 {
		return rev[i+1:]
	}
	return rev
}

// ChartDigest: the OCIRepository artifact is the digest release.yaml asks for,
// and the HelmRelease is Ready with that chart (and its current config) deployed.
type ChartDigest struct{}

func (ChartDigest) Name() string { return "chart digest deployed" }

func (ChartDigest) Run(ctx context.Context, env *gate.Env) gate.Result {
	ns, name, want := env.App.Namespace, env.App.Name, env.Release.Digest
	q := fmt.Sprintf("kubectl -n %s get ocirepository,helmrelease %s -o yaml", ns, name)
	oci, err := env.Cluster.Get(ctx, cluster.OCIRepository, ns, name)
	if err != nil {
		return pending("ocirepository: "+err.Error(), q)
	}
	rev, _, _ := unstructured.NestedString(oci.Object, "status", "artifact", "revision")
	if !strings.HasSuffix(rev, want) {
		return pending(fmt.Sprintf("OCIRepository artifact %q, want %s", rev, want), q)
	}
	hr, err := env.Cluster.Get(ctx, cluster.HelmRelease, ns, name)
	if err != nil {
		return pending("helmrelease: "+err.Error(), q)
	}
	if st, reason, msg, _ := cluster.Condition(hr, "Ready"); st != "True" || !cluster.Reconciled(hr) {
		if st == "False" && (reason == "InstallFailed" || reason == "UpgradeFailed" || reason == "TestFailed" || reason == "RollbackSucceeded") {
			return fail(fmt.Sprintf("HelmRelease %s: %s", reason, msg), "", q)
		}
		return pending(fmt.Sprintf("HelmRelease not Ready (%s: %s)", reason, msg), q)
	}
	latest, ok := latestHistory(hr)
	if !ok {
		return pending("HelmRelease has no release history yet", q)
	}
	if d, _ := latest["ociDigest"].(string); d != "" && d != want {
		return pending(fmt.Sprintf("deployed chart %s, want %s", d, want), q)
	}
	if s, _ := latest["status"].(string); s != "deployed" {
		return pending("latest release status "+s, q)
	}
	cfg, _ := latest["configDigest"].(string)
	attempted, _, _ := unstructured.NestedString(hr.Object, "status", "lastAttemptedConfigDigest")
	if attempted != "" && cfg != "" && cfg != attempted {
		return pending("deployed config digest differs from last attempted config", q)
	}
	ver, _ := latest["chartVersion"].(string)
	return pass(fmt.Sprintf("%s %s, config %s", ver, shortDigest(want), shortDigest(cfg)), q)
}

func latestHistory(hr *unstructured.Unstructured) (map[string]any, bool) {
	hist, _, _ := unstructured.NestedSlice(hr.Object, "status", "history")
	if len(hist) == 0 {
		return nil, false
	}
	m, ok := hist[0].(map[string]any)
	return m, ok
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// ImageTags: every running container of the app uses an image tag the release
// pins. Apps whose release pins no tags (CRD/policy-only charts) are skipped.
type ImageTags struct{}

func (ImageTags) Name() string { return "running images match release" }

func (ImageTags) Run(ctx context.Context, env *gate.Env) gate.Result {
	want := env.Release.ImageTags()
	ns := targetNamespace(ctx, env)
	q := fmt.Sprintf("kubectl -n %s get pods -o jsonpath='{..image}'", ns)
	if len(want) == 0 {
		return skip("release pins no image tags")
	}
	allowed := map[string]bool{}
	for _, t := range want {
		allowed[t] = true
	}
	pods, err := env.Cluster.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return pending("list pods: "+err.Error(), q)
	}
	var bad []string
	n := 0
	for i := range pods.Items {
		p := &pods.Items[i]
		if isOneShot(p) {
			continue
		}
		if !cluster.PodReady(p) {
			return pending("pod "+p.Name+" not ready", q)
		}
		for _, c := range p.Spec.Containers {
			n++
			if t := imageTag(c.Image); !allowed[t] {
				bad = append(bad, fmt.Sprintf("%s/%s=%s", p.Name, c.Name, c.Image))
			}
		}
	}
	if n == 0 {
		return pending("no running pods in "+ns, q)
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fail("containers not on a release tag: "+strings.Join(bad, ", "), fmt.Sprintf("%d/%d containers", n-len(bad), n), q)
	}
	return pass(fmt.Sprintf("%d containers on %s", n, strings.Join(want, ", ")), q)
}

func targetNamespace(ctx context.Context, env *gate.Env) string {
	if hr, err := env.Cluster.Get(ctx, cluster.HelmRelease, env.App.Namespace, env.App.Name); err == nil {
		if t, _, _ := unstructured.NestedString(hr.Object, "spec", "targetNamespace"); t != "" {
			return t
		}
	}
	return env.App.Namespace
}

// isOneShot: Job pods and Helm hook/test pods are not part of the running set.
func isOneShot(p *corev1.Pod) bool {
	if _, ok := p.Annotations["helm.sh/hook"]; ok {
		return true
	}
	for _, o := range p.OwnerReferences {
		if o.Kind == "Job" {
			return true
		}
	}
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

// imageTag returns the tag of an image reference ("latest" when none).
func imageTag(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	slash := strings.LastIndex(image, "/")
	if c := strings.LastIndex(image, ":"); c > slash {
		return image[c+1:]
	}
	return "latest"
}

// HelmTests: Helm test hooks of the deployed release succeeded, where the
// chart has tests and the HelmRelease enables them.
type HelmTests struct{}

func (HelmTests) Name() string { return "helm tests passed" }

func (HelmTests) Run(ctx context.Context, env *gate.Env) gate.Result {
	ns, name := env.App.Namespace, env.App.Name
	q := fmt.Sprintf("kubectl -n %s get helmrelease %s -o jsonpath='{.status.history[0].testHooks}'", ns, name)
	hr, err := env.Cluster.Get(ctx, cluster.HelmRelease, ns, name)
	if err != nil {
		return pending("helmrelease: "+err.Error(), q)
	}
	if on, _, _ := unstructured.NestedBool(hr.Object, "spec", "test", "enable"); !on {
		return skip("tests disabled on HelmRelease")
	}
	latest, ok := latestHistory(hr)
	if !ok {
		return pending("no release history yet", q)
	}
	hooks, _ := latest["testHooks"].(map[string]any)
	if len(hooks) == 0 {
		if st, _, _, _ := cluster.Condition(hr, "TestSuccess"); st == "" {
			return skip("chart has no tests")
		}
		return pending("tests not recorded yet", q)
	}
	var failed, running []string
	for n, h := range hooks {
		m, _ := h.(map[string]any)
		switch m["phase"] {
		case "Succeeded":
		case "Failed":
			failed = append(failed, n)
		default:
			running = append(running, n)
		}
	}
	sort.Strings(failed)
	if len(failed) > 0 {
		return fail("failed: "+strings.Join(failed, ", "), fmt.Sprintf("%d/%d", len(hooks)-len(failed), len(hooks)), q)
	}
	if len(running) > 0 {
		return pending("still running: "+strings.Join(running, ", "), q)
	}
	return pass(fmt.Sprintf("%d/%d succeeded", len(hooks), len(hooks)), q)
}
