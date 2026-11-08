package checks

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/twallac10/platform-promoter/internal/config"
	"github.com/twallac10/platform-promoter/internal/gate"
)

func kvpol(name string, ready bool, actions []any, labels map[string]any) *unstructured.Unstructured {
	md := map[string]any{"name": name}
	if labels != nil {
		md["labels"] = labels
	}
	return u(map[string]any{
		"apiVersion": "policies.kyverno.io/v1", "kind": "ValidatingPolicy", "metadata": md,
		"spec":   map[string]any{"validationActions": actions},
		"status": map[string]any{"conditionStatus": map[string]any{"ready": ready, "message": "not yet"}},
	})
}

// denyIf makes dry-run creates of matching objects fail with Forbidden.
func denyIf(t *testing.T, env *gate.Env, match func(o *unstructured.Unstructured) bool) {
	t.Helper()
	fc, ok := env.Cluster.Dynamic.(*dynfake.FakeDynamicClient)
	if !ok {
		t.Fatal("not a fake dynamic client")
	}
	fc.PrependReactor("create", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ca := a.(k8stesting.CreateAction)
		o, _ := ca.GetObject().(*unstructured.Unstructured)
		if o != nil && match(o) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: a.GetResource().Resource}, o.GetName(), fmt.Errorf("denied by policy"))
		}
		return false, nil, nil
	})
}

func isPrivileged(o *unstructured.Unstructured) bool {
	cs, _, _ := unstructured.NestedSlice(o.Object, "spec", "containers")
	for _, c := range cs {
		if b, _, _ := unstructured.NestedBool(c.(map[string]any), "securityContext", "privileged"); b {
			return true
		}
	}
	return false
}

func kwh(kind, name string, labels map[string]any, svcNS, svc string) *unstructured.Unstructured {
	return u(map[string]any{
		"apiVersion": "admissionregistration.k8s.io/v1", "kind": kind,
		"metadata": map[string]any{"name": name, "labels": labels},
		"webhooks": []any{map[string]any{"name": "w", "clientConfig": map[string]any{
			"service": map[string]any{"namespace": svcNS, "name": svc}}}},
	})
}

func eps(ns, svc string, ready bool) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: svc + "-abc", Namespace: ns, Labels: map[string]string{"kubernetes.io/service-name": svc}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}}},
	}
}

var kapp = config.App{Name: "kyverno", Namespace: "kyverno"}

func TestKyvernoWebhooks(t *testing.T) {
	ctx := context.Background()
	lbl := map[string]any{kyvernoWebhookLabel: "kyverno"}
	vw := kwh("ValidatingWebhookConfiguration", "kyverno-resource-validating-webhook-cfg", lbl, "kyverno", "kyverno-svc")
	mw := kwh("MutatingWebhookConfiguration", "kyverno-resource-mutating-webhook-cfg", lbl, "kyverno", "kyverno-svc")
	other := kwh("ValidatingWebhookConfiguration", "cert-manager-webhook", nil, "cert-manager", "cm")
	tests := []struct {
		name string
		objs []runtime.Object
		want gate.Status
	}{
		{"pass", []runtime.Object{vw, mw, other, eps("kyverno", "kyverno-svc", true)}, gate.Pass},
		{"no validating webhook", []runtime.Object{mw, other}, gate.Pending},
		{"only foreign webhook", []runtime.Object{other}, gate.Pending},
		{"endpoint not ready", []runtime.Object{vw, eps("kyverno", "kyverno-svc", false)}, gate.Pending},
		{"no endpoint slice", []runtime.Object{vw}, gate.Pending},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := KyvernoWebhooks{}.Run(ctx, newEnv(t, kapp, tc.objs, nil))
			expect(t, r, tc.want)
			if r.Query == "" {
				t.Error("missing query")
			}
		})
	}
}

func TestKyvernoPoliciesReady(t *testing.T) {
	ctx := context.Background()
	cp := func(ready string) runtime.Object {
		return u(map[string]any{"apiVersion": "kyverno.io/v1", "kind": "ClusterPolicy",
			"metadata": map[string]any{"name": "legacy"}, "status": map[string]any{"conditions": readyCond(ready)}})
	}
	tests := []struct {
		name string
		objs []runtime.Object
		want gate.Status
	}{
		{"pass", []runtime.Object{kvpol("a", true, []any{"Audit"}, nil), kvpol("b", true, []any{"Deny"}, nil)}, gate.Pass},
		{"pass with legacy", []runtime.Object{kvpol("a", true, nil, nil), cp("True")}, gate.Pass},
		{"zero policies", nil, gate.Pending},
		{"not ready", []runtime.Object{kvpol("a", true, nil, nil), kvpol("b", false, nil, nil)}, gate.Pending},
		{"legacy not ready", []runtime.Object{kvpol("a", true, nil, nil), cp("False")}, gate.Pending},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			expect(t, KyvernoPoliciesReady{}.Run(ctx, newEnv(t, kapp, tc.objs, nil)), tc.want)
		})
	}
}

