package checks

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/twallac10/platform-promoter/internal/cluster"
	"github.com/twallac10/platform-promoter/internal/gate"
)

// Check set "kube-prometheus-stack": evidence that the monitoring stack
// itself works, using the canary app (podinfo in ns canary) as the witness
// that discovery -> scrape -> query works end to end.

func init() {
	Register("kube-prometheus-stack", func() []gate.Check {
		return []gate.Check{
			&PromReady{},
			&PromCanaryScraped{},
			&PromTargetsUp{},
			&PromRuleEvalFailures{},
			&PromGrafanaHealth{},
			&PromDashboardQuery{},
		}
	})
}

const (
	promHRNamespace = "monitoring"
	promHRName      = "kube-prometheus-stack"

	promGrafanaService = "grafana" // grafana.fullnameOverride: grafana
	promGrafanaPort    = 80
	promGrafanaSecret  = "grafana"    // Secret with admin-user / admin-password
	promDatasourceUID  = "prometheus" // uid provisioned by kps (rendered chart 93.0.0)
	// "Kubernetes / Compute Resources / Cluster", uid from the rendered chart 93.0.0.
	promDashboardUID = "efa86fd1d0c121a26444b636a3f509a8"

	promCanaryNS  = "canary"
	promCanaryJob = "podinfo"
)

// promWindow is max(ceil(time since the HelmRelease became Ready), 60s) as a
// PromQL range, e.g. "[245s]". ok=false while the HelmRelease is not Ready.
func promWindow(ctx context.Context, env *gate.Env) (win string, reason string, ok bool) {
	hr, err := env.Cluster.Get(ctx, cluster.HelmRelease, promHRNamespace, promHRName)
	if err != nil {
		return "", "helmrelease " + promHRNamespace + "/" + promHRName + ": " + err.Error(), false
	}
	if st, r, _, _ := cluster.Condition(hr, "Ready"); st != "True" {
		return "", fmt.Sprintf("HelmRelease not Ready (%s)", r), false
	}
	t, found := cluster.ConditionTime(hr, "Ready")
	if !found {
		return "", "HelmRelease has no Ready lastTransitionTime", false
	}
	secs := int(math.Ceil(env.Since(t).Seconds()))
	if secs < 60 {
		secs = 60
	}
	return fmt.Sprintf("[%ds]", secs), "", true
}

// ---- 1. Prometheus ready ----

type PromReady struct{}

func (*PromReady) Name() string { return "prometheus ready" }

func (*PromReady) Run(ctx context.Context, env *gate.Env) gate.Result {
	q := fmt.Sprintf("GET http://%s.%s:%d/-/ready", PromService, PromNamespace, PromPort)
	base, done, err := env.Cluster.HTTP.BaseURL(ctx, PromNamespace, PromService, PromPort)
	if err != nil {
		return pending("reach prometheus: "+err.Error(), q)
	}
	defer done()
	code, body, err := promDo(ctx, http.MethodGet, base+"/-/ready", nil, nil)
	if err != nil {
		return pending(err.Error(), q)
	}
	if code != http.StatusOK {
		return pending(fmt.Sprintf("/-/ready returned %d: %s", code, strings.TrimSpace(string(body))), q)
	}
	return pass("/-/ready 200", q)
}

// ---- 2. canary scraped ----

type PromCanaryScraped struct{}

func (*PromCanaryScraped) Name() string { return "canary scraped since Ready" }

func (*PromCanaryScraped) Run(ctx context.Context, env *gate.Env) gate.Result {
	win, reason, ok := promWindow(ctx, env)
	if !ok {
		return pending(reason, "kubectl -n monitoring get helmrelease kube-prometheus-stack")
	}
	sel := fmt.Sprintf(`up{namespace=%q,job=%q}`, promCanaryNS, promCanaryJob)
	q := fmt.Sprintf("sum_over_time(%s%s)", sel, win)
	cur, err := PromQuery(ctx, env, sel)
	if err != nil {
		return pending(err.Error(), q)
	}
	if len(cur) == 0 {
		return pending("canary target not discovered", q)
	}
	scrapes, found, err := PromScalar(ctx, env, q)
	if err != nil {
		return pending(err.Error(), q)
	}
	if !found {
		return pending("canary target not discovered", q)
	}
	min := env.Gate.MinScrapes
	if int(scrapes) < min {
		return pending(fmt.Sprintf("%d/%d scrapes since Ready", int(scrapes), min), q)
	}
	up := cur[0].Value
	val := fmt.Sprintf("%d scrapes, up=%g", int(scrapes), up)
	if up != 1 {
		return pending(fmt.Sprintf("canary target is down (up=%g)", up), q)
	}
	return pass(val, q)
}

// ---- 3. all targets up ----

type PromTargetsUp struct{}

func (*PromTargetsUp) Name() string { return "all scrape targets up" }

