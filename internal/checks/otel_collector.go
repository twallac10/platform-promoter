package checks

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/twallac10/platform-promoter/internal/gate"
)

// Check set "otel-collector".

func init() {
	Register("otel-collector", func() []gate.Check {
		return []gate.Check{
			&OtelReady{},
			&OtelAcceptedSpans{},
			&OtelExportFailures{},
		}
	})
}

const (
	otelNamespace   = "otel"
	otelDeployment  = "otel-collector"
	otelService     = "otel-collector"
	otelMetricsPort = 8888 // collector self metrics
)

// ---- 1. collector ready ----

type OtelReady struct{}

func (*OtelReady) Name() string { return "otel collector ready" }

func (*OtelReady) Run(ctx context.Context, env *gate.Env) gate.Result {
	q := fmt.Sprintf("kubectl -n %s get deployment %s", otelNamespace, otelDeployment)
	d, err := env.Cluster.Kube.AppsV1().Deployments(otelNamespace).Get(ctx, otelDeployment, metav1.GetOptions{})
	if err != nil {
		return pending("deployment: "+err.Error(), q)
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	have := d.Status.AvailableReplicas
	if have < 1 || have != want {
		return pending(fmt.Sprintf("%d/%d replicas available", have, want), q)
	}
	return pass(fmt.Sprintf("%d/%d replicas available", have, want), q)
}

// ---- two-sample counter logic ----

// otelSampler scrapes the collector's self metrics and compares each scrape
// with the previous one, at least SampleInterval apart. Each check keeps its
// own sampler (and so makes its own scrapes; that is cheap).
type otelSampler struct {
	prev map[string]float64
	at   time.Time
}

func otelMetricsURL(ctx context.Context, env *gate.Env) (string, func(), error) {
	base, done, err := env.Cluster.HTTP.BaseURL(ctx, otelNamespace, otelService, otelMetricsPort)
	if err != nil {
		return "", func() {}, fmt.Errorf("reach collector: %w", err)
	}
	return base + "/metrics", done, nil
}

// step returns (prev, cur, elapsed, "") when two samples at least
// SampleInterval apart are available, else a pending reason. After a
// comparison the current scrape becomes the baseline for the next one.
func (s *otelSampler) step(ctx context.Context, env *gate.Env) (prev, cur map[string]float64, dt time.Duration, reason string) {
	if s.prev != nil {
		gap := env.Gate.SampleInterval.D()
		if el := env.Since(s.at); el < gap {
			return nil, nil, 0, fmt.Sprintf("sampling: next sample in %s", (gap - el).Round(time.Second))
		}
	}
	u, done, err := otelMetricsURL(ctx, env)
	if err != nil {
		return nil, nil, 0, err.Error()
	}
	defer done()
	m, err := ScrapeMetrics(ctx, u)
	if err != nil {
		return nil, nil, 0, "scrape collector metrics: " + err.Error()
	}
	at := otelNow(env)
	if s.prev == nil {
		s.prev, s.at = m, at
		return nil, nil, 0, "sampling: first sample taken"
	}
	prev, dt = s.prev, at.Sub(s.at)
	s.prev, s.at = m, at
	return prev, m, dt, ""
}

func otelNow(env *gate.Env) time.Time {
	if env.Now != nil {
		return env.Now()
	}
	return time.Now()
}

const otelMetricsQuery = "GET http://otel-collector.otel:8888/metrics"

// ---- 2. accepted spans increasing ----

// OtelAcceptedSpans: the collector's accepted-spans counter grows. The
// collector self-metrics do not label by client, and the canary is the only
// span producer in the demo, so growth is attributed to the canary.
type OtelAcceptedSpans struct{ s otelSampler }

func (*OtelAcceptedSpans) Name() string { return "otel collector accepting spans" }

const otelAcceptedPrefix = "otelcol_receiver_accepted_spans"

func (c *OtelAcceptedSpans) Run(ctx context.Context, env *gate.Env) gate.Result {
	q := otelMetricsQuery + " | " + otelAcceptedPrefix + "_total (delta)"
	prev, cur, dt, reason := c.s.step(ctx, env)
	if reason != "" {
		return pending(reason, q)
	}
	b, ok := SumPrefix(cur, otelAcceptedPrefix)
	if !ok {
		return pending("no accepted-spans metric", q)
	}
	a, _ := SumPrefix(prev, otelAcceptedPrefix)
	d := b - a // a negative delta (counter reset) counts as no increase
	if d > 0 {
		env.Record("otel.accepted_spans", d)
		return pass(fmt.Sprintf("+%g spans in %s", d, dt.Round(time.Second)), q)
	}
	// Spans may arrive in batches; keep waiting (the runner fails us at timeout).
	return gate.Result{Status: gate.Pending, Query: q, Value: fmt.Sprintf("+0 spans in %s", dt.Round(time.Second)),
		Reason: "no increase in accepted spans yet"}
}

// ---- 3. export failures not increasing ----

type OtelExportFailures struct{ s otelSampler }

func (*OtelExportFailures) Name() string { return "otel collector export failures not increasing" }

const otelFailedPrefix = "otelcol_exporter_send_failed_" // spans, metric_points, log_records

func (c *OtelExportFailures) Run(ctx context.Context, env *gate.Env) gate.Result {
	q := otelMetricsQuery + " | " + otelFailedPrefix + "* (delta)"
	prev, cur, dt, reason := c.s.step(ctx, env)
	if reason != "" {
		return pending(reason, q)
	}
	// Counters that were never incremented may be absent: that means 0 failures.
	b, _ := SumPrefix(cur, otelFailedPrefix)
	a, _ := SumPrefix(prev, otelFailedPrefix)
	d := b - a
	if d < 0 { // collector restarted: counters reset
		d = 0
	}
	env.Record("otel.export_failed", d)
	if d > 0 {
		return fail(fmt.Sprintf("exporter send failures increased by %g", d),
			fmt.Sprintf("+%g failed in %s", d, dt.Round(time.Second)), q)
	}
	return pass(fmt.Sprintf("+0 failed in %s", dt.Round(time.Second)), q)
}
