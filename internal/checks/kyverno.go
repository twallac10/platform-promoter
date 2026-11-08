package checks

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/twallac10/platform-promoter/internal/gate"
)

func init() {
	Register("kyverno", func() []gate.Check {
		return []gate.Check{
			&KyvernoWebhooks{},
			&KyvernoPoliciesReady{},
			&KyvernoDryRun{},
			&KyvernoCanaryReports{},
		}
	})
}

// GVRs used by the Kyverno check sets.
var (
	kyvernoVPolGVR   = schema.GroupVersionResource{Group: "policies.kyverno.io", Version: "v1", Resource: "validatingpolicies"}
	kyvernoCPolGVR   = schema.GroupVersionResource{Group: "kyverno.io", Version: "v1", Resource: "clusterpolicies"}
	kyvernoReportGVR = schema.GroupVersionResource{Group: "wgpolicyk8s.io", Version: "v1alpha2", Resource: "policyreports"}
	kyvernoVWCGVR    = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingwebhookconfigurations"}
	kyvernoMWCGVR    = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "mutatingwebhookconfigurations"}
	kyvernoPodGVR    = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	kyvernoCanaryNS  = "canary"
	// Dry-run pods go to the promoter's own namespace: it exists wherever a
	// gate runs, unlike the canary, which is installed after every app.
	kyvernoDryRunNS    = "promoter"
	kyvernoPrivPolicy  = "disallow-privileged-containers"
	kyvernoBaselineKey = "kyverno.canary_policy_failures"
)

// kyvernoWebhookLabel is set by Kyverno on the webhook configurations it manages.
const kyvernoWebhookLabel = "webhook.kyverno.io/managed-by"

// ---- shared helpers (kyverno-prefixed) ----

// kyvernoPolicyReady reads status.conditionStatus.ready of a ValidatingPolicy.
func kyvernoPolicyReady(p *unstructured.Unstructured) (bool, string) {
	ready, found, _ := unstructured.NestedBool(p.Object, "status", "conditionStatus", "ready")
	if found && ready {
		return true, ""
	}
	msg, _, _ := unstructured.NestedString(p.Object, "status", "conditionStatus", "message")
	if !found {
		return false, "no status yet"
	}
	if msg == "" {
		msg = "ready=false"
	}
	return false, msg
}

// kyvernoPolicyDenies reports whether spec.validationActions contains Deny.
func kyvernoPolicyDenies(p *unstructured.Unstructured) bool {
	return kyvernoHasAction(p, "Deny")
}

func kyvernoHasAction(p *unstructured.Unstructured, action string) bool {
	acts, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "validationActions")
	for _, a := range acts {
		if a == action {
			return true
		}
	}
	return false
}

func kyvernoModeName(p *unstructured.Unstructured) string {
	acts, _, _ := unstructured.NestedStringSlice(p.Object, "spec", "validationActions")
	return strings.Join(acts, ",")
}

// kyvernoIsDenied classifies an admission error: denied (true, nil) or an
// unrelated failure (false, err). A nil error is "admitted" (false, nil).
func kyvernoIsDenied(err error, policyNames ...string) (denied bool, other error) {
	if err == nil {
		return false, nil
	}
	if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
		return true, nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "denied") {
		return true, nil
	}
	for _, n := range policyNames {
		if n != "" && strings.Contains(msg, strings.ToLower(n)) {
			return true, nil
		}
	}
	return false, err
}

// kyvernoPod builds a pod for dry-run. Good pods are restricted-compliant;
// bad pods are privileged.
func kyvernoPod(name, ns string, privileged bool) *unstructured.Unstructured {
	sc := map[string]any{
		"allowPrivilegeEscalation": false,
		"runAsNonRoot":             true,
		"runAsUser":                int64(10001),
		"capabilities":             map[string]any{"drop": []any{"ALL"}},
		"seccompProfile":           map[string]any{"type": "RuntimeDefault"},
	}
	if privileged {
		sc = map[string]any{"privileged": true}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": ns,
			"labels": map[string]any{"app.kubernetes.io/managed-by": "platform-promoter"}},
		"spec": map[string]any{
			"containers": []any{map[string]any{
				"name": "probe", "image": "registry.k8s.io/pause:3.9", "securityContext": sc,
			}},
		},
	}}
}

