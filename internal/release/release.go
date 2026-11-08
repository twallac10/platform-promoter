// Package release parses an app's tiers/<tier>/release.yaml: the promotable
// unit (chart digest + shared values).
package release

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

const VersionAnnotation = "platform.twallac10.io/chart-version"

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type Release struct {
	Raw     []byte
	Digest  string
	Version string
	Values  map[string]any
}

type doc struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec map[string]any `json:"spec"`
}

func Parse(raw []byte) (*Release, error) {
	r := &Release{Raw: raw}
	for _, part := range splitDocs(raw) {
		var d doc
		if err := yaml.Unmarshal(part, &d); err != nil {
			return nil, err
		}
		switch d.Kind {
		case "OCIRepository":
			ref, _ := d.Spec["ref"].(map[string]any)
			r.Digest, _ = ref["digest"].(string)
			r.Version = d.Metadata.Annotations[VersionAnnotation]
		case "HelmRelease":
			r.Values, _ = d.Spec["values"].(map[string]any)
		case "":
		default:
			return nil, fmt.Errorf("release.yaml: unexpected kind %q", d.Kind)
		}
	}
	if !digestRE.MatchString(r.Digest) {
		return nil, fmt.Errorf("release.yaml: OCIRepository spec.ref.digest %q is not a sha256 digest", r.Digest)
	}
	return r, nil
}

// ShortDigest is the first 12 hex chars, for branch names.
func (r *Release) ShortDigest() string { return strings.TrimPrefix(r.Digest, "sha256:")[:12] }

// ImageTags returns every value under a key named "tag" in the shared values:
// the image tags this release pins. Sorted, unique.
func (r *Release) ImageTags() []string {
	set := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				if s, ok := val.(string); ok && k == "tag" && s != "" {
					set[s] = true
					continue
				}
				walk(val)
			}
		case []any:
			for _, val := range x {
				walk(val)
			}
		}
	}
	walk(r.Values)
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func splitDocs(raw []byte) [][]byte {
	var out [][]byte
	for _, p := range regexp.MustCompile(`(?m)^---\s*$`).Split(string(raw), -1) {
		if len(bytes.TrimSpace([]byte(p))) > 0 {
			out = append(out, []byte(p))
		}
	}
	return out
}
