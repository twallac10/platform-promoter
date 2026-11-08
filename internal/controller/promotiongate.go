// Package controller reconciles PromotionGates: one per app, in every tier
// cluster. It watches the app's Flux Kustomization and HelmRelease, runs the
// evidence gate on this cluster when there is something new to attest,
// records the outcome on GitHub (authoritative), mirrors it into the
// PromotionGate status, and opens the next tier's PR when the whole tier passed.
package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "github.com/twallac10/platform-promoter/api/v1alpha1"
	"github.com/twallac10/platform-promoter/internal/config"
	"github.com/twallac10/platform-promoter/internal/ledger"
	"github.com/twallac10/platform-promoter/internal/promote"
	"github.com/twallac10/platform-promoter/internal/release"
)

// Timing. No soak: these only bound retries of a gate whose result is
// already recorded.
var (
	RetryFailedOnChange = 2 * time.Minute  // Flux reconciled something new since the failed gate
	RetryFailedIdle     = 30 * time.Minute // periodic retry of a failed gate
	RetryStale          = 15 * time.Minute // in_progress left behind (crash)
	ReopenPR            = 6 * time.Hour    // passed, but the next tier still lacks it
	StuckAfter          = 10 * time.Minute // Flux not Ready this long: gate anyway, to record the failure
)

// CanaryKustomization is the fleet's Kustomization for the canary tenant.
const CanaryKustomization = "canary"