// kyvernoDryRunPods dry-runs the good and bad pods in ns. wantBadDenied says
// what the bad pod must do. Returns a finished result.
func kyvernoDryRunPods(ctx context.Context, env *gate.Env, ns string, wantBadDenied bool, policyNames []string, modeLabel, query string) gate.Result {
	good := kyvernoPod("promoter-known-good", ns, false)
	gErr := env.Cluster.DryRunCreate(ctx, kyvernoPodGVR, good)
	if d, other := kyvernoIsDenied(gErr, policyNames...); other != nil {
		return pending("dry-run known-good pod: "+other.Error(), query)
	} else if d {
		return fail("known-good pod was denied: "+gErr.Error(), "good denied", query)
	}
	bad := kyvernoPod("promoter-known-bad", ns, true)
	bErr := env.Cluster.DryRunCreate(ctx, kyvernoPodGVR, bad)
	badDenied, other := kyvernoIsDenied(bErr, policyNames...)
	if other != nil {
		return pending("dry-run known-bad pod: "+other.Error(), query)
	}
	val := fmt.Sprintf("good admitted, bad %s (%s)", map[bool]string{true: "denied", false: "admitted"}[badDenied], modeLabel)
	if badDenied != wantBadDenied {
		want := "admitted"
		if wantBadDenied {
			want = "denied"
		}
		// Pending, not Fail: right after an upgrade the webhook may not have
		// loaded the policies yet. Still wrong at the timeout -> Fail.
		r := pending(fmt.Sprintf("privileged pod should be %s in %s mode", want, modeLabel), query)
		r.Value = val
		return r
	}
	return pass(val, query)
}

func kyvernoNotFoundOrNoKind(err error) bool {
	return apierrors.IsNotFound(err) || meta.IsNoMatchError(err)
}

// ---- 1. webhooks ----

// KyvernoWebhooks: Kyverno's admission webhooks are registered and their
// services have ready endpoints.
type KyvernoWebhooks struct{}

func (KyvernoWebhooks) Name() string { return "kyverno admission webhooks ready" }

func (KyvernoWebhooks) Run(ctx context.Context, env *gate.Env) gate.Result {
	q := "kubectl get validatingwebhookconfigurations,mutatingwebhookconfigurations -l " + kyvernoWebhookLabel + "=kyverno"
	type svcRef struct{ ns, name string }
	svcs := map[svcRef]bool{}
	var nValidating, nMutating int
	for _, g := range []struct {
		gvr schema.GroupVersionResource
		n   *int
	}{{kyvernoVWCGVR, &nValidating}, {kyvernoMWCGVR, &nMutating}} {
		list, err := env.Cluster.Dynamic.Resource(g.gvr).List(ctx, metav1.ListOptions{})
		if err != nil {
			return pending("list "+g.gvr.Resource+": "+err.Error(), q)
		}
		for i := range list.Items {
			o := &list.Items[i]
			if o.GetLabels()[kyvernoWebhookLabel] != "kyverno" && !strings.HasPrefix(o.GetName(), "kyverno-") {
				continue
			}
			*g.n++
			whs, _, _ := unstructured.NestedSlice(o.Object, "webhooks")
			for _, w := range whs {
				wm, _ := w.(map[string]any)
				ns, _, _ := unstructured.NestedString(wm, "clientConfig", "service", "namespace")
				name, _, _ := unstructured.NestedString(wm, "clientConfig", "service", "name")
				if name != "" {
					svcs[svcRef{ns, name}] = true
				}
			}
		}
	}
	if nValidating == 0 {
		return pending("no Kyverno ValidatingWebhookConfiguration registered yet", q)
	}
	var notReady []string
	for s := range svcs {
		sl, err := env.Cluster.Kube.DiscoveryV1().EndpointSlices(s.ns).List(ctx, metav1.ListOptions{
			LabelSelector: "kubernetes.io/service-name=" + s.name})
		if err != nil {
			return pending(fmt.Sprintf("list endpointslices %s/%s: %v", s.ns, s.name, err), q)
		}
		ready := 0
		for _, sli := range sl.Items {
			for _, ep := range sli.Endpoints {
				if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
					ready++
				}
			}
		}
		if ready == 0 {
			notReady = append(notReady, s.ns+"/"+s.name)
		}
	}
	if len(notReady) > 0 {
		sort.Strings(notReady)
		return pending("no ready endpoints for service(s): "+strings.Join(notReady, ", "), q)
	}
	return pass(fmt.Sprintf("%d validating + %d mutating configs, %d services with ready endpoints", nValidating, nMutating, len(svcs)), q)
}

// ---- 2. all policies ready ----

// KyvernoPoliciesReady: every ValidatingPolicy (and legacy ClusterPolicy) is Ready.
type KyvernoPoliciesReady struct{}

func (KyvernoPoliciesReady) Name() string { return "kyverno policies ready" }

