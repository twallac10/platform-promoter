package checks

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/twallac10/platform-promoter/internal/config"
	"github.com/twallac10/platform-promoter/internal/gate"
)

var grApp = config.App{Name: "guardrails", Namespace: "guardrails"}

func TestGuardrailPoliciesReady(t *testing.T) {
	ctx := context.Background()
	a, b := "deny-helmrepository", "restrict-ocirepository-url"
	tests := []struct {
		name string
		objs []runtime.Object
		want gate.Status
	}{
		{"pass", []runtime.Object{kvpol(a, true, nil, nil), kvpol(b, true, nil, nil)}, gate.Pass},
		{"one missing", []runtime.Object{kvpol(a, true, nil, nil)}, gate.Pending},
		{"one not ready", []runtime.Object{kvpol(a, true, nil, nil), kvpol(b, false, nil, nil)}, gate.Pending},
		{"none", nil, gate.Pending},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			expect(t, GuardrailPoliciesReady{}.Run(ctx, newEnv(t, grApp, tc.objs, nil)), tc.want)
		})
	}
}

func grHR(prefix string) runtime.Object {
	return u(map[string]any{"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
		"metadata": map[string]any{"name": "guardrails", "namespace": "guardrails"},
		"spec":     map[string]any{"values": map[string]any{"allowedOCIPrefix": prefix}}})
}

func grGuard(prefix string) func(o *unstructured.Unstructured) bool {
	return func(o *unstructured.Unstructured) bool {
		switch o.GetKind() {
		case "HelmRepository":
			return true
		case "OCIRepository":
			url, _, _ := unstructured.NestedString(o.Object, "spec", "url")
			return !strings.HasPrefix(url, prefix)
		}
		return false
	}
}

func TestGuardrailDryRun(t *testing.T) {
	ctx := context.Background()
	prefix := "oci://ghcr.io/twallac10/charts/"
	tests := []struct {
		name  string
		objs  []runtime.Object
		deny  func(o *unstructured.Unstructured) bool
		want  gate.Status
		value string
	}{
		{"all correct", nil, grGuard(prefix), gate.Pass, "HelmRepository denied, foreign OCI denied, GHCR OCI admitted"},
		{"custom prefix from HR", []runtime.Object{grHR("oci://example.com/c/")}, grGuard("oci://example.com/c/"), gate.Pass, ""},
		{"HelmRepository admitted", nil, func(o *unstructured.Unstructured) bool { return o.GetKind() == "OCIRepository" && grGuard(prefix)(o) }, gate.Pending, ""},
		{"foreign OCI admitted", nil, func(o *unstructured.Unstructured) bool { return o.GetKind() == "HelmRepository" }, gate.Pending, ""},
		{"allowed OCI denied", nil, func(o *unstructured.Unstructured) bool { return true }, gate.Pending, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t, grApp, tc.objs, nil)
			denyIf(t, env, tc.deny)
			r := GuardrailDryRun{}.Run(ctx, env)
			expect(t, r, tc.want)
			if tc.value != "" && r.Value != tc.value {
				t.Errorf("value = %q, want %q", r.Value, tc.value)
			}
			if r.Query == "" {
				t.Error("missing query")
			}
		})
	}
}

func TestGuardrailDryRunConnectionErrorPending(t *testing.T) {
	env := newEnv(t, grApp, nil, nil)
	env.Cluster.Dynamic.(*dynfake.FakeDynamicClient).PrependReactor("create", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("dial tcp: connection refused")
	})
	expect(t, GuardrailDryRun{}.Run(context.Background(), env), gate.Pending)
}