var (
	kustomizationGVK = schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"}
	helmReleaseGVK   = schema.GroupVersionKind{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"}
)

// Gater runs the gate (and promotion). *promote.Promoter implements it.
type Gater interface {
	Check(ctx context.Context, app, tier string) (*promote.Result, error)
	Promote(ctx context.Context, app, from string) (string, *promote.Result, error)
}

type Reconciler struct {
	client.Client
	Fleet   func(context.Context) (*config.Fleet, error)
	Ledger  ledger.Ledger
	Gater   Gater
	Tier    string
	Cluster string // this cluster's name in tiers.yaml
	// Namespace holds the PromotionGates (named after their app).
	Namespace string
	Resync    time.Duration
	Now       func() time.Time

	// running holds the apps whose gate this process is running right now.
	// An in_progress Deployment for any other app was left by a previous
	// operator process (one active leader per cluster), so it is stale.
	mu      sync.Mutex
	running map[string]bool
}

func (r *Reconciler) setRunning(app string, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running == nil {
		r.running = map[string]bool{}
	}
	if on {
		r.running[app] = true
	} else {
		delete(r.running, app)
	}
}

func (r *Reconciler) isRunning(app string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running[app]
}

// +kubebuilder:rbac:groups=platform.twallac10.io,resources=promotiongates,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.twallac10.io,resources=promotiongates/status,verbs=get;update;patch

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var pg v1.PromotionGate
	if err := r.Get(ctx, req.NamespacedName, &pg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := pg.DeepCopy()
	pg.Status.ObservedGeneration = pg.Generation
	pg.Status.Tier, pg.Status.Cluster = r.Tier, r.Cluster

	if pg.Spec.Suspend {
		setPhase(&pg, v1.PhaseSuspended, "suspended")
		return ctrl.Result{}, r.patchStatus(ctx, orig, &pg)
	}
	d, err := r.decide(ctx, &pg)
	if err != nil {
		setPhase(&pg, v1.PhaseError, err.Error())
		if perr := r.patchStatus(ctx, orig, &pg); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{}, err // backoff
	}
	if !d.run {
		setPhase(&pg, d.phase, d.why)
		return ctrl.Result{RequeueAfter: r.requeue(d.after)}, r.patchStatus(ctx, orig, &pg)
	}

	// Run the gate. Show Checking first so `kubectl get pg` reflects it.
	log.Info("gate", "app", pg.Spec.App, "why", d.why)
	start := metav1.NewTime(r.now())
	pg.Status.LastGate = &v1.GateRun{Digest: d.rel.Digest, Version: d.rel.Version, StartTime: start, ObservedRevision: d.fingerprint}
	setPhase(&pg, v1.PhaseChecking, d.why)
	if err := r.patchStatus(ctx, orig, &pg); err != nil {
		return ctrl.Result{}, err
	}
	orig = pg.DeepCopy()

	r.setRunning(pg.Spec.App, true)
	defer r.setRunning(pg.Spec.App, false)
	var res *promote.Result
	var url string
	if d.last {
		res, err = r.Gater.Check(ctx, pg.Spec.App, r.Tier)
	} else {
		url, res, err = r.Gater.Promote(ctx, pg.Spec.App, r.Tier)
	}
	done := metav1.NewTime(r.now())
	pg.Status.LastGate.CompletionTime = &done
	fillGate(pg.Status.LastGate, res)

	var gf *promote.GateFailedError
	switch {
	case errors.As(err, &gf):
		setPhase(&pg, v1.PhaseFailed, pg.Status.LastGate.Summary)
	case err != nil:
		setPhase(&pg, v1.PhaseError, err.Error())
		log.Error(err, "gate", "app", pg.Spec.App)
	case url != "":
		next, _, _ := d.fleet.Next(r.Tier)
		pg.Status.Promotion = &v1.PromotionRef{TargetTier: next.Name, PullRequestURL: url, Time: done}
		setPhase(&pg, v1.PhasePassed, "promotion PR opened: "+url)
	case res != nil && res.Passed():
		msg := "passed"
		if !d.last {
			msg = "passed; waiting for the tier's other clusters or already promoted"
		}
		setPhase(&pg, v1.PhasePassed, msg)
	default:
		setPhase(&pg, v1.PhaseFailed, pg.Status.LastGate.Summary)
	}
	setGateCondition(&pg, d.rel.Digest)
	return ctrl.Result{RequeueAfter: r.requeue(0)}, r.patchStatus(ctx, orig, &pg)
}

type decision struct {
	run         bool
	why         string
	phase       string
	after       time.Duration // requeue hint when not running
	last        bool          // this is the last tier
	rel         *release.Release
	fingerprint string
	fleet       *config.Fleet
}

// decide works from GitHub (what main declares, what the ledger recorded for
// this cluster) plus the Flux state, so it is correct after restarts.
func (r *Reconciler) decide(ctx context.Context, pg *v1.PromotionGate) (*decision, error) {
	fleet, err := r.Fleet(ctx)
	if err != nil {
		return nil, err
	}
	app, err := fleet.App(pg.Spec.App)
	if err != nil {
		return nil, err
	}
	d := &decision{fleet: fleet}
	_, hasNext, err := fleet.Next(r.Tier)
	if err != nil {
		return nil, err
	}
	d.last = !hasNext

	path := "tiers/" + r.Tier + "/release.yaml"
	commit, err := r.Ledger.LastCommit(ctx, app.Repo, path)
	if err != nil {
		return nil, err
	}
	raw, ok, err := r.Ledger.File(ctx, app.Repo, path, commit)
	if err != nil || !ok {
		return nil, errOr(err, path+" missing in "+app.Repo)
	}
	if d.rel, err = release.Parse(raw); err != nil {
		return nil, err
	}
	pg.Status.Release = &v1.ReleaseRef{Digest: d.rel.Digest, Version: d.rel.Version, Commit: commit}

	flux, err := r.fluxState(ctx, app)
	if err != nil {
		return nil, err
	}
	d.fingerprint = flux.fingerprint

	dep, err := r.Ledger.LatestDeployment(ctx, app.Repo, r.Tier, app.Name, r.Cluster, false)
	if err != nil {
		return nil, err
	}
	mirrorLedger(pg, dep)

	since := time.Duration(1<<62 - 1)
	if lg := pg.Status.LastGate; lg != nil {
		t := lg.StartTime.Time
		if lg.CompletionTime != nil {
			t = lg.CompletionTime.Time
		}
		since = r.now().Sub(t)
	}
	changed := pg.Status.LastGate == nil || pg.Status.LastGate.ObservedRevision != d.fingerprint

	// Flux first: gate once the app is Ready, or once it has been stuck long
	// enough that the failure should be recorded.
	if !flux.ready && flux.notReadyFor < StuckAfter {
		d.phase, d.why = v1.PhaseWaiting, "waiting for Flux: "+flux.message
		d.after = StuckAfter - flux.notReadyFor
		return d, nil
	}
	short := d.rel.ShortDigest()
	switch {
	case dep == nil || dep.Payload.Digest != d.rel.Digest || dep.Payload.ReleaseCommit != commit:
		// A release is the whole release.yaml (digest + shared values), so a
		// values-only change (same digest, new commit) is gated too.
		d.run, d.why = true, "new release "+d.rel.Version+" ("+short+" @ "+shortSHA(commit)+")"
	case dep.State == "in_progress" || dep.State == "queued" || dep.State == "pending":
		if r.isRunning(app.Name) {
			d.phase, d.why = v1.PhaseChecking, "gate running"
			break
		}
		// Not ours: left behind by a previous operator process (restart or
		// upgrade). Re-run now. RetryStale only guards against a gate started
		// by the CLI moments ago.
		d.run, d.why = since > RetryStale || pg.Status.Phase == v1.PhaseChecking, "previous gate was interrupted"
		d.phase, d.after = v1.PhaseChecking, RetryStale-since
	case dep.State == "success":
		d.phase = v1.PhasePassed
		if d.last {
			d.why = "passed (last tier)"
			break
		}
		next, _, _ := fleet.Next(r.Tier)
		nraw, _, err := r.Ledger.File(ctx, app.Repo, "tiers/"+next.Name+"/release.yaml", "main")
		if err != nil {
			return nil, err
		}
		if bytes.Equal(nraw, raw) {
			d.phase, d.why = v1.PhasePromoted, "promoted to "+next.Name
			break
		}
		d.run, d.why = since > ReopenPR, "passed; "+next.Name+" still behind"
		d.after = ReopenPR - since
	default: // failure / error
		limit := RetryFailedIdle
		if changed {
			limit = RetryFailedOnChange
		}
		d.run, d.why = since > limit, "retry after "+dep.State
		d.phase, d.after = v1.PhaseFailed, limit-since
		if !d.run {
			d.why = "gate failed"
			if lg := pg.Status.LastGate; lg != nil && lg.Summary != "" {
				d.why = lg.Summary
			}
		}
	}
	return d, nil
}

type fluxState struct {
	ready       bool
	notReadyFor time.Duration
	message     string
	fingerprint string
}

// fluxState reads the fleet's Kustomization for the app and the app's
// HelmRelease. The fingerprint changes whenever Flux applied or attempted
// something new.
func (r *Reconciler) fluxState(ctx context.Context, app config.App) (fluxState, error) {
	var st fluxState
	ks := &unstructured.Unstructured{}
	ks.SetGroupVersionKind(kustomizationGVK)
	if err := r.Get(ctx, types.NamespacedName{Namespace: "flux-system", Name: app.Name}, ks); err != nil {
		if client.IgnoreNotFound(err) == nil {
			st.message = "Kustomization flux-system/" + app.Name + " not found"
			return st, nil
		}
		return st, err
	}
	hr := &unstructured.Unstructured{}
	hr.SetGroupVersionKind(helmReleaseGVK)
	if err := r.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: app.Name}, hr); client.IgnoreNotFound(err) != nil {
		return st, err
	}
	st.fingerprint = fingerprint(ks, hr)
	// The canary exercises every app, so its state is part of what a gate
	// observed: when it changes (e.g. it finally comes up during bootstrap), a
	// failed gate is retried soon instead of after RetryFailedIdle.
	canary := &unstructured.Unstructured{}
	canary.SetGroupVersionKind(kustomizationGVK)
	if err := r.Get(ctx, types.NamespacedName{Namespace: "flux-system", Name: CanaryKustomization}, canary); err == nil {
		st.fingerprint += " canary=" + fingerprintOne(canary)
	}
	cond := readyCondition(ks)
	st.ready = cond.status == "True"
	st.message = cond.reason + ": " + cond.message
	// Waiting on a dependency is not this release being stuck.
	if !st.ready && !cond.since.IsZero() && cond.reason != "DependencyNotReady" {
		st.notReadyFor = r.now().Sub(cond.since)
	}
	return st, nil
}

