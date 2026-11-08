package checks

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/twallac10/platform-promoter/internal/config"
	"github.com/twallac10/platform-promoter/internal/gate"
)

var kpApp = config.App{Name: "kyverno-policies", Namespace: "kyverno-policies"}

func kpHR(values map[string]any) runtime.Object {
	return u(map[string]any{"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
		"metadata": map[string]any{"name": "kyverno-policies", "namespace": "kyverno-policies"},
		"spec":     map[string]any{"values": values}})
}

var kpLabels = map[string]any{"app.kubernetes.io/part-of": "kyverno-policies"}

func TestKyvernoChartPoliciesReady(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		objs []runtime.Object
		want gate.Status
	}{
		{"pass", []runtime.Object{kvpol("a", true, []any{"Audit"}, kpLabels), kvpol("other", false, nil, nil)}, gate.Pass},
		{"none from chart", []runtime.Object{kvpol("other", true, nil, nil)}, gate.Pending},
		{"not ready", []runtime.Object{kvpol("a", true, nil, kpLabels), kvpol("b", false, nil, kpLabels)}, gate.Pending},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			expect(t, KyvernoChartPoliciesReady{}.Run(ctx, newEnv(t, kpApp, tc.objs, nil)), tc.want)
		})
	}
}

func TestKyvernoPolicyMode(t *testing.T) {
	ctx := context.Background()
	enf := map[string]any{"validationFailureAction": "Enforce"}
	tests := []struct {
		name string
		objs []runtime.Object
		want gate.Status
	}{
		{"default audit ok", []runtime.Object{kpHR(map[string]any{}), kvpol("a", true, []any{"Audit"}, kpLabels)}, gate.Pass},
		{"enforce ok", []runtime.Object{kpHR(enf), kvpol("a", true, []any{"Deny"}, kpLabels)}, gate.Pass},
		{"enforce but audit", []runtime.Object{kpHR(enf), kvpol("a", true, []any{"Audit"}, kpLabels)}, gate.Fail},
		{"audit but deny", []runtime.Object{kpHR(map[string]any{}), kvpol("a", true, []any{"Deny"}, kpLabels)}, gate.Fail},
		{"audit with deny+audit", []runtime.Object{kpHR(map[string]any{}), kvpol("a", true, []any{"Audit", "Deny"}, kpLabels)}, gate.Fail},
		{"override ignored", []runtime.Object{
			kpHR(map[string]any{"validationFailureActionByPolicy": map[string]any{"a": []any{}}}),
			kvpol("a", true, []any{"Deny"}, kpLabels)}, gate.Pass},
		{"no helmrelease", []runtime.Object{kvpol("a", true, []any{"Audit"}, kpLabels)}, gate.Pending},
		{"no policies", []runtime.Object{kpHR(enf)}, gate.Pending},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			expect(t, KyvernoPolicyMode{}.Run(ctx, newEnv(t, kpApp, tc.objs, nil)), tc.want)
		})
	}
}

func TestKyvernoPolicyDryRun(t *testing.T) {
	ctx := context.Background()
	enf := map[string]any{"validationFailureAction": "Enforce"}
	tests := []struct {
		name     string
		values   map[string]any
		denyBad  bool
		want     gate.Status
		noHelmRl bool
	}{
		{"enforce denies", enf, true, gate.Pass, false},
		{"enforce admits", enf, false, gate.Pending, false},
		{"audit admits", map[string]any{}, false, gate.Pass, false},
		{"audit denies", map[string]any{}, true, gate.Pending, false},
		{"no helmrelease", nil, false, gate.Pending, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var objs []runtime.Object
			if !tc.noHelmRl {
				objs = append(objs, kpHR(tc.values))
			}
			env := newEnv(t, kpApp, objs, nil)
			denyIf(t, env, func(o *unstructured.Unstructured) bool { return tc.denyBad && isPrivileged(o) })
			expect(t, KyvernoPolicyDryRun{}.Run(ctx, env), tc.want)
		})
	}
}
