package checks

import (
	"context"
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/twallac10/platform-promoter/internal/cluster"
	"github.com/twallac10/platform-promoter/internal/gate"
)

func init() {
	Register("kyverno-policies", func() []gate.Check {
		return []gate.Check{
			&KyvernoChartPoliciesReady{},
			&KyvernoPolicyMode{},
			&KyvernoPolicyDryRun{},
		}
	})
}

// The chart labels every policy app.kubernetes.io/part-of=kyverno-policies
// (app.kubernetes.io/instance is the Helm release name, which is also
// kyverno-policies under Flux, but part-of does not depend on that).
const (
	kyvernoPoliciesSelector = "app.kubernetes.io/part-of=kyverno-policies"
	kyvernoPoliciesNS       = "kyverno-policies"
	kyvernoPoliciesHR       = "kyverno-policies"
)

func kyvernoChartPolicies(ctx context.Context, env *gate.Env) ([]unstructured.Unstructured, error) {
	l, err := env.Cluster.Dynamic.Resource(kyvernoVPolGVR).List(ctx, metav1.ListOptions{LabelSelector: kyvernoPoliciesSelector})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}

// kyvernoPoliciesExpectedMode reads the HelmRelease: returns enforce (true =
// Enforce) and the per-policy override names to ignore.
func kyvernoPoliciesExpectedMode(ctx context.Context, env *gate.Env) (enforce bool, overrides map[string]bool, err error) {
	hr, err := env.Cluster.Get(ctx, cluster.HelmRelease, kyvernoPoliciesNS, kyvernoPoliciesHR)
	if err != nil {
		return false, nil, err
	}
	act, _, _ := unstructured.NestedString(hr.Object, "spec", "values", "validationFailureAction")
	by, _, _ := unstructured.NestedMap(hr.Object, "spec", "values", "validationFailureActionByPolicy")
	overrides = map[string]bool{}
	for k := range by {
		overrides[k] = true
	}
	return strings.EqualFold(act, "Enforce"), overrides, nil
}

func kyvernoActionName(enforce bool) string {
	if enforce {
		return "Enforce"
	}
	return "Audit"
}

// KyvernoChartPoliciesReady: the chart's policies exist and are Ready.
type KyvernoChartPoliciesReady struct{}

func (KyvernoChartPoliciesReady) Name() string { return "kyverno-policies ready" }

func (KyvernoChartPoliciesReady) Run(ctx context.Context, env *gate.Env) gate.Result {
	q := "kubectl get validatingpolicies.policies.kyverno.io -l " + kyvernoPoliciesSelector
	items, err := kyvernoChartPolicies(ctx, env)
	if err != nil {
		return pending("list validatingpolicies: "+err.Error(), q)
	}
	if len(items) == 0 {
		return pending("no ValidatingPolicies with "+kyvernoPoliciesSelector+" yet", q)
	}
	var notReady []string
	for i := range items {
		if ok, why := kyvernoPolicyReady(&items[i]); !ok {
			notReady = append(notReady, items[i].GetName()+" ("+why+")")
		}
	}
	if len(notReady) > 0 {
		sort.Strings(notReady)
		return pending(fmt.Sprintf("%d/%d not ready: %s", len(notReady), len(items), strings.Join(notReady, ", ")), q)
	}
	return pass(fmt.Sprintf("%d/%d policies ready", len(items), len(items)), q)
}

// KyvernoPolicyMode: policies' validationActions match the HelmRelease's
// validationFailureAction.
type KyvernoPolicyMode struct{}

func (KyvernoPolicyMode) Name() string { return "kyverno-policies enforcement mode" }

func (KyvernoPolicyMode) Run(ctx context.Context, env *gate.Env) gate.Result {
	q := fmt.Sprintf("kubectl -n %s get helmrelease %s -o jsonpath='{.spec.values.validationFailureAction}'; kubectl get validatingpolicies -l %s -o custom-columns=NAME:.metadata.name,ACTIONS:.spec.validationActions",
		kyvernoPoliciesNS, kyvernoPoliciesHR, kyvernoPoliciesSelector)
	enforce, overrides, err := kyvernoPoliciesExpectedMode(ctx, env)
	if err != nil {
		return pending("get helmrelease: "+err.Error(), q)
	}
	items, err := kyvernoChartPolicies(ctx, env)
	if err != nil {
		return pending("list validatingpolicies: "+err.Error(), q)
	}
	if len(items) == 0 {
		return pending("no ValidatingPolicies with "+kyvernoPoliciesSelector+" yet", q)
	}
	var bad []string
	checked := 0
	for i := range items {
		p := &items[i]
		if overrides[p.GetName()] {
			continue
		}
		checked++
		var ok bool
		if enforce {
			ok = kyvernoPolicyDenies(p)
		} else {
			ok = kyvernoHasAction(p, "Audit") && !kyvernoPolicyDenies(p)
		}
		if !ok {
			bad = append(bad, fmt.Sprintf("%s=[%s]", p.GetName(), kyvernoModeName(p)))
		}
	}
	want := kyvernoActionName(enforce)
	if len(bad) > 0 {
		sort.Strings(bad)
		return fail(fmt.Sprintf("HelmRelease says %s but policies differ: %s", want, strings.Join(bad, ", ")),
			fmt.Sprintf("%d/%d match %s", checked-len(bad), checked, want), q)
	}
	return pass(fmt.Sprintf("%d policies match %s (%d overridden)", checked, want, len(overrides)), q)
}

// KyvernoPolicyDryRun: a privileged pod is denied under Enforce, admitted under Audit.
type KyvernoPolicyDryRun struct{}

func (KyvernoPolicyDryRun) Name() string { return "kyverno-policies admission dry-run" }

func (KyvernoPolicyDryRun) Run(ctx context.Context, env *gate.Env) gate.Result {
	q := fmt.Sprintf("kubectl -n %s apply --dry-run=server -f <privileged pod>", kyvernoCanaryNS)
	enforce, _, err := kyvernoPoliciesExpectedMode(ctx, env)
	if err != nil {
		return pending("get helmrelease: "+err.Error(), q)
	}
	return kyvernoDryRunPods(ctx, env, kyvernoCanaryNS, enforce, []string{kyvernoPrivPolicy}, kyvernoActionName(enforce), q)
}
