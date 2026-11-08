package cluster

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// portForwardHTTP reaches a Service from outside the cluster by
// port-forwarding to one ready pod behind it (like `kubectl port-forward svc/x`).
// Forwards are cached per service port for the life of the process.
type portForwardHTTP struct {
	cfg  *rest.Config
	kube kubernetes.Interface

	mu    sync.Mutex
	cache map[string]string
	stops []chan struct{}
}

func (p *portForwardHTTP) BaseURL(ctx context.Context, ns, svc string, port int) (string, func(), error) {
	key := fmt.Sprintf("%s/%s:%d", ns, svc, port)
	p.mu.Lock()
	defer p.mu.Unlock()
	if u, ok := p.cache[key]; ok {
		return u, func() {}, nil
	}
	pod, targetPort, err := p.resolve(ctx, ns, svc, port)
	if err != nil {
		return "", nil, err
	}
	transport, upgrader, err := spdy.RoundTripperFor(p.cfg)
	if err != nil {
		return "", nil, err
	}
	host := strings.TrimPrefix(strings.TrimPrefix(p.cfg.Host, "https://"), "http://")
	u := &url.URL{Scheme: "https", Host: host, Path: fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", ns, pod)}
	if strings.HasPrefix(p.cfg.Host, "http://") {
		u.Scheme = "http"
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, u)
	stop, ready := make(chan struct{}), make(chan struct{})
	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{fmt.Sprintf("0:%d", targetPort)}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		return "", nil, err
	}
	errc := make(chan error, 1)
	go func() { errc <- fw.ForwardPorts() }()
	select {
	case <-ready:
	case err := <-errc:
		return "", nil, fmt.Errorf("port-forward %s: %w", key, err)
	case <-ctx.Done():
		close(stop)
		return "", nil, ctx.Err()
	}
	ports, err := fw.GetPorts()
	if err != nil || len(ports) == 0 {
		close(stop)
		return "", nil, fmt.Errorf("port-forward %s: no local port", key)
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local)
	if p.cache == nil {
		p.cache = map[string]string{}
	}
	p.cache[key] = base
	p.stops = append(p.stops, stop)
	return base, func() {}, nil
}

// Close stops every port-forward.
func (p *portForwardHTTP) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.stops {
		close(s)
	}
	p.stops, p.cache = nil, nil
}

// resolve finds a ready pod behind the service and the container port the
// service port targets.
func (p *portForwardHTTP) resolve(ctx context.Context, ns, svcName string, port int) (string, int, error) {
	svc, err := p.kube.CoreV1().Services(ns).Get(ctx, svcName, metav1.GetOptions{})
	if err != nil {
		return "", 0, err
	}
	var target intstr.IntOrString
	found := false
	for _, sp := range svc.Spec.Ports {
		if int(sp.Port) == port {
			target, found = sp.TargetPort, true
			break
		}
	}
	if !found {
		return "", 0, fmt.Errorf("service %s/%s has no port %d", ns, svcName, port)
	}
	if len(svc.Spec.Selector) == 0 {
		return "", 0, fmt.Errorf("service %s/%s has no selector", ns, svcName)
	}
	pods, err := p.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(svc.Spec.Selector).String()})
	if err != nil {
		return "", 0, err
	}
	for _, pod := range pods.Items {
		if !podReady(&pod) {
			continue
		}
		tp := target.IntValue()
		if target.Type == intstr.String {
			tp = 0
			for _, c := range pod.Spec.Containers {
				for _, cp := range c.Ports {
					if cp.Name == target.StrVal {
						tp = int(cp.ContainerPort)
					}
				}
			}
		}
		if tp == 0 {
			tp = port
		}
		return pod.Name, tp, nil
	}
	return "", 0, fmt.Errorf("service %s/%s has no ready pods", ns, svcName)
}

func podReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// PodReady is exported for checks.
func PodReady(p *corev1.Pod) bool { return podReady(p) }
