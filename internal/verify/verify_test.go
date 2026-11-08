package verify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/twallac10/platform-promoter/internal/config"
	"github.com/twallac10/platform-promoter/internal/ledger"
)

const (
	dA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func rel(d string) []byte {
	return []byte(fmt.Sprintf("kind: OCIRepository\nspec:\n  ref:\n    digest: %q\n---\nkind: HelmRelease\nspec:\n  values: {}\n", d))
}

type files struct{ head, main map[string][]byte }

func (f files) Head(p string) ([]byte, bool, error) { b, ok := f.head[p]; return b, ok, nil }
func (f files) Main(p string) ([]byte, bool, error) { b, ok := f.main[p]; return b, ok, nil }
func (f files) MainCommit(string) (string, error)   { return "c0ffee1", nil }

type deps map[string]*ledger.Deployment // env/cluster

func (d deps) LatestDeployment(_ context.Context, _, env, _, cl string, _ bool) (*ledger.Deployment, error) {
	return d[env+"/"+cl], nil
}

type reg map[string]bool

func (r reg) ManifestExists(_ context.Context, _, d string) (bool, error) { return r[d], nil }

func fleet(t *testing.T) *config.Fleet {
	f, err := config.Parse(
		[]byte("tiers:\n- {name: dev, clusters: [kind-tier-dev]}\n- {name: stg, clusters: [kind-tier-stg]}\n- {name: prd, clusters: [kind-tier-prd], requiresApproval: true}\n"),
		[]byte("owner: twallac10\napps:\n- {name: kyverno, repo: platform-kyverno, namespace: kyverno}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func base(t *testing.T) Input {
	return Input{
		Fleet: fleet(t), Repo: "platform-kyverno",
		Changed: []string{"tiers/stg/release.yaml"}, Labels: []string{"promotion"},
		Files: files{
			head: map[string][]byte{"tiers/stg/release.yaml": rel(dB)},
			main: map[string][]byte{"tiers/dev/release.yaml": rel(dB), "tiers/stg/release.yaml": rel(dA),
				"base/ocirepository.yaml": []byte("spec:\n  url: oci://ghcr.io/twallac10/charts/kyverno\n")},
		},
		Ledger: deps{"dev/kind-tier-dev": {ID: 9, State: "success", Payload: ledger.Payload{Digest: dB, ReleaseCommit: "c0ffee1"}}},
		Reg:    reg{dB: true},
	}
}

func run(t *testing.T, in Input) *Outcome {
	t.Helper()
	o, err := Verify(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func failing(o *Outcome) string {
	var s []string
	for _, r := range o.Rows {
		if !r.OK {
			s = append(s, r.Check+": "+r.Detail)
		}
	}
	return strings.Join(s, "; ")
}

func TestGoodPromotion(t *testing.T) {
	o := run(t, base(t))
	if !o.Passed() || !o.IsPromotion || o.Target != "stg" || o.Source != "dev" || o.RequiresApproval {
		t.Fatalf("%+v %s", o, failing(o))
	}
}

func TestProdNeedsApproval(t *testing.T) {
	in := base(t)
	in.Changed = []string{"tiers/prd/release.yaml"}
	in.Files.(files).head["tiers/prd/release.yaml"] = rel(dB)
	in.Files.(files).main["tiers/stg/release.yaml"] = rel(dB)
	in.Ledger = deps{"stg/kind-tier-stg": {State: "success", Payload: ledger.Payload{Digest: dB, ReleaseCommit: "c0ffee1"}}}
	o := run(t, in)
	if !o.Passed() || !o.RequiresApproval {
		t.Fatalf("%+v %s", o, failing(o))
	}
}

func TestSneakyTierChangeFails(t *testing.T) {
	in := base(t)
	in.Changed = []string{"tiers/stg/release.yaml", "tiers/stg/tier.yaml"}
	o := run(t, in)
	if o.Passed() || !strings.Contains(failing(o), "tiers/stg/tier.yaml") {
		t.Fatal(failing(o))
	}
}

func TestHandMadePromotionWithoutEvidenceFails(t *testing.T) {
	in := base(t)
	in.Labels = nil // not even labelled: touching release.yaml makes it a promotion
	in.Ledger = deps{}
	o := run(t, in)
	if !o.IsPromotion || o.Passed() || !strings.Contains(failing(o), "no gate Deployment recorded in dev") {
		t.Fatal(failing(o))
	}
}

func TestEvidenceForAnotherDigestFails(t *testing.T) {
	in := base(t)
	in.Ledger = deps{"dev/kind-tier-dev": {State: "success", Payload: ledger.Payload{Digest: dA}}}
	if o := run(t, in); o.Passed() {
		t.Fatal("passed with evidence for a different digest")
	}
	in.Ledger = deps{"dev/kind-tier-dev": {State: "failure", Payload: ledger.Payload{Digest: dB, ReleaseCommit: "c0ffee1"}}}
	if o := run(t, in); o.Passed() {
		t.Fatal("passed with a failed source gate")
	}
}

func TestValuesChangedSinceSourceGateFails(t *testing.T) {
	in := base(t)
	in.Ledger = deps{"dev/kind-tier-dev": {State: "success", Payload: ledger.Payload{Digest: dB, ReleaseCommit: "0ld0ld0"}}}
	if o := run(t, in); o.Passed() || !strings.Contains(failing(o), "not gated yet") {
		t.Fatal("passed on evidence for an older release commit with the same digest:", failing(o))
	}
}

func TestNotACopyOfSourceFails(t *testing.T) {
	in := base(t)
	in.Files.(files).head["tiers/stg/release.yaml"] = append(rel(dB), []byte("# extra\n")...)
	if o := run(t, in); o.Passed() {
		t.Fatal("passed although target differs from source")
	}
}

func TestEntryPromotionChecksRegistry(t *testing.T) {
	in := base(t)
	in.Changed = []string{"tiers/dev/release.yaml"}
	in.Files.(files).head["tiers/dev/release.yaml"] = rel(dA)
	in.Reg = reg{dA: true}
	if o := run(t, in); !o.Passed() || o.Source != "" {
		t.Fatalf("%s", failing(o))
	}
	in.Reg = reg{}
	if o := run(t, in); o.Passed() {
		t.Fatal("passed with a digest missing from GHCR")
	}
}

func TestNonPromotionPR(t *testing.T) {
	in := base(t)
	in.Changed = []string{"tiers/prd/tier.yaml"}
	in.Labels = nil
	o := run(t, in)
	if o.IsPromotion || !o.Passed() {
		t.Fatalf("%+v", o)
	}
}

func TestScanSources(t *testing.T) {
	dir := t.TempDir()
	write := func(p, s string) {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644)
	}
	write("base/ocirepository.yaml", "apiVersion: source.toolkit.fluxcd.io/v1\nkind: OCIRepository\nspec:\n  url: oci://ghcr.io/twallac10/charts/kyverno\n")
	write("chart/templates/x.yaml", "{{ if .Values.x }}\nkind: HelmRepository\n{{ end }}\n")
	in := base(t)
	in.HeadDir = dir
	if o := run(t, in); !o.Passed() {
		t.Fatal(failing(o))
	}
	write("tiers/dev/extra.yaml", "a: 1\n---\napiVersion: source.toolkit.fluxcd.io/v1\nkind: HelmRepository\nmetadata: {name: x}\n---\napiVersion: source.toolkit.fluxcd.io/v1\nkind: OCIRepository\nspec: {url: oci://ghcr.io/twallac10-evil/charts/x}\n")
	in.Changed, in.Labels = []string{"tiers/dev/extra.yaml"}, nil
	o := run(t, in)
	if o.Passed() || !strings.Contains(failing(o), "HelmRepository") || !strings.Contains(failing(o), "twallac10-evil") {
		t.Fatal(failing(o))
	}
}
