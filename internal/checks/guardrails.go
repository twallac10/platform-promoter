package checks

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/twallac10/platform-promoter/internal/cluster"
	"github.com/twallac10/platform-promoter/internal/gate"
)

func init() {
	Register("guardrails", func() []gate.Check {
		return []gate.Check{&GuardrailPoliciesReady{}, &GuardrailDryRun{}}
	})
}

const (
	guardrailsNS          = "guardrails"
	guardrailsHR          = "guardrails"
	guardrailsDefaultPref = "oci://ghcr.io/twallac10/charts/"
)

var guardrailPolicies = []string{"deny-helmrepository", "restrict-ocirepository-url"}

// GuardrailPoliciesReady: both guard policies exist and are ready.
type GuardrailPoliciesReady struct{}

func (GuardrailPoliciesReady) Name() string { return "guardrail policies ready" }

func (GuardrailPoliciesReady) Run(ctx context.Context, env *gate.Env) gate.Result {
	q := "kubectl get validatingpolicies " + strings.Join(guardrailPolicies, " ")
	var problems []string
	for _, n := range guardrailPolicies {
		p, err := env.Cluster.Dynamic.Resource(kyvernoVPolGVR).Get(ctx, n, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				problems = append(problems, n+" missing")
				continue
			}
			return pending("get "+n+": "+err.Error(), q)
		}
		if ok, why := kyvernoPolicyReady(p); !ok {
			problems = append(problems, n+" not ready ("+why+")")
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return pending(strings.Join(problems, "; "), q)
	}
	return pass(fmt.Sprintf("%d/%d guard policies ready", len(guardrailPolicies), len(guardrailPolicies)), q)
}

// GuardrailDryRun: server-side dry-run proves the guardrails deny what they
// should and admit what they should.
type GuardrailDryRun struct{}

func (GuardrailDryRun) Name() string { return "guardrails admission dry-run" }

func (GuardrailDryRun) Run(ctx context.Context, env *gate.Env) gate.Result {
	q := fmt.Sprintf("kubectl -n %s apply --dry-run=server -f <HelmRepository | OCIRepository docker.io | OCIRepository ghcr.io prefix>", guardrailsNS)
	prefix := guardrailsDefaultPref
	if hr, err := env.Cluster.Get(ctx, cluster.HelmRelease, guardrailsNS, guardrailsHR); err == nil {
		if p, _, _ := unstructured.NestedString(hr.Object, "spec", "values", "allowedOCIPrefix"); p != "" {
			prefix = p
		}
	}
	mk := func(kind, name string, spec map[string]any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": kind,
			"metadata": map[string]any{"name": name, "namespace": guardrailsNS},
			"spec":     spec,
		}}
	}
	cases := []struct {
		label    string
		gvr      string
		obj      *unstructured.Unstructured
		wantDeny bool
		okLabel  string
	}{
		{"HelmRepository", "helm", mk("HelmRepository", "promoter-probe-helm", map[string]any{"url": "https://charts.example.com", "interval": "1h"}), true, "HelmRepository denied"},
		{"foreign OCI", "oci", mk("OCIRepository", "promoter-probe-foreign", map[string]any{"url": "oci://docker.io/x/charts/y", "interval": "1h"}), true, "foreign OCI denied"},
		{"allowed OCI", "oci", mk("OCIRepository", "promoter-probe-allowed", map[string]any{"url": prefix + "promoter-probe", "interval": "1h"}), false, "GHCR OCI admitted"},
	}
	var parts []string
	for _, c := range cases {
		gvr := cluster.OCIRepository
		if c.gvr == "helm" {
			gvr = cluster.HelmRepository
		}
		err := env.Cluster.DryRunCreate(ctx, gvr, c.obj)
		denied, other := kyvernoIsDenied(err, guardrailPolicies...)
		if other != nil {
			return pending(fmt.Sprintf("dry-run %s: %v", c.label, other), q)
		}
		if denied != c.wantDeny {
			got := "admitted"
			if denied {
				got = "denied"
			}
			want := "admitted"
			if c.wantDeny {
				want = "denied"
			}
			detail := ""
			if err != nil {
				detail = ": " + err.Error()
			}
			// Pending: the policy may not be loaded by the webhook yet; Fail at timeout.
			r := pending(fmt.Sprintf("%s was %s, want %s%s", c.label, got, want, detail), q)
			r.Value = strings.Join(append(parts, c.label+" "+got), ", ")
			return r
		}
		parts = append(parts, c.okLabel)
	}
	return pass(strings.Join(parts, ", "), q)
}
