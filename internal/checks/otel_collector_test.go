package checks

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/twallac10/platform-promoter/internal/gate"
)

func otelDeploy(replicas, available int32) runtime.Object {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "otel-collector", Namespace: "otel"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: available},
	}
}

func TestOtelReady(t *testing.T) {
	ctx := context.Background()
	expect(t, (&OtelReady{}).Run(ctx, newEnv(t, app, []runtime.Object{otelDeploy(2, 2)}, nil)), gate.Pass)
	expect(t, (&OtelReady{}).Run(ctx, newEnv(t, app, []runtime.Object{otelDeploy(2, 1)}, nil)), gate.Pending)
	expect(t, (&OtelReady{}).Run(ctx, newEnv(t, app, []runtime.Object{otelDeploy(1, 0)}, nil)), gate.Pending)
	expect(t, (&OtelReady{}).Run(ctx, newEnv(t, app, nil, nil)), gate.Pending)
}

// metricsSvc serves the text returned by *body.
func metricsSvc(body *atomic.Value) fakeServices {
	return fakeServices{"otel/otel-collector:8888": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body.Load().(string))
	})}
}

func otelMetrics(spans, failedSpans, failedMetrics int) string {
	return fmt.Sprintf(`# TYPE otelcol_receiver_accepted_spans_total counter
otelcol_receiver_accepted_spans_total{receiver="otlp",transport="grpc"} %d
otelcol_exporter_send_failed_spans_total{exporter="debug"} %d
otelcol_exporter_send_failed_metric_points_total{exporter="prometheus"} %d
`, spans, failedSpans, failedMetrics)
}

// advance moves the fake clock by d.
func advance(env *gate.Env, d time.Duration) {
	now := env.Now()
	env.Now = func() time.Time { return now.Add(d) }
}

func TestOtelAcceptedSpans(t *testing.T) {
	ctx := context.Background()
	var body atomic.Value
	body.Store(otelMetrics(10, 0, 0))
	env := newEnv(t, app, nil, metricsSvc(&body))
	c := &OtelAcceptedSpans{}

	expect(t, c.Run(ctx, env), gate.Pending) // first sample
	expect(t, c.Run(ctx, env), gate.Pending) // same instant: too early
	advance(env, 10*time.Second)
	body.Store(otelMetrics(50, 0, 0))
	expect(t, c.Run(ctx, env), gate.Pending) // 10s < 30s

	advance(env, 30*time.Second)
	r := c.Run(ctx, env)
	expect(t, r, gate.Pass)
	if r.Value != "+40 spans in 40s" {
		t.Errorf("value = %q", r.Value)
	}
	if r.Query == "" {
		t.Error("missing query")
	}
	if env.Metrics()["otel.accepted_spans"] != 40 {
		t.Errorf("metrics = %v", env.Metrics())
	}

	// No increase between two later samples: stays pending.
	advance(env, 30*time.Second)
	r = c.Run(ctx, env)
	expect(t, r, gate.Pending)
	if r.Value != "+0 spans in 30s" {
		t.Errorf("value = %q", r.Value)
	}
}

func TestOtelAcceptedSpansMetricAbsent(t *testing.T) {
	ctx := context.Background()
	var body atomic.Value
	body.Store("up 1\n")
	env := newEnv(t, app, nil, metricsSvc(&body))
	c := &OtelAcceptedSpans{}
	expect(t, c.Run(ctx, env), gate.Pending)
	advance(env, time.Minute)
	r := c.Run(ctx, env)
	expect(t, r, gate.Pending)
	if r.Reason != "no accepted-spans metric" {
		t.Errorf("reason = %q", r.Reason)
	}
}

func TestOtelAcceptedSpansUnreachable(t *testing.T) {
	expect(t, (&OtelAcceptedSpans{}).Run(context.Background(), newEnv(t, app, nil, fakeServices{})), gate.Pending)
}

func TestOtelExportFailures(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		second string
		want   gate.Status
		rec    float64
	}{
		{"no failures", otelMetrics(10, 1, 2), gate.Pass, 0},
		{"failures grow", otelMetrics(10, 4, 3), gate.Fail, 4},
		{"metric absent", "up 1\n", gate.Pass, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body atomic.Value
			body.Store(otelMetrics(10, 1, 2))
			env := newEnv(t, app, nil, metricsSvc(&body))
			c := &OtelExportFailures{}
			expect(t, c.Run(ctx, env), gate.Pending)
			advance(env, 30*time.Second)
			body.Store(tc.second)
			r := c.Run(ctx, env)
			expect(t, r, tc.want)
			if got := env.Metrics()["otel.export_failed"]; got != tc.rec {
				t.Errorf("recorded %v, want %v", got, tc.rec)
			}
			if r.Query == "" || r.Value == "" {
				t.Errorf("empty value/query: %+v", r)
			}
		})
	}
}

func TestOtelRegistered(t *testing.T) {
	f, ok := Sets["otel-collector"]
	if !ok || len(f()) != 3 {
		t.Fatal("set not registered with 3 checks")
	}
}
