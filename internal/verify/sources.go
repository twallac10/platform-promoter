package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// GitFiles reads PR head content from the working tree and main from a git
// ref in the same checkout (e.g. origin/main).
type GitFiles struct {
	Dir     string
	MainRef string
}

func (g GitFiles) Head(path string) ([]byte, bool, error) {
	b, err := exec.Command("git", "-C", g.Dir, "show", "HEAD:"+path).Output()
	if err != nil {
		return nil, false, nil
	}
	return b, true, nil
}

func (g GitFiles) Main(path string) ([]byte, bool, error) {
	b, err := exec.Command("git", "-C", g.Dir, "show", g.MainRef+":"+path).Output()
	if err != nil {
		return nil, false, nil
	}
	return b, true, nil
}

func (g GitFiles) MainCommit(path string) (string, error) {
	b, err := exec.Command("git", "-C", g.Dir, "log", "-1", "--format=%H", g.MainRef, "--", path).Output()
	if err != nil {
		return "", fmt.Errorf("git log %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// ChangedFiles lists files changed between the merge base of main and HEAD.
func ChangedFiles(dir, mainRef string) ([]string, error) {
	out, err := exec.Command("git", "-C", dir, "diff", "--name-only", mainRef+"...HEAD").Output()
	if err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}
	var files []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			files = append(files, l)
		}
	}
	return files, nil
}

// GHCR checks manifests anonymously (packages are public).
type GHCR struct{ Client *http.Client }

func (g GHCR) ManifestExists(ctx context.Context, ociURL, digest string) (bool, error) {
	c := g.Client
	if c == nil {
		c = &http.Client{Timeout: 20 * time.Second}
	}
	rest, ok := strings.CutPrefix(ociURL, "oci://ghcr.io/")
	if !ok {
		return false, fmt.Errorf("%s is not on ghcr.io", ociURL)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://ghcr.io/token?scope=repository:"+rest+":pull", nil)
	resp, err := c.Do(req)
	if err != nil {
		return false, err
	}
	var tok struct {
		Token string `json:"token"`
	}
	err = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if err != nil || tok.Token == "" {
		return false, fmt.Errorf("ghcr token for %s: %v (is the package public?)", rest, err)
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodHead, "https://ghcr.io/v2/"+rest+"/manifests/"+digest, nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	resp, err = c.Do(req)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("HEAD manifest: %s", resp.Status)
}
