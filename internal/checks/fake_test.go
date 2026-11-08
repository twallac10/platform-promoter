package checks

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/twallac10/platform-promoter/internal/cluster"
	"github.com/twallac10/platform-promoter/internal/config"
	"github.com/twallac10/platform-promoter/internal/gate"
	"github.com/twallac10/platform-promoter/internal/release"
)

// Test harness shared by every check test in this package.

const testDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// listKinds registers every GVR the checks list, so the fake dynamic client
// can serve List calls.
var listKinds = map[schema.GroupVersionResource]string{
	cluster.Kustomization:  "KustomizationList",
	cluster.HelmRelease:    "HelmReleaseList",
	cluster.OCIRepository:  "OCIRepositoryList",
	cluster.HelmRepository: "HelmRepositoryList",
	{Group: "policies.kyverno.io", Version: "v1", Resource: "validatingpolicies"}:                       "ValidatingPolicyList",
	{Group: "policies.kyverno.io", Version: "v1beta1", Resource: "validatingpolicies"}:                  "ValidatingPolicyList",
	{Group: "kyverno.io", Version: "v1", Resource: "clusterpolicies"}:                                   "ClusterPolicyList",
	{Group: "wgpolicyk8s.io", Version: "v1alpha2", Resource: "policyreports"}:                           "PolicyReportList",
	{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingwebhookconfigurations"}: "ValidatingWebhookConfigurationList",
	{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "mutatingwebhookconfigurations"}:   "MutatingWebhookConfigurationList",
}

// fakeServices maps "ns/svc:port" to a handler.
type fakeServices map[string]http.Handler

func (f fakeServices) BaseURL(_ context.Context, ns, svc string, port int) (string, func(), error) {
	h, ok := f[fmt.Sprintf("%s/%s:%d", ns, svc, port)]
	if !ok {
		return "", nil, fmt.Errorf("no fake service %s/%s:%d", ns, svc, port)
	}
	s := httptest.NewServer(h)
	return s.URL, s.Close, nil
}

type fakeRepo struct{ ancestor bool }

func (r fakeRepo) IsAncestorOrEqual(context.Context, string, string) (bool, error) {
	return r.ancestor, nil
}

type envOpt func(*gate.Env)

// newEnv builds an Env for app/namespace with fake clients. Typed objects go to
// the kube fake, *unstructured.Unstructured to the dynamic fake.
func newEnv(t *testing.T, app config.App, objs []runtime.Object, svcs fakeServices, opts ...envOpt) *gate.Env {
	t.Helper()
	var typed, dyn []runtime.Object
	for _, o := range objs {
		if _, ok := o.(*unstructured.Unstructured); ok {
			dyn = append(dyn, o)
		} else {
			typed = append(typed, o)
		}
	}
	scheme := runtime.NewScheme()
	c := &cluster.Cluster{
		Name:    "fake",
		Kube:    kubefake.NewSimpleClientset(typed...),
		Dynamic: dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, dyn...),
		HTTP:    svcs,
	}
	rel := &release.Release{Digest: testDigest, Version: "1.0.0", Values: map[string]any{}}
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	env := &gate.Env{
		App: app, Tier: config.Tier{Name: "dev", Clusters: []string{"fake"}},
		Gate: config.DefaultGate, Cluster: c, Release: rel,
		ReleaseCommit: "aaaaaaa", Repo: fakeRepo{ancestor: true},
		Now: func() time.Time { return now },
	}
	for _, o := range opts {
		o(env)
	}
	return env
}

// u builds an unstructured object from a map.
func u(obj map[string]any) *unstructured.Unstructured { return &unstructured.Unstructured{Object: obj} }

func readyCond(status string) []any {
	return []any{map[string]any{"type": "Ready", "status": status, "reason": "R", "message": "m",
		"lastTransitionTime": "2026-11-08T11:50:00Z"}}
}

// promHandler serves /api/v1/query, answering by exact expression.
func promHandler(answers map[string][]Sample) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		res, ok := answers[q]
		if !ok {
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
			return
		}
		out := `{"status":"success","data":{"resultType":"vector","result":[`
		for i, s := range res {
			if i > 0 {
				out += ","
			}
			lbl := "{"
			j := 0
			for k, v := range s.Labels {
				if j > 0 {
					lbl += ","
				}
				lbl += fmt.Sprintf("%q:%q", k, v)
				j++
			}
			lbl += "}"
			out += fmt.Sprintf(`{"metric":%s,"value":[0,"%g"]}`, lbl, s.Value)
		}
		w.Write([]byte(out + "]}}"))
	})
}

func expect(t *testing.T, r gate.Result, want gate.Status) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("status = %s, want %s (reason: %s, value: %s)", r.Status, want, r.Reason, r.Value)
	}
}