func fingerprint(ks, hr *unstructured.Unstructured) string {
	ksRev, _, _ := unstructured.NestedString(ks.Object, "status", "lastAppliedRevision")
	hrRev, _, _ := unstructured.NestedString(hr.Object, "status", "lastAttemptedRevision")
	hrCfg, _, _ := unstructured.NestedString(hr.Object, "status", "lastAttemptedConfigDigest")
	return fmt.Sprintf("ks=%s hr=%s cfg=%s ready=%s/%s", ksRev, hrRev, hrCfg, readyCondition(ks).status, readyCondition(hr).status)
}

type condition struct {
	status, reason, message string
	since                   time.Time
}

func readyCondition(o *unstructured.Unstructured) condition {
	if o == nil {
		return condition{}
	}
	conds, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] != "Ready" {
			continue
		}
		var out condition
		out.status, _ = m["status"].(string)
		out.reason, _ = m["reason"].(string)
		out.message, _ = m["message"].(string)
		if s, _ := m["lastTransitionTime"].(string); s != "" {
			out.since, _ = time.Parse(time.RFC3339, s)
		}
		return out
	}
	return condition{}
}

// mirrorLedger copies what GitHub recorded into status when status has no
// newer run of its own (e.g. after a restart or a gate run from the CLI).
func mirrorLedger(pg *v1.PromotionGate, dep *ledger.Deployment) {
	if dep == nil {
		return
	}
	lg := pg.Status.LastGate
	if lg != nil && lg.Digest == dep.Payload.Digest && lg.CompletionTime != nil {
		return
	}
	if lg == nil || lg.Digest != dep.Payload.Digest {
		pg.Status.LastGate = &v1.GateRun{Digest: dep.Payload.Digest, Version: dep.Payload.Version}
		lg = pg.Status.LastGate
	}
	lg.Passed = dep.State == "success"
	lg.EvidenceURL = dep.LogURL
	lg.Summary = "recorded on GitHub: " + dep.State
}