func TestKyvernoDryRun(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		objs    []runtime.Object
		denyBad bool
		denyAll bool
		want    gate.Status
		value   string
	}{
		{"deny mode enforced", []runtime.Object{kvpol(kyvernoPrivPolicy, true, []any{"Deny"}, nil)}, true, false, gate.Pass, "good admitted, bad denied (Deny)"},
		{"audit mode admits", []runtime.Object{kvpol(kyvernoPrivPolicy, true, []any{"Audit"}, nil)}, false, false, gate.Pass, "good admitted, bad admitted (Audit)"},
		{"deny mode but bad admitted", []runtime.Object{kvpol(kyvernoPrivPolicy, true, []any{"Deny"}, nil)}, false, false, gate.Pending, ""},
		{"audit mode but bad denied", []runtime.Object{kvpol(kyvernoPrivPolicy, true, []any{"Audit"}, nil)}, true, false, gate.Pending, ""},
		{"good pod denied", []runtime.Object{kvpol(kyvernoPrivPolicy, true, []any{"Deny"}, nil)}, true, true, gate.Fail, ""},
		{"policy missing", nil, false, false, gate.Skip, ""},
		{"policy not ready", []runtime.Object{kvpol(kyvernoPrivPolicy, false, []any{"Deny"}, nil)}, true, false, gate.Pending, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t, kapp, tc.objs, nil)
			denyIf(t, env, func(o *unstructured.Unstructured) bool {
				return tc.denyAll && o.GetKind() == "Pod" || tc.denyBad && isPrivileged(o)
			})
			r := KyvernoDryRun{}.Run(ctx, env)
			expect(t, r, tc.want)
			if tc.value != "" && r.Value != tc.value {
				t.Errorf("value = %q, want %q", r.Value, tc.value)
			}
		})
	}
}

func TestKyvernoDryRunOtherErrorIsPending(t *testing.T) {
	env := newEnv(t, kapp, []runtime.Object{kvpol(kyvernoPrivPolicy, true, []any{"Deny"}, nil)}, nil)
	env.Cluster.Dynamic.(*dynfake.FakeDynamicClient).PrependReactor("create", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("connection refused")
	})
	expect(t, KyvernoDryRun{}.Run(context.Background(), env), gate.Pending)
}

func preport(name string, fail any) runtime.Object {
	return u(map[string]any{"apiVersion": "wgpolicyk8s.io/v1alpha2", "kind": "PolicyReport",
		"metadata": map[string]any{"name": name, "namespace": "canary"},
		"summary":  map[string]any{"fail": fail, "pass": int64(3)}})
}

func TestKyvernoCanaryReports(t *testing.T) {
	ctx := context.Background()
	two := []runtime.Object{preport("a", int64(1)), preport("b", float64(1))}
	tests := []struct {
		name     string
		objs     []runtime.Object
		baseline map[string]float64
		want     gate.Status
		recorded float64
	}{
		{"no baseline", two, nil, gate.Pass, 2},
		{"equal baseline", two, map[string]float64{kyvernoBaselineKey: 2}, gate.Pass, 2},
		{"fewer than baseline", two, map[string]float64{kyvernoBaselineKey: 5}, gate.Pass, 2},
		{"increase", two, map[string]float64{kyvernoBaselineKey: 1}, gate.Fail, 2},
		{"no reports", nil, nil, gate.Pending, -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t, kapp, tc.objs, nil, func(e *gate.Env) { e.Baseline = tc.baseline })
			r := KyvernoCanaryReports{}.Run(ctx, env)
			expect(t, r, tc.want)
			got, ok := env.Metrics()[kyvernoBaselineKey]
			if tc.recorded < 0 && ok {
				t.Error("metric recorded despite no reports")
			}
			if tc.recorded >= 0 && got != tc.recorded {
				t.Errorf("recorded %v, want %v", got, tc.recorded)
			}
		})
	}
}

var _ = corev1.Pod{}

func TestKyvernoRegistered(t *testing.T) {
	for _, n := range []string{"kyverno", "kyverno-policies", "guardrails"} {
		f, ok := Sets[n]
		if !ok || len(f()) == 0 {
			t.Errorf("check set %q not registered", n)
		}
	}
}
