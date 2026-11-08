// Package cluster gives checks read access to one cluster: typed and dynamic
// clients, HTTP access to in-cluster services, server-side dry-run, and Events.
package cluster

import (
	"context"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Well-known resources.
var (
	Kustomization  = schema.GroupVersionResource{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}
	HelmRelease    = schema.GroupVersionResource{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}
	OCIRepository  = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "ocirepositories"}
	HelmRepository = schema.GroupVersionResource{Group: "source.toolkit.fluxcd.io", Version: "v1", Resource: "helmrepositories"}
)

// Cluster is one kube context (or the in-cluster config).
type Cluster struct {
	Name    string // kube context name, or "in-cluster"
	Config  *rest.Config
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	// HTTP reaches a Service. Direct DNS in-cluster; a port-forward locally.
	HTTP ServiceHTTP
	// InCluster is true when the promoter runs inside the cluster it checks.
	InCluster bool
}

// ServiceHTTP returns a base URL ("http://host:port") for a Service port and a
// func to release it (closes the port-forward, if any).
type ServiceHTTP interface {
	BaseURL(ctx context.Context, namespace, service string, port int) (string, func(), error)
}

// FromContext builds a Cluster from a kubeconfig context (empty = in-cluster).
func FromContext(kubeContext string) (*Cluster, error) {
	var cfg *rest.Config
	var err error
	name := kubeContext
	inCluster := kubeContext == ""
	if inCluster {
		cfg, err = rest.InClusterConfig()
		name = "in-cluster"
	} else {
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(),
			&clientcmd.ConfigOverrides{CurrentContext: kubeContext},
		).ClientConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("kube config for %q: %w", name, err)
	}
	cfg.Timeout = 30 * time.Second
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	c := &Cluster{Name: name, Config: cfg, Kube: kube, Dynamic: dyn, InCluster: inCluster}
	if inCluster {
		c.HTTP = directHTTP{}
	} else {
		c.HTTP = &portForwardHTTP{cfg: cfg, kube: kube}
	}
	return c, nil
}

// Get fetches one object.
func (c *Cluster) Get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	return c.Dynamic.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
}

// DryRunCreate submits obj with server-side dry-run, so admission webhooks
// (Kyverno) run but nothing is persisted. Returns the admission error, if any.
func (c *Cluster) DryRunCreate(ctx context.Context, gvr schema.GroupVersionResource, obj *unstructured.Unstructured) error {
	ri := c.Dynamic.Resource(gvr)
	var err error
	if ns := obj.GetNamespace(); ns != "" {
		_, err = ri.Namespace(ns).Create(ctx, obj, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	} else {
		_, err = ri.Create(ctx, obj, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	}
	return err
}

// Event records a Kubernetes Event on an object (e.g. the app's HelmRelease),
// so gate outcomes show in `kubectl describe` / `kubectl events`.
func (c *Cluster) Event(ctx context.Context, ref corev1.ObjectReference, eventType, reason, message string) error {
	now := metav1.NewTime(time.Now())
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: ref.Name + ".promoter-",
			Namespace:    ref.Namespace,
		},
		InvolvedObject:      ref,
		Type:                eventType,
		Reason:              reason,
		Message:             truncate(message, 1024),
		Source:              corev1.EventSource{Component: "platform-promoter"},
		ReportingController: "platform.twallac10.io/promoter",
		ReportingInstance:   "promoter",
		FirstTimestamp:      now,
		LastTimestamp:       now,
		Count:               1,
	}
	_, err := c.Kube.CoreV1().Events(ref.Namespace).Create(ctx, ev, metav1.CreateOptions{})
	return err
}

// Condition returns status, reason and message of a status condition.
func Condition(obj *unstructured.Unstructured, condType string) (status, reason, message string, ok bool) {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == condType {
			s, _ := m["status"].(string)
			r, _ := m["reason"].(string)
			msg, _ := m["message"].(string)
			return s, r, msg, true
		}
	}
	return "", "", "", false
}

// ConditionTime returns lastTransitionTime of a condition.
func ConditionTime(obj *unstructured.Unstructured, condType string) (time.Time, bool) {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == condType {
			s, _ := m["lastTransitionTime"].(string)
			t, err := time.Parse(time.RFC3339, s)
			return t, err == nil
		}
	}
	return time.Time{}, false
}

// Reconciled reports whether status.observedGeneration has caught up.
func Reconciled(obj *unstructured.Unstructured) bool {
	og, found, _ := unstructured.NestedInt64(obj.Object, "status", "observedGeneration")
	return found && og == obj.GetGeneration()
}

type directHTTP struct{}

func (directHTTP) BaseURL(_ context.Context, ns, svc string, port int) (string, func(), error) {
	return fmt.Sprintf("http://%s.%s.svc:%d", svc, ns, port), func() {}, nil
}

// Client is the HTTP client checks should use against BaseURLs.
var Client = &http.Client{Timeout: 20 * time.Second}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