func fillGate(g *v1.GateRun, res *promote.Result) {
	if res == nil || len(res.Reports) == 0 {
		g.Summary = "no gate report"
		return
	}
	rep := res.Reports[0]
	g.Passed, g.Summary = rep.Passed(), rep.Summary()
	if len(res.EvidenceURLs) > 0 {
		g.EvidenceURL = res.EvidenceURLs[0]
	}
	g.Results = nil
	for _, x := range rep.Results {
		g.Results = append(g.Results, v1.CheckResult{Name: x.Check, Status: string(x.Status), Value: x.Value, Reason: x.Reason})
	}
}

func setPhase(pg *v1.PromotionGate, phase, msg string) {
	pg.Status.Phase = phase
	if len(msg) > 240 {
		msg = msg[:237] + "..."
	}
	pg.Status.Message = msg
}

func setGateCondition(pg *v1.PromotionGate, digest string) {
	c := metav1.Condition{Type: v1.ConditionGatePassed, Status: metav1.ConditionFalse, Reason: "GateFailed",
		Message: pg.Status.Message, ObservedGeneration: pg.Generation}
	if lg := pg.Status.LastGate; lg != nil && lg.Passed && lg.Digest == digest {
		c.Status, c.Reason = metav1.ConditionTrue, "GatePassed"
	}
	meta.SetStatusCondition(&pg.Status.Conditions, c)
}