func (*PromTargetsUp) Run(ctx context.Context, env *gate.Env) gate.Result {
	const qDown, qAll = `count(up == 0)`, `count(up)`
	down, found, err := PromScalar(ctx, env, qDown)
	if err != nil {
		return pending(err.Error(), qDown)
	}
	if found && down > 0 {
		jobs := map[string]bool{}
		if s, err := PromQuery(ctx, env, `up == 0`); err == nil {
			for _, x := range s {
				jobs[x.Labels["job"]] = true
			}
		}
		var names []string
		for j := range jobs {
			names = append(names, j)
		}
		sort.Strings(names)
		return gate.Result{Status: gate.Pending, Value: fmt.Sprintf("%d targets down", int(down)),
			Reason: fmt.Sprintf("%d targets down: %s", int(down), strings.Join(names, ", ")), Query: qDown}
	}
	total, found, err := PromScalar(ctx, env, qAll)
	if err != nil {
		return pending(err.Error(), qAll)
	}
	if !found || total == 0 {
		return pending("no scrape targets yet", qAll)
	}
	return pass(fmt.Sprintf("%d targets up", int(total)), qDown+" == 0 of "+qAll)
}

// ---- 4. rule evaluation failures ----

type PromRuleEvalFailures struct{}

func (*PromRuleEvalFailures) Name() string { return "no rule evaluation failures" }

func (*PromRuleEvalFailures) Run(ctx context.Context, env *gate.Env) gate.Result {
	win, reason, ok := promWindow(ctx, env)
	if !ok {
		return pending(reason, "kubectl -n monitoring get helmrelease kube-prometheus-stack")
	}
	q := fmt.Sprintf("sum(increase(prometheus_rule_evaluation_failures_total%s))", win)
	v, _, err := PromScalar(ctx, env, q) // empty result = no failures recorded
	if err != nil {
		return pending(err.Error(), q)
	}
	if v > 0 {
		return fail(fmt.Sprintf("%g rule evaluation failures since Ready", v), fmt.Sprintf("%g failures", v), q)
	}
	return pass("0 failures", q)
}

// ---- Grafana helpers ----

type promGrafana struct {
	base string
	auth string // "Basic ..." or ""
}

func (g promGrafana) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	var hdr http.Header
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
		hdr = http.Header{"Content-Type": {"application/json"}}
	}
	if g.auth != "" {
		if hdr == nil {
			hdr = http.Header{}
		}
		hdr.Set("Authorization", g.auth)
	}
	code, b, err := promDo(ctx, method, g.base+path, rd, hdr)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, code, truncStr(strings.TrimSpace(string(b)), 200))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

func promDo(ctx context.Context, method, u string, body io.Reader, hdr http.Header) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := cluster.Client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, b, nil
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// promGrafanaClient returns a client for Grafana. With admin=true it reads
// the admin credentials from Secret monitoring/grafana. That is the one
// Secret the promoter reads; it is only used to call Grafana's own read APIs.
func promGrafanaClient(ctx context.Context, env *gate.Env, admin bool) (promGrafana, func(), error) {
	base, done, err := env.Cluster.HTTP.BaseURL(ctx, PromNamespace, promGrafanaService, promGrafanaPort)
	if err != nil {
		return promGrafana{}, func() {}, fmt.Errorf("reach grafana: %w", err)
	}
	g := promGrafana{base: base}
	if admin {
		sec, err := env.Cluster.Kube.CoreV1().Secrets(PromNamespace).Get(ctx, promGrafanaSecret, metav1.GetOptions{})
		if err != nil {
			done()
			return promGrafana{}, func() {}, fmt.Errorf("secret %s/%s: %w", PromNamespace, promGrafanaSecret, err)
		}
		user, pw := string(sec.Data["admin-user"]), string(sec.Data["admin-password"])
		if user == "" || pw == "" {
			done()
			return promGrafana{}, func() {}, fmt.Errorf("secret %s/%s lacks admin-user/admin-password", PromNamespace, promGrafanaSecret)
		}
		g.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pw))
	}
	return g, done, nil
}

// ---- 5. Grafana health ----

type PromGrafanaHealth struct{}

func (*PromGrafanaHealth) Name() string { return "grafana healthy, prometheus datasource ok" }

func (*PromGrafanaHealth) Run(ctx context.Context, env *gate.Env) gate.Result {
	q := fmt.Sprintf("GET grafana/api/health; GET grafana/api/datasources/uid/%s/health", promDatasourceUID)
	g, done, err := promGrafanaClient(ctx, env, false)
	if err != nil {
		return pending(err.Error(), q)
	}
	defer done()
	var h struct {
		Database string `json:"database"`
	}
	if err := g.do(ctx, http.MethodGet, "/api/health", nil, &h); err != nil {
		return pending(err.Error(), q)
	}
	if h.Database != "ok" {
		return pending(fmt.Sprintf("grafana database=%q", h.Database), q)
	}
	ga, done2, err := promGrafanaClient(ctx, env, true)
	if err != nil {
		return pending(err.Error(), q)
	}
	defer done2()
	var dh struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if err := ga.do(ctx, http.MethodGet, "/api/datasources/uid/"+promDatasourceUID+"/health", nil, &dh); err != nil {
		return pending(err.Error(), q)
	}
	if !strings.EqualFold(dh.Status, "OK") {
		return pending(fmt.Sprintf("datasource %s status=%q: %s", promDatasourceUID, dh.Status, dh.Message), q)
	}
	return pass(fmt.Sprintf("database ok, datasource %s OK", promDatasourceUID), q)
}

