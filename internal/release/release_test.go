package release

import (
	"os"
	"reflect"
	"testing"
)

func TestParseKyvernoRelease(t *testing.T) {
	raw, err := os.ReadFile("../../../platform-kyverno/tiers/dev/release.yaml")
	if err != nil {
		t.Skip("workspace layout not present:", err)
	}
	r, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != "3.9.2" {
		t.Errorf("version = %q", r.Version)
	}
	if got := r.ImageTags(); !reflect.DeepEqual(got, []string{"v1.19.2"}) {
		t.Errorf("tags = %v", got)
	}
}

func TestParse(t *testing.T) {
	raw := []byte(`apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: a
  annotations:
    platform.twallac10.io/chart-version: "1.2.3"
spec:
  ref:
    digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: a
spec:
  values:
    image: {tag: "v1"}
    sidecars:
      - image: {tag: "v2"}
    other: {tag: ""}
`)
	r, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != "1.2.3" || r.ShortDigest() != "aaaaaaaaaaaa" {
		t.Errorf("got %+v", r)
	}
	if got := r.ImageTags(); !reflect.DeepEqual(got, []string{"v1", "v2"}) {
		t.Errorf("tags = %v", got)
	}
	if _, err := Parse([]byte("kind: OCIRepository\nspec: {ref: {digest: sha256:0}}")); err == nil {
		t.Error("expected bad digest error")
	}
}
