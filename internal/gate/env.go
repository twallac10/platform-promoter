package gate

import (
	"context"
	"sync"
	"time"

	"github.com/twallac10/platform-promoter/internal/cluster"
	"github.com/twallac10/platform-promoter/internal/config"
	"github.com/twallac10/platform-promoter/internal/release"
)

// Repo answers questions about the app's Git history (backed by GitHub).
type Repo interface {
	// IsAncestorOrEqual reports whether commit a is b or an ancestor of b.
	IsAncestorOrEqual(ctx context.Context, a, b string) (bool, error)
}

// Env is everything a check may look at. Checks only ever read their own
// cluster: no credentials to any other cluster exist.
type Env struct {
	App     config.App
	Tier    config.Tier
	Gate    config.Gate // thresholds
	Cluster *cluster.Cluster

	// Release is what main says this tier should run (tiers/<tier>/release.yaml).
	Release *release.Release
	// ReleaseCommit is the app-repo commit that last changed that file.
	ReleaseCommit string
	Repo          Repo

	// Baseline holds metrics recorded by this tier's last successful gate
	// (from the GitHub Deployment payload). Empty on the first run.
	Baseline map[string]float64

	mu      sync.Mutex
	metrics map[string]float64

	// Clock, replaceable in tests.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
}

// Record stores a metric so the next gate run can use it as a baseline.
func (e *Env) Record(key string, v float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.metrics == nil {
		e.metrics = map[string]float64{}
	}
	e.metrics[key] = v
}

// Metrics returns the recorded metrics.
func (e *Env) Metrics() map[string]float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]float64, len(e.metrics))
	for k, v := range e.metrics {
		out[k] = v
	}
	return out
}

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Env) after(d time.Duration) <-chan time.Time {
	if e.After != nil {
		return e.After(d)
	}
	return time.After(d)
}

// Since returns the time elapsed since t using the env clock.
func (e *Env) Since(t time.Time) time.Duration { return e.now().Sub(t) }
