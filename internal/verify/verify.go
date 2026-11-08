// Package verify is the CI side of the gate: it checks a pull request against
// the promotion rules using only GitHub-visible facts (the diff, main, the
// Deployment ledger, the registry). It never talks to a cluster.
//
// A PR is a promotion if it carries the `promotion` label or touches any
// tiers/<tier>/release.yaml. Promotions must:
//   - change exactly one file, tiers/<target>/release.yaml
//   - for a non-first tier: be a byte-for-byte copy of the previous tier's
//     release.yaml on main, and that tier's latest gate Deployment must be
//     `success` for the same chart digest on every one of its clusters
//   - for the first tier (entry): point at a digest that exists in GHCR
//
// Every PR (promotion or not) must not add a HelmRepository or an
// OCIRepository outside the GHCR prefix anywhere in the repo.
package verify

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/twallac10/platform-promoter/internal/config"
	"github.com/twallac10/platform-promoter/internal/ledger"
	"github.com/twallac10/platform-promoter/internal/release"
)

const PromotionLabel = "promotion"

// Files reads repository content at the PR head and at main.
type Files interface {
	Head(path string) ([]byte, bool, error)
	Main(path string) ([]byte, bool, error)
	// MainCommit is the newest commit on main that touched path.
	MainCommit(path string) (string, error)
}

// Deployments is the slice of the ledger CI needs.
type Deployments interface {
	LatestDeployment(ctx context.Context, repo, environment, app, cluster string, onlySuccess bool) (*ledger.Deployment, error)
}

// Registry answers whether a manifest digest exists, e.g. in GHCR.
type Registry interface {
	ManifestExists(ctx context.Context, ociURL, digest string) (bool, error)
}

type Input struct {
	Fleet   *config.Fleet
	Repo    string   // app repo name (e.g. platform-kyverno)
	Changed []string // files changed by the PR (relative paths)
	Labels  []string
	HeadDir string // checkout of the PR head, scanned for forbidden sources
	Files   Files
	Ledger  Deployments
	Reg     Registry
}

type Row struct {
	Check, Result, Detail string
	OK                    bool
}

type Outcome struct {
	IsPromotion      bool
	Target, Source   string
	RequiresApproval bool
	Digest           string
	Rows             []Row
}

func (o *Outcome) Passed() bool {
	for _, r := range o.Rows {
		if !r.OK {
			return false
		}
	}
	return true
}

func (o *Outcome) add(ok bool, check, detail string) {
	res := "✅ pass"
	if !ok {
		res = "❌ fail"
	}
	o.Rows = append(o.Rows, Row{Check: check, Result: res, Detail: detail, OK: ok})
}

// Markdown renders the outcome for the job summary.
func (o *Outcome) Markdown() string {
	var b strings.Builder
	switch {
	case !o.IsPromotion:
		b.WriteString("Not a promotion PR (no `promotion` label, no release.yaml change).\n\n")
	case o.Target == "":
		b.WriteString("Promotion PR.\n\n")
	case o.Source == "":
		fmt.Fprintf(&b, "Entry promotion into **%s** (`%s`).\n\n", o.Target, o.Digest)
	default:
		fmt.Fprintf(&b, "Promotion **%s → %s** (`%s`).\n\n", o.Source, o.Target, o.Digest)
	}
	b.WriteString("| guardrail | result | detail |\n|---|---|---|\n")
	for _, r := range o.Rows {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", r.Check, r.Result, strings.ReplaceAll(r.Detail, "|", `\|`))
	}
	return b.String()
}

var releaseRE = regexp.MustCompile(`^tiers/([^/]+)/release\.yaml$`)

