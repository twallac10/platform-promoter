package ledger

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-github/v76/github"
)

type call struct {
	Method, Path string
	Body         map[string]any
}

// fakeGitHub records calls and answers the handful of endpoints the ledger uses.
func fakeGitHub(t *testing.T, routes map[string]func(w http.ResponseWriter, body map[string]any)) (*GitHub, *[]call) {
	t.Helper()
	var mu sync.Mutex
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		calls = append(calls, call{r.Method, r.URL.Path, body})
		mu.Unlock()
		key := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/api/v3")
		if h, ok := routes[key]; ok {
			w.Header().Set("Content-Type", "application/json")
			h(w, body)
			return
		}
		t.Logf("unrouted %s", key)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	t.Cleanup(srv.Close)
	c := github.NewClient(nil)
	u, _ := url.Parse(srv.URL + "/api/v3/")
	c.BaseURL = u
	return &GitHub{Owner: "o", Client: c}, &calls
}

func reply(s string) func(http.ResponseWriter, map[string]any) {
	return func(w http.ResponseWriter, _ map[string]any) { w.Write([]byte(s)) }
}

func TestStartDeployment(t *testing.T) {
	g, calls := fakeGitHub(t, map[string]func(http.ResponseWriter, map[string]any){
		"POST /repos/o/r/deployments":            reply(`{"id": 7}`),
		"POST /repos/o/r/deployments/7/statuses": reply(`{"id": 1, "state": "in_progress"}`),
	})
	id, err := g.StartDeployment(context.Background(), "r", "abc123", false, Payload{App: "kyverno", Tier: "dev", Cluster: "kind-tier-dev", Digest: "sha256:x"})
	if err != nil || id != 7 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	d := (*calls)[0].Body
	if d["ref"] != "abc123" || d["environment"] != "dev" || d["task"] != "gate" {
		t.Errorf("deployment request %v", d)
	}
	if rc, ok := d["required_contexts"].([]any); !ok || len(rc) != 0 {
		t.Errorf("required_contexts must be an explicit empty list, got %v", d["required_contexts"])
	}
	if p := d["payload"].(map[string]any); p["digest"] != "sha256:x" || p["cluster"] != "kind-tier-dev" {
		t.Errorf("payload %v", p)
	}
	if s := (*calls)[1].Body; s["state"] != "in_progress" {
		t.Errorf("status %v", s)
	}
}

func TestLatestDeploymentFiltersByAppAndCluster(t *testing.T) {
	g, _ := fakeGitHub(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /repos/o/r/deployments": reply(`[
		  {"id": 3, "sha": "s3", "payload": {"app":"kyverno","tier":"dev","cluster":"other","digest":"d3"}},
		  {"id": 2, "sha": "s2", "payload": {"app":"kyverno","tier":"dev","cluster":"kind-tier-dev","digest":"d2"}},
		  {"id": 1, "sha": "s1", "payload": {"app":"kyverno","tier":"dev","cluster":"kind-tier-dev","digest":"d1"}}]`),
		"GET /repos/o/r/deployments/2/statuses": reply(`[{"state":"failure"}]`),
		"GET /repos/o/r/deployments/1/statuses": reply(`[{"state":"success","log_url":"https://github.com/o/r/commit/s1#commitcomment-9"}]`),
	})
	d, err := g.LatestDeployment(context.Background(), "r", "dev", "kyverno", "kind-tier-dev", false)
	if err != nil || d.ID != 2 || d.State != "failure" {
		t.Fatalf("latest any: %+v %v", d, err)
	}
	d, err = g.LatestDeployment(context.Background(), "r", "dev", "kyverno", "kind-tier-dev", true)
	if err != nil || d.ID != 1 || d.Payload.Digest != "d1" || !strings.HasSuffix(d.LogURL, "-9") {
		t.Fatalf("latest success: %+v %v", d, err)
	}
}

func TestOpenPromotionPRCreatesBranchFileAndLabel(t *testing.T) {
	g, calls := fakeGitHub(t, map[string]func(http.ResponseWriter, map[string]any){
		"GET /repos/o/r/git/ref/heads/main":              reply(`{"ref":"refs/heads/main","object":{"sha":"mainsha"}}`),
		"POST /repos/o/r/git/refs":                       reply(`{"ref":"refs/heads/promote/stg/abc"}`),
		"GET /repos/o/r/contents/tiers/stg/release.yaml": reply(`{"type":"file","sha":"filesha","encoding":"base64","content":"` + base64.StdEncoding.EncodeToString([]byte("old")) + `"}`),
		"PUT /repos/o/r/contents/tiers/stg/release.yaml": reply(`{"content":{"sha":"new"}}`),
		"GET /repos/o/r/pulls":                           reply(`[]`),
		"POST /repos/o/r/pulls":                          reply(`{"number": 5, "node_id": "PR_5", "html_url": "https://github.com/o/r/pull/5"}`),
		"POST /graphql":                                  reply(`{"data": {}}`),
		"POST /repos/o/r/issues/5/labels":                reply(`[{"name":"promotion"}]`),
	})
	u, err := g.OpenPromotionPR(context.Background(), PR{
		Repo: "r", Branch: "promote/stg/abc", Path: "tiers/stg/release.yaml", Content: []byte("new"),
		Title: "t", Body: "b", CommitMessage: "m", Labels: []string{"promotion"}, AutoMerge: true,
	})
	if err != nil || u != "https://github.com/o/r/pull/5" {
		t.Fatalf("u=%q err=%v", u, err)
	}
	var put, ref map[string]any
	for _, c := range *calls {
		switch {
		case c.Method == "PUT":
			put = c.Body
		case c.Method == "POST" && strings.HasSuffix(c.Path, "/git/refs"):
			ref = c.Body
		}
	}
	var gql map[string]any
	for _, c := range *calls {
		if strings.HasSuffix(c.Path, "/graphql") {
			gql = c.Body
		}
	}
	if gql == nil || !strings.Contains(gql["query"].(string), "enablePullRequestAutoMerge") || gql["variables"].(map[string]any)["id"] != "PR_5" {
		t.Errorf("auto-merge not enabled: %v", gql)
	}
	if ref["sha"] != "mainsha" || ref["ref"] != "refs/heads/promote/stg/abc" {
		t.Errorf("branch not created from main: %v", ref)
	}
	if put["branch"] != "promote/stg/abc" || put["sha"] != "filesha" || put["content"] != base64.StdEncoding.EncodeToString([]byte("new")) {
		t.Errorf("file commit %v", put)
	}
}

func TestEvidenceRoundTrip(t *testing.T) {
	body := EmbedEvidence("| table |", Evidence{Metrics: map[string]float64{"kyverno.canary_policy_failures": 2}})
	ev, err := ExtractEvidence(body)
	if err != nil || ev.Metrics["kyverno.canary_policy_failures"] != 2 {
		t.Fatalf("%+v %v", ev, err)
	}
}
