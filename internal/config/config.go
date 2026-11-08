// Package config loads the fleet's tiers.yaml and apps.yaml.
package config

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

type Tier struct {
	Name             string   `json:"name"`
	Clusters         []string `json:"clusters"`
	RequiresApproval bool     `json:"requiresApproval"`
}

type App struct {
	Name      string   `json:"name"`
	Repo      string   `json:"repo"`
	Namespace string   `json:"namespace"`
	DependsOn []string `json:"dependsOn"`
	// Checks names the app-specific check sets run on top of the common ones.
	Checks []string `json:"checks"`
}

// Gate holds the thresholds for metric-based checks.
type Gate struct {
	MaxWait        Duration `json:"maxWait"`        // per check
	PollInterval   Duration `json:"pollInterval"`   // re-ask a pending check
	MinScrapes     int      `json:"minScrapes"`     // successful canary scrapes since Ready
	SampleInterval Duration `json:"sampleInterval"` // min gap between two counter samples
}

type Fleet struct {
	Owner string `json:"owner"`
	Tiers []Tier `json:"-"`
	Apps  []App  `json:"apps"`
	Gate  Gate   `json:"gate"`
}

// DefaultGate is used for anything apps.yaml leaves unset.
var DefaultGate = Gate{
	MaxWait:        Duration(5 * time.Minute),
	PollInterval:   Duration(10 * time.Second),
	MinScrapes:     3,
	SampleInterval: Duration(30 * time.Second),
}

// Load reads tiers.yaml and apps.yaml from a directory or a base URL
// (e.g. https://raw.githubusercontent.com/<owner>/platform-fleet/main).
func Load(src string) (*Fleet, error) {
	tiersRaw, err := read(src, "tiers.yaml")
	if err != nil {
		return nil, err
	}
	appsRaw, err := read(src, "apps.yaml")
	if err != nil {
		return nil, err
	}
	return Parse(tiersRaw, appsRaw)
}

func Parse(tiersRaw, appsRaw []byte) (*Fleet, error) {
	var t struct {
		Tiers []Tier `json:"tiers"`
	}
	if err := yaml.Unmarshal(tiersRaw, &t); err != nil {
		return nil, fmt.Errorf("tiers.yaml: %w", err)
	}
	var f Fleet
	if err := yaml.Unmarshal(appsRaw, &f); err != nil {
		return nil, fmt.Errorf("apps.yaml: %w", err)
	}
	f.Tiers = t.Tiers
	if len(f.Tiers) == 0 {
		return nil, fmt.Errorf("tiers.yaml: no tiers")
	}
	seen := map[string]bool{}
	for _, tr := range f.Tiers {
		if tr.Name == "" || len(tr.Clusters) == 0 {
			return nil, fmt.Errorf("tiers.yaml: tier %q needs a name and at least one cluster", tr.Name)
		}
		if seen[tr.Name] {
			return nil, fmt.Errorf("tiers.yaml: duplicate tier %q", tr.Name)
		}
		seen[tr.Name] = true
	}
	if f.Owner == "" {
		return nil, fmt.Errorf("apps.yaml: owner is required")
	}
	d := DefaultGate
	if f.Gate.MaxWait == 0 {
		f.Gate.MaxWait = d.MaxWait
	}
	if f.Gate.PollInterval == 0 {
		f.Gate.PollInterval = d.PollInterval
	}
	if f.Gate.MinScrapes == 0 {
		f.Gate.MinScrapes = d.MinScrapes
	}
	if f.Gate.SampleInterval == 0 {
		f.Gate.SampleInterval = d.SampleInterval
	}
	return &f, nil
}

func (f *Fleet) App(name string) (App, error) {
	for _, a := range f.Apps {
		if a.Name == name {
			return a, nil
		}
	}
	return App{}, fmt.Errorf("unknown app %q (apps.yaml has: %s)", name, strings.Join(f.appNames(), ", "))
}

func (f *Fleet) Tier(name string) (Tier, error) {
	for _, t := range f.Tiers {
		if t.Name == name {
			return t, nil
		}
	}
	return Tier{}, fmt.Errorf("unknown tier %q (tiers.yaml has: %s)", name, strings.Join(f.tierNames(), ", "))
}

// Next returns the tier after name, or false if name is the last tier.
func (f *Fleet) Next(name string) (Tier, bool, error) {
	for i, t := range f.Tiers {
		if t.Name == name {
			if i+1 == len(f.Tiers) {
				return Tier{}, false, nil
			}
			return f.Tiers[i+1], true, nil
		}
	}
	return Tier{}, false, fmt.Errorf("unknown tier %q", name)
}

// Previous returns the tier before name, or false if name is the first tier.
func (f *Fleet) Previous(name string) (Tier, bool, error) {
	for i, t := range f.Tiers {
		if t.Name == name {
			if i == 0 {
				return Tier{}, false, nil
			}
			return f.Tiers[i-1], true, nil
		}
	}
	return Tier{}, false, fmt.Errorf("unknown tier %q", name)
}

// IsLast reports whether the tier is the final one (production).
func (f *Fleet) IsLast(name string) bool { return f.Tiers[len(f.Tiers)-1].Name == name }

func (f *Fleet) appNames() []string {
	var s []string
	for _, a := range f.Apps {
		s = append(s, a.Name)
	}
	return s
}

func (f *Fleet) tierNames() []string {
	var s []string
	for _, t := range f.Tiers {
		s = append(s, t.Name)
	}
	return s
}

func read(src, name string) ([]byte, error) {
	if strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "http://") {
		resp, err := http.Get(strings.TrimSuffix(src, "/") + "/" + name)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %s", name, resp.Status)
		}
		return io.ReadAll(resp.Body)
	}
	return os.ReadFile(filepath.Join(src, name))
}

// Duration unmarshals "5m" style strings.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Duration(d).String() + `"`), nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }
