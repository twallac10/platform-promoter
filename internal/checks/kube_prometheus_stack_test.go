package checks

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/twallac10/platform-promoter/internal/gate"
)

// kpsHR is the kube-prometheus-stack HelmRelease; Ready since 11:50:00, the
// fake clock is 12:00:00, so the window is 600s.
func kpsHR(ready string) runtime.Object {
	return u(map[string]any{
		"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": "HelmRelease",
		"metadata": map[string]any{"name": "kube-prometheus-stack", "namespace": "monitoring"},
		"status":   map[string]any{"conditions": readyCond(ready)},
	})
}

const kpsWin = "[600s]"

func promSvc(ready int, answers map[string][]Sample) fakeServices {
	mux := http.NewServeMux()
	mux.HandleFunc("/-/ready", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(ready)
		fmt.Fprint(w, "Prometheus Server is Ready.")
	})
	mux.Handle("/", promHandler(answers))
	return fakeServices{"monitoring/kps-prometheus:9090": mux}
}

func TestPromReady(t *testing.T) {
	ctx := context.Background()
	expect(t, (&PromReady{}).Run(ctx, newEnv(t, app, nil, promSvc(200, nil))), gate.Pass)
	r := (&PromReady{}).Run(ctx, newEnv(t, app, nil, promSvc(503, nil)))
	expect(t, r, gate.Pending)
	if r.Query == "" {
		t.Error("missing query")
	}
	expect(t, (&PromReady{}).Run(ctx, newEnv(t, app, nil, fakeServices{})), gate.Pending)
}

func TestPromCanaryScraped(t *testing.T) {
	sel := `up{namespace="canary",job="podinfo"}`
	sum := fmt.Sprintf("sum_over_time(%s%s)", sel, kpsWin)
	lbl := map[string]string{"job": "podinfo"}
	tests := []struct {
		name    string
		hr      runtime.Object
		answers map[string][]Sample
		want    gate.Status
		in      string
	}{
		{"pass", kpsHR("True"), map[string][]Sample{sel: {{lbl, 1}}, sum: {{lbl, 5}}}, gate.Pass, "5 scrapes, up=1"},
		{"too few scrapes", kpsHR("True"), map[string][]Sample{sel: {{lbl, 1}}, sum: {{lbl, 2}}}, gate.Pending, "2/3 scrapes since Ready"},
		{"not discovered", kpsHR("True"), nil, gate.Pending, "canary target not discovered"},
		{"down", kpsHR("True"), map[string][]Sample{sel: {{lbl, 0}}, sum: {{lbl, 5}}}, gate.Pending, "down"},
		{"hr not ready", kpsHR("False"), nil, gate.Pending, "not Ready"},
		{"no hr", nil, nil, gate.Pending, "helmrelease"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var objs []runtime.Object
			if tc.hr != nil {
				objs = append(objs, tc.hr)
			}
			r := (&PromCanaryScraped{}).Run(context.Background(), newEnv(t, app, objs, promSvc(200, tc.answers)))
			expect(t, r, tc.want)
			if !strings.Contains(r.Value+r.Reason, tc.in) {
				t.Errorf("want %q in value/reason, got value=%q reason=%q", tc.in, r.Value, r.Reason)
			}
			if r.Status == gate.Pass && r.Query != sum {
				t.Errorf("query = %q", r.Query)
			}
		})
	}
}

func TestPromWindowMinimum(t *testing.T) {
	env := newEnv(t, app, []runtime.Object{kpsHR("True")}, nil, func(e *gate.Env) {
		e.Now = func() time.Time { return time.Date(2026, 11, 8, 11, 50, 10, 500, time.UTC) }
	})
	win, _, ok := promWindow(context.Background(), env)
	if !ok || win != "[60s]" {
		t.Fatalf("win = %q ok=%v", win, ok)
	}
	env.Now = func() time.Time { return time.Date(2026, 11, 8, 12, 4, 5, 1, time.UTC) }
	if win, _, _ := promWindow(context.Background(), env); win != "[846s]" {
		t.Fatalf("win = %q", win)
	}
}

func TestPromTargetsUp(t *testing.T) {
	ctx := context.Background()
	r := (&PromTargetsUp{}).Run(ctx, newEnv(t, app, nil, promSvc(200, map[string][]Sample{"count(up)": {{nil, 12}}})))
	expect(t, r, gate.Pass)
	if r.Value != "12 targets up" {
		t.Errorf("value = %q", r.Value)
	}
	r = (&PromTargetsUp{}).Run(ctx, newEnv(t, app, nil, promSvc(200, map[string][]Sample{
		"count(up == 0)": {{nil, 2}},
		"up == 0":        {{map[string]string{"job": "kubelet"}, 0}, {map[string]string{"job": "coredns"}, 0}},
	})))
	expect(t, r, gate.Pending)
	if !strings.Contains(r.Reason, "coredns, kubelet") {
		t.Errorf("reason = %q", r.Reason)
	}
	expect(t, (&PromTargetsUp{}).Run(ctx, newEnv(t, app, nil, promSvc(200, nil))), gate.Pending)
}

func TestPromRuleEvalFailures(t *testing.T) {
	q := "sum(increase(prometheus_rule_evaluation_failures_total" + kpsWin + "))"
	objs := []runtime.Object{kpsHR("True")}
	ctx := context.Background()
	expect(t, (&PromRuleEvalFailures{}).Run(ctx, newEnv(t, app, objs, promSvc(200, nil))), gate.Pass)
	expect(t, (&PromRuleEvalFailures{}).Run(ctx, newEnv(t, app, objs, promSvc(200, map[string][]Sample{q: {{nil, 0}}}))), gate.Pass)
	r := (&PromRuleEvalFailures{}).Run(ctx, newEnv(t, app, objs, promSvc(200, map[string][]Sample{q: {{nil, 3}}})))
	expect(t, r, gate.Fail)
	if r.Query != q {
		t.Errorf("query = %q", r.Query)
	}
	expect(t, (&PromRuleEvalFailures{}).Run(ctx, newEnv(t, app, nil, promSvc(200, nil))), gate.Pending)
}