// ---- 6. dashboard query returns data ----

type PromDashboardQuery struct{}

func (*PromDashboardQuery) Name() string { return "grafana dashboard query returns data" }

var promTemplateVar = regexp.MustCompile(`\$\{(\w+)(?::\w+)?\}|\$(\w+)`)

// promSubstitute replaces Grafana template variables in a panel expression.
// $datasource -> uid, $cluster -> "" (single-cluster: matches series without
// a cluster label), built-in intervals -> fixed values, anything else -> ".*"
// (the dashboard's own selectors use =~ for those).
func promSubstitute(expr string) string {
	return promTemplateVar.ReplaceAllStringFunc(expr, func(m string) string {
		name := strings.Trim(m, "${}")
		if i := strings.Index(name, ":"); i >= 0 {
			name = name[:i]
		}
		switch name {
		case "datasource":
			return promDatasourceUID
		case "cluster":
			return ""
		case "__rate_interval":
			return "5m"
		case "__interval", "__range":
			if name == "__range" {
				return "5m"
			}
			return "1m"
		case "__interval_ms":
			return "60000"
		default:
			return ".*"
		}
	})
}

// promPanelExprs collects target expressions in dashboard order, descending
// into rows' nested panels.
func promPanelExprs(panels []any, out *[]string) {
	for _, p := range panels {
		m, _ := p.(map[string]any)
		for _, t := range asSlice(m["targets"]) {
			tm, _ := t.(map[string]any)
			if e, _ := tm["expr"].(string); strings.TrimSpace(e) != "" {
				*out = append(*out, e)
			}
		}
		promPanelExprs(asSlice(m["panels"]), out)
	}
}

func asSlice(v any) []any { s, _ := v.([]any); return s }

// promMaxExprs bounds how many panel targets we try. The first targets of a
// dashboard may be built on recording rules this deployment disables (e.g.
// node-exporter rules), so we take the first one that returns data.
const promMaxExprs = 8

func (*PromDashboardQuery) Run(ctx context.Context, env *gate.Env) gate.Result {
	q := fmt.Sprintf("GET grafana/api/dashboards/uid/%s; POST grafana/api/ds/query (datasource uid %s)", promDashboardUID, promDatasourceUID)
	g, done, err := promGrafanaClient(ctx, env, true)
	if err != nil {
		return pending(err.Error(), q)
	}
	defer done()
	var d struct {
		Dashboard struct {
			Title  string `json:"title"`
			Panels []any  `json:"panels"`
		} `json:"dashboard"`
	}
	if err := g.do(ctx, http.MethodGet, "/api/dashboards/uid/"+url.PathEscape(promDashboardUID), nil, &d); err != nil {
		return pending(err.Error(), q)
	}
	var exprs []string
	promPanelExprs(d.Dashboard.Panels, &exprs)
	if len(exprs) == 0 {
		return pending("dashboard has no panel queries yet", q)
	}
	if len(exprs) > promMaxExprs {
		exprs = exprs[:promMaxExprs]
	}
	var lastErr string
	for _, raw := range exprs {
		expr := promSubstitute(raw)
		body := map[string]any{
			"queries": []any{map[string]any{
				"refId": "A", "datasource": map[string]any{"uid": promDatasourceUID},
				"expr": expr, "instant": true,
			}},
			"from": "now-5m", "to": "now",
		}
		var resp struct {
			Results map[string]struct {
				Error  string `json:"error"`
				Frames []struct {
					Data struct {
						Values []([]any) `json:"values"`
					} `json:"data"`
				} `json:"frames"`
			} `json:"results"`
		}
		if err := g.do(ctx, http.MethodPost, "/api/ds/query", body, &resp); err != nil {
			lastErr = err.Error()
			continue
		}
		r := resp.Results["A"]
		if r.Error != "" {
			lastErr = r.Error
			continue
		}
		n := 0
		for _, f := range r.Frames {
			if len(f.Data.Values) > 0 && len(f.Data.Values[0]) > 0 {
				n++
			}
		}
		if n > 0 {
			return pass(fmt.Sprintf("%d series from %q", n, d.Dashboard.Title),
				fmt.Sprintf("POST /api/ds/query expr=%s", expr))
		}
	}
	reason := fmt.Sprintf("first %d queries of dashboard %q returned no data", len(exprs), d.Dashboard.Title)
	if lastErr != "" {
		reason += " (last error: " + truncStr(lastErr, 150) + ")"
	}
	return pending(reason, q)
}
