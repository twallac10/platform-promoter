package checks

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/twallac10/platform-promoter/internal/cluster"
	"github.com/twallac10/platform-promoter/internal/gate"
)

// Prometheus from kube-prometheus-stack (fullnameOverride: kps).
const (
	PromNamespace = "monitoring"
	PromService   = "kps-prometheus"
	PromPort      = 9090
)

// Sample is one instant-vector element.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// PromQuery runs an instant query against the tier's Prometheus.
func PromQuery(ctx context.Context, env *gate.Env, expr string) ([]Sample, error) {
	base, done, err := env.Cluster.HTTP.BaseURL(ctx, PromNamespace, PromService, PromPort)
	if err != nil {
		return nil, fmt.Errorf("reach prometheus: %w", err)
	}
	defer done()
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := GetJSON(ctx, base+"/api/v1/query?query="+url.QueryEscape(expr), nil, &body); err != nil {
		return nil, err
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s", body.Error)
	}
	var out []Sample
	for _, r := range body.Data.Result {
		s, _ := r.Value[1].(string)
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("prometheus value %q: %w", s, err)
		}
		out = append(out, Sample{Labels: r.Metric, Value: v})
	}
	return out, nil
}

// PromScalar runs a query expected to return at most one sample; ok=false when empty.
func PromScalar(ctx context.Context, env *gate.Env, expr string) (v float64, ok bool, err error) {
	s, err := PromQuery(ctx, env, expr)
	if err != nil || len(s) == 0 {
		return 0, false, err
	}
	return s[0].Value, true, nil
}

// GetJSON GETs url into out. header may be nil.
func GetJSON(ctx context.Context, u string, header http.Header, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := cluster.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: %s: %s", redact(u), resp.Status, strings.TrimSpace(string(b[:min(len(b), 200)])))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// ScrapeMetrics fetches a Prometheus text-format endpoint and returns the sum
// of each metric family by name (labels are summed away), e.g.
// {"otelcol_receiver_accepted_spans_total": 1234}. Enough for counter deltas.
func ScrapeMetrics(ctx context.Context, u string) (map[string]float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := cluster.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return ParseMetrics(resp.Body)
}

var metricLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{.*\})?\s+(\S+)`)

// ParseMetrics sums Prometheus text-format samples by metric name.
func ParseMetrics(r io.Reader) (map[string]float64, error) {
	out := map[string]float64{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		m := metricLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		out[m[1]] += v
	}
	return out, sc.Err()
}

// SumPrefix sums every metric whose name starts with prefix (handles the
// _total suffix and versions that renamed counters).
func SumPrefix(m map[string]float64, prefix string) (float64, bool) {
	var sum float64
	found := false
	for k, v := range m {
		if strings.HasPrefix(k, prefix) {
			sum += v
			found = true
		}
	}
	return sum, found
}

func redact(u string) string {
	if p, err := url.Parse(u); err == nil && p.User != nil {
		p.User = url.User("***")
		return p.String()
	}
	return u
}

// Shorthands for results.
func pass(value, query string) gate.Result {
	return gate.Result{Status: gate.Pass, Value: value, Query: query}
}
func fail(reason, value, query string) gate.Result {
	return gate.Result{Status: gate.Fail, Reason: reason, Value: value, Query: query}
}
func pending(reason, query string) gate.Result {
	return gate.Result{Status: gate.Pending, Reason: reason, Query: query}
}
func skip(reason string) gate.Result { return gate.Result{Status: gate.Skip, Reason: reason} }

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