func Verify(ctx context.Context, in Input) (*Outcome, error) {
	o := &Outcome{}
	appName, err := appFor(in.Fleet, in.Repo)
	if err != nil {
		return nil, err
	}

	// Forbidden sources, for every PR.
	prefix := fmt.Sprintf("oci://ghcr.io/%s/charts/", in.Fleet.Owner)
	if in.HeadDir != "" {
		bad, err := ScanSources(in.HeadDir, prefix)
		if err != nil {
			return nil, err
		}
		o.add(len(bad) == 0, "no HelmRepository / foreign OCIRepository",
			orNone(bad, "only "+prefix+"*"))
	}

	var touched []string
	for _, f := range in.Changed {
		if releaseRE.MatchString(f) {
			touched = append(touched, f)
		}
	}
	o.IsPromotion = contains(in.Labels, PromotionLabel) || len(touched) > 0
	if !o.IsPromotion {
		return o, nil
	}

	// 1. Exactly one file: tiers/<target>/release.yaml.
	if len(in.Changed) != 1 || len(touched) != 1 {
		o.add(false, "only tiers/<target>/release.yaml changed",
			fmt.Sprintf("a promotion must change exactly one release.yaml and nothing else; changed: %s", strings.Join(in.Changed, ", ")))
		return o, nil
	}
	o.Target = releaseRE.FindStringSubmatch(touched[0])[1]
	target, err := in.Fleet.Tier(o.Target)
	if err != nil {
		o.add(false, "target tier exists", err.Error())
		return o, nil
	}
	o.RequiresApproval = target.RequiresApproval
	o.add(true, "only tiers/<target>/release.yaml changed", touched[0])

	head, ok, err := in.Files.Head(touched[0])
	if err != nil {
		return nil, err
	}
	if !ok {
		o.add(false, "release.yaml present", touched[0]+" deleted")
		return o, nil
	}
	rel, err := release.Parse(head)
	if err != nil {
		o.add(false, "release.yaml valid", err.Error())
		return o, nil
	}
	o.Digest = rel.Digest

	prev, hasPrev, _ := in.Fleet.Previous(o.Target)
	if !hasPrev {
		// Entry into the chain: the digest must exist in our registry.
		url, err := ociURL(in.Files)
		if err != nil {
			o.add(false, "chart digest exists in GHCR", err.Error())
			return o, nil
		}
		exists, err := in.Reg.ManifestExists(ctx, url, rel.Digest)
		if err != nil {
			o.add(false, "chart digest exists in GHCR", err.Error())
		} else {
			o.add(exists, "chart digest exists in GHCR", fmt.Sprintf("%s@%s", url, rel.Digest))
		}
		return o, nil
	}
	o.Source = prev.Name

	// 2. Verbatim copy of the source tier's release.yaml on main.
	srcPath := "tiers/" + prev.Name + "/release.yaml"
	src, ok, err := in.Files.Main(srcPath)
	if err != nil {
		return nil, err
	}
	o.add(ok && bytes.Equal(src, head), "target = source release.yaml on main",
		map[bool]string{true: "identical to main:" + srcPath, false: "differs from main:" + srcPath}[ok && bytes.Equal(src, head)])

	// 3. The source tier proved exactly this release (digest + values, i.e.
	// the commit that last changed its release.yaml) on every cluster.
	srcCommit, err := in.Files.MainCommit(srcPath)
	if err != nil {
		return nil, err
	}
	for _, c := range prev.Clusters {
		d, err := in.Ledger.LatestDeployment(ctx, in.Repo, prev.Name, appName, c, false)
		check := fmt.Sprintf("%s gate passed on %s", prev.Name, c)
		switch {
		case err != nil:
			o.add(false, check, err.Error())
		case d == nil:
			o.add(false, check, "no gate Deployment recorded in "+prev.Name)
		case d.Payload.Digest != rel.Digest:
			o.add(false, check, fmt.Sprintf("latest Deployment is for %s, not %s", d.Payload.Digest, rel.Digest))
		case d.Payload.ReleaseCommit != srcCommit:
			o.add(false, check, fmt.Sprintf("latest Deployment gated release commit %.7s, but %s was last changed in %.7s (not gated yet)", d.Payload.ReleaseCommit, srcPath, srcCommit))
		case d.State != "success":
			o.add(false, check, fmt.Sprintf("latest Deployment is %s (%s)", d.State, d.LogURL))
		default:
			o.add(true, check, fmt.Sprintf("Deployment %d success for this digest %s", d.ID, d.LogURL))
		}
	}
	return o, nil
}

func appFor(f *config.Fleet, repo string) (string, error) {
	for _, a := range f.Apps {
		if a.Repo == repo {
			return a.Name, nil
		}
	}
	return "", fmt.Errorf("repo %s is not in apps.yaml", repo)
}

// ociURL reads the chart URL from base/ocirepository.yaml on main.
func ociURL(f Files) (string, error) {
	raw, ok, err := f.Main("base/ocirepository.yaml")
	if err != nil || !ok {
		return "", fmt.Errorf("read base/ocirepository.yaml: %v", err)
	}
	var d struct {
		Spec struct {
			URL string `json:"url"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &d); err != nil || d.Spec.URL == "" {
		return "", fmt.Errorf("base/ocirepository.yaml has no spec.url")
	}
	return d.Spec.URL, nil
}

// ScanSources walks dir for YAML declaring a HelmRepository or an
// OCIRepository outside prefix. Files that aren't plain YAML (Helm templates)
// are skipped.
func ScanSources(dir, prefix string) ([]string, error) {
	var bad []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		for _, doc := range regexp.MustCompile(`(?m)^---\s*$`).Split(string(raw), -1) {
			var o struct {
				APIVersion string `json:"apiVersion"`
				Kind       string `json:"kind"`
				Spec       struct {
					URL string `json:"url"`
				} `json:"spec"`
			}
			if yaml.Unmarshal([]byte(doc), &o) != nil || !strings.HasPrefix(o.APIVersion, "source.toolkit.fluxcd.io/") {
				continue
			}
			switch {
			case o.Kind == "HelmRepository":
				bad = append(bad, rel+": HelmRepository")
			case o.Kind == "OCIRepository" && o.Spec.URL != "" && !strings.HasPrefix(o.Spec.URL, prefix):
				bad = append(bad, fmt.Sprintf("%s: OCIRepository %s", rel, o.Spec.URL))
			}
		}
		return nil
	})
	sort.Strings(bad)
	return bad, err
}

func orNone(bad []string, ok string) string {
	if len(bad) == 0 {
		return ok
	}
	return strings.Join(bad, "; ")
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if strings.EqualFold(strings.TrimSpace(x), v) {
			return true
		}
	}
	return false
}