func (KyvernoPoliciesReady) Run(ctx context.Context, env *gate.Env) gate.Result {
	q := "kubectl get validatingpolicies.policies.kyverno.io,clusterpolicies.kyverno.io"
	vp, err := env.Cluster.Dynamic.Resource(kyvernoVPolGVR).List(ctx, metav1.ListOptions{})
	if err != nil && !kyvernoNotFoundOrNoKind(err) {
		return pending("list validatingpolicies: "+err.Error(), q)
	}
	var total int
	var notReady []string
	if vp != nil {
		for i := range vp.Items {
			total++
			if ok, why := kyvernoPolicyReady(&vp.Items[i]); !ok {
				notReady = append(notReady, vp.Items[i].GetName()+" ("+why+")")
			}
		}
	}
	cp, err := env.Cluster.Dynamic.Resource(kyvernoCPolGVR).List(ctx, metav1.ListOptions{})
	if err != nil && !kyvernoNotFoundOrNoKind(err) {
		return pending("list clusterpolicies: "+err.Error(), q)
	}
	if cp != nil {
		for i := range cp.Items {
			total++
			if st, _, msg, _ := conditionOf(&cp.Items[i], "Ready"); st != "True" {
				notReady = append(notReady, "ClusterPolicy/"+cp.Items[i].GetName()+" ("+msg+")")
			}
		}
	}
	if total == 0 {
		return pending("no policies installed yet (kyverno-policies/guardrails not deployed)", q)
	}
	if len(notReady) > 0 {
		sort.Strings(notReady)
		return pending(fmt.Sprintf("%d/%d policies not ready: %s", len(notReady), total, strings.Join(notReady, ", ")), q)
	}
	return pass(fmt.Sprintf("%d/%d policies ready", total, total), q)
}

func conditionOf(o *unstructured.Unstructured, t string) (status, reason, msg string, ok bool) {
	conds, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == t {
			status, _ = m["status"].(string)
			reason, _ = m["reason"].(string)
			msg, _ = m["message"].(string)
			return status, reason, msg, true
		}
	}
	return "", "", "", false
}

// ---- 3. dry-run ----

// KyvernoDryRun: server-side dry-run of a good and a bad pod in the canary
// namespace behaves as the policy's mode says.
type KyvernoDryRun struct{}

func (KyvernoDryRun) Name() string { return "kyverno admission dry-run" }

func (KyvernoDryRun) Run(ctx context.Context, env *gate.Env) gate.Result {
	q := fmt.Sprintf("kubectl -n %s apply --dry-run=server -f <privileged pod>; kubectl get validatingpolicy %s -o jsonpath='{.spec.validationActions}'", kyvernoDryRunNS, kyvernoPrivPolicy)
	p, err := env.Cluster.Dynamic.Resource(kyvernoVPolGVR).Get(ctx, kyvernoPrivPolicy, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return skip("ValidatingPolicy " + kyvernoPrivPolicy + " not installed")
		}
		return pending("get policy: "+err.Error(), q)
	}
	if ok, why := kyvernoPolicyReady(p); !ok {
		return pending(kyvernoPrivPolicy+" not ready: "+why, q)
	}
	deny := kyvernoPolicyDenies(p)
	mode := "Audit"
	if deny {
		mode = "Deny"
	}
	return kyvernoDryRunPods(ctx, env, kyvernoDryRunNS, deny, []string{kyvernoPrivPolicy}, mode, q)
}

// ---- 4. canary policy report failures ----

// KyvernoCanaryReports: PolicyReport failures in the canary namespace do not
// exceed the pre-upgrade baseline.
type KyvernoCanaryReports struct{}

func (KyvernoCanaryReports) Name() string { return "no new canary policy failures" }

func (KyvernoCanaryReports) Run(ctx context.Context, env *gate.Env) gate.Result {
	q := fmt.Sprintf("kubectl -n %s get policyreports.wgpolicyk8s.io -o jsonpath='{.items[*].summary.fail}'", kyvernoCanaryNS)
	if _, err := env.Cluster.Kube.CoreV1().Namespaces().Get(ctx, kyvernoCanaryNS, metav1.GetOptions{}); err != nil {
		// Only during bootstrap: the canary is installed after every platform
		// app. Once it exists, the operator re-gates (it watches the canary).
		return skip("canary not deployed yet")
	}
	list, err := env.Cluster.Dynamic.Resource(kyvernoReportGVR).Namespace(kyvernoCanaryNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		return pending("list policyreports: "+err.Error(), q)
	}
	if len(list.Items) == 0 {
		return pending("reports not generated yet", q)
	}
	var n float64
	for i := range list.Items {
		v, _, _ := unstructured.NestedFieldNoCopy(list.Items[i].Object, "summary", "fail")
		n += kyvernoNum(v)
	}
	env.Record(kyvernoBaselineKey, n)
	base, ok := env.Baseline[kyvernoBaselineKey]
	if !ok {
		return pass(fmt.Sprintf("%g failures (no baseline yet)", n), q)
	}
	if n > base {
		return fail(fmt.Sprintf("canary policy failures rose from %g to %g", base, n), fmt.Sprintf("%g failures (baseline %g)", n, base), q)
	}
	return pass(fmt.Sprintf("%g failures (baseline %g)", n, base), q)
}

func kyvernoNum(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case int32:
		return float64(x)
	case float64:
		return x
	}
	return 0
}