func grafanaSecret() runtime.Object {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "grafana", Namespace: "monitoring"},
		Data:       map[string][]byte{"admin-user": []byte("admin"), "admin-password": []byte("pw")},
	}
}

// grafanaHandler fakes the Grafana APIs used by the checks and enforces basic
// auth on everything but /api/health.
func grafanaHandler(db, dsStatus string, queryBody string) http.Handler {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if u, p, ok := r.BasicAuth(); !ok || u != "admin" || p != "pw" {
				http.Error(w, "unauthorized", 401)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"database":%q,"version":"13.3.0"}`, db)
	})
	mux.HandleFunc("/api/datasources/uid/prometheus/health", auth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"status":%q,"message":"x"}`, dsStatus)
	}))
	mux.HandleFunc("/api/dashboards/uid/"+promDashboardUID, auth(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"dashboard":{"title":"Compute","panels":[
		 {"type":"row","panels":[{"targets":[{"expr":"a{cluster=\"$cluster\"}"}]}]},
		 {"targets":[{"expr":"sum(rate(x{cluster=\"$cluster\",job=~\"$job\"}[$__rate_interval])) by (${ns})"}]}]}}`)
	}))
	mux.HandleFunc("/api/ds/query", auth(func(w http.ResponseWriter, r *http.Request) {
		b := new(strings.Builder)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		body := b.String()
		if !strings.Contains(body, `"uid":"prometheus"`) || strings.Contains(body, "$") {
			http.Error(w, "bad query "+body, 400)
			return
		}
		// Only the second expression (after variable substitution) has data.
		if strings.Contains(body, `sum(rate(x{cluster=\"\",job=~\".*\"}[5m])) by (.*)`) {
			fmt.Fprint(w, queryBody)
			return
		}
		fmt.Fprint(w, `{"results":{"A":{"status":200,"frames":[{"data":{"values":[[],[]]}}]}}}`)
	}))
	return mux
}

const dsData = `{"results":{"A":{"status":200,"frames":[{"data":{"values":[[1],[0.5]]}}]}}}`

func TestPromGrafanaHealth(t *testing.T) {
	ctx := context.Background()
	svc := func(db, ds string) fakeServices {
		return fakeServices{"monitoring/grafana:80": grafanaHandler(db, ds, dsData)}
	}
	objs := []runtime.Object{grafanaSecret()}
	expect(t, (&PromGrafanaHealth{}).Run(ctx, newEnv(t, app, objs, svc("ok", "OK"))), gate.Pass)
	expect(t, (&PromGrafanaHealth{}).Run(ctx, newEnv(t, app, objs, svc("failing", "OK"))), gate.Pending)
	r := (&PromGrafanaHealth{}).Run(ctx, newEnv(t, app, objs, svc("ok", "ERROR")))
	expect(t, r, gate.Pending)
	if !strings.Contains(r.Reason, "ERROR") {
		t.Errorf("reason = %q", r.Reason)
	}
	// Secret missing.
	expect(t, (&PromGrafanaHealth{}).Run(ctx, newEnv(t, app, nil, svc("ok", "OK"))), gate.Pending)
	// Grafana unreachable.
	expect(t, (&PromGrafanaHealth{}).Run(ctx, newEnv(t, app, objs, fakeServices{})), gate.Pending)
}

func TestPromDashboardQuery(t *testing.T) {
	ctx := context.Background()
	objs := []runtime.Object{grafanaSecret()}
	env := newEnv(t, app, objs, fakeServices{"monitoring/grafana:80": grafanaHandler("ok", "OK", dsData)})
	r := (&PromDashboardQuery{}).Run(ctx, env)
	expect(t, r, gate.Pass)
	if !strings.Contains(r.Value, "Compute") || !strings.Contains(r.Query, "sum(rate(x{cluster=") {
		t.Errorf("value=%q query=%q", r.Value, r.Query)
	}
	// No data from any panel.
	empty := `{"results":{"A":{"status":200,"frames":[]}}}`
	env = newEnv(t, app, objs, fakeServices{"monitoring/grafana:80": grafanaHandler("ok", "OK", empty)})
	expect(t, (&PromDashboardQuery{}).Run(ctx, env), gate.Pending)
	// Datasource error reported inside the response.
	errBody := `{"results":{"A":{"error":"boom"}}}`
	env = newEnv(t, app, objs, fakeServices{"monitoring/grafana:80": grafanaHandler("ok", "OK", errBody)})
	r = (&PromDashboardQuery{}).Run(ctx, env)
	expect(t, r, gate.Pending)
	if !strings.Contains(r.Reason, "boom") {
		t.Errorf("reason = %q", r.Reason)
	}
	// No credentials.
	expect(t, (&PromDashboardQuery{}).Run(ctx, newEnv(t, app, nil, fakeServices{"monitoring/grafana:80": grafanaHandler("ok", "OK", dsData)})), gate.Pending)
}

func TestPromSubstitute(t *testing.T) {
	got := promSubstitute(`m{cluster="$cluster",job=~"$job",ds="${datasource}"}[$__rate_interval] $__interval`)
	want := `m{cluster="",job=~".*",ds="prometheus"}[5m] 1m`
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestKPSRegistered(t *testing.T) {
	f, ok := Sets["kube-prometheus-stack"]
	if !ok || len(f()) != 6 {
		t.Fatalf("set not registered with 6 checks")
	}
}