func (r *Reconciler) patchStatus(ctx context.Context, orig, pg *v1.PromotionGate) error {
	if pg.Status.LastGate != nil {
		setGateCondition(pg, pg.Status.LastGate.Digest)
	}
	return r.Status().Patch(ctx, pg, client.MergeFrom(orig))
}

func (r *Reconciler) requeue(hint time.Duration) time.Duration {
	if hint > 0 && hint < r.Resync {
		return hint + time.Second
	}
	return r.Resync
}

// SetupWithManager watches PromotionGates and maps changes of an app's Flux
// Kustomization (flux-system/<app>) and HelmRelease (<ns>/<app>) to its gate.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager, concurrency int) error {
	ks := &unstructured.Unstructured{}
	ks.SetGroupVersionKind(kustomizationGVK)
	hr := &unstructured.Unstructured{}
	hr.SetGroupVersionKind(helmReleaseGVK)
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.PromotionGate{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(ks, handler.EnqueueRequestsFromMapFunc(r.mapFlux), builder.WithPredicates(fluxChanged())).
		Watches(hr, handler.EnqueueRequestsFromMapFunc(r.mapFlux), builder.WithPredicates(fluxChanged())).
		WithOptions(controller.Options{MaxConcurrentReconciles: concurrency}).
		Named("promotiongate").
		Complete(r)
}

func (r *Reconciler) mapFlux(ctx context.Context, o client.Object) []reconcile.Request {
	fleet, err := r.Fleet(ctx)
	if err != nil {
		return nil
	}
	if o.GetNamespace() == "flux-system" && o.GetName() == CanaryKustomization &&
		strings.HasPrefix(o.GetObjectKind().GroupVersionKind().Group, "kustomize.") {
		var reqs []reconcile.Request
		for _, a := range fleet.Apps {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: a.Name}})
		}
		return reqs
	}
	for _, a := range fleet.Apps {
		isKS := o.GetNamespace() == "flux-system" && o.GetName() == a.Name && strings.HasPrefix(o.GetObjectKind().GroupVersionKind().Group, "kustomize.")
		isHR := o.GetNamespace() == a.Namespace && o.GetName() == a.Name && strings.HasPrefix(o.GetObjectKind().GroupVersionKind().Group, "helm.")
		if isKS || isHR {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: a.Name}}}
		}
	}
	return nil
}

// fluxChanged passes updates that change what Flux applied or its readiness,
// not every status heartbeat.
func fluxChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, ok1 := e.ObjectOld.(*unstructured.Unstructured)
			n, ok2 := e.ObjectNew.(*unstructured.Unstructured)
			if !ok1 || !ok2 {
				return true
			}
			return fingerprintOne(o) != fingerprintOne(n)
		},
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func fingerprintOne(o *unstructured.Unstructured) string {
	a, _, _ := unstructured.NestedString(o.Object, "status", "lastAppliedRevision")
	b, _, _ := unstructured.NestedString(o.Object, "status", "lastAttemptedRevision")
	c, _, _ := unstructured.NestedString(o.Object, "status", "lastAttemptedConfigDigest")
	rc := readyCondition(o)
	return strings.Join([]string{a, b, c, rc.status, rc.reason}, "|")
}

// FleetLoader caches tiers.yaml/apps.yaml and reloads them after ttl.
func FleetLoader(src string, ttl time.Duration) func(context.Context) (*config.Fleet, error) {
	var mu sync.Mutex
	var cached *config.Fleet
	var at time.Time
	return func(context.Context) (*config.Fleet, error) {
		mu.Lock()
		defer mu.Unlock()
		if cached != nil && time.Since(at) < ttl {
			return cached, nil
		}
		f, err := config.Load(src)
		if err != nil {
			if cached != nil {
				return cached, nil // keep serving the last good config
			}
			return nil, err
		}
		cached, at = f, time.Now()
		return f, nil
	}
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func errOr(err error, msg string) error {
	if err != nil {
		return err
	}
	return errors.New(msg)
}
