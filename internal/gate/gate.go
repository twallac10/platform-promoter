// Package gate defines the evidence gate: pluggable checks, a runner that
// polls them until they have enough data to answer (bounded by a timeout,
// never a fixed soak), and the resulting report.
//
// A Check is deliberately small so it can be backed by something other than
// in-process code, e.g. a custom resource whose status the check reads and
// translates into a Result.
package gate

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Status of one check.
type Status string

const (
	Pass    Status = "pass"
	Fail    Status = "fail"
	Pending Status = "pending" // not enough data yet; the runner asks again
	Skip    Status = "skip"    // does not apply (e.g. chart has no tests)
)

// Result is one row of the evidence table.
type Result struct {
	Check  string `json:"check"`
	Status Status `json:"status"`
	// Value is the observed evidence, short and human readable ("3/3 up").
	Value string `json:"value,omitempty"`
	// Query is how to reproduce it: a PromQL expression, a kubectl command, a URL.
	Query string `json:"query,omitempty"`
	// Reason explains a fail, skip or pending in one line.
	Reason string `json:"reason,omitempty"`
	// Elapsed is how long the check took to reach its final status.
	Elapsed time.Duration `json:"elapsed,omitempty"`
}

// Check is one piece of evidence. Run is called repeatedly until it returns
// something other than Pending or the gate times out. A Check instance is used
// for a single gate run, so it may keep state between calls (e.g. a first
// counter sample to compare against later).
type Check interface {
	Name() string
	Run(ctx context.Context, env *Env) Result
}

// Options bound the runner.
type Options struct {
	MaxWait      time.Duration // per check: give up waiting for data after this
	PollInterval time.Duration // how often to re-ask a Pending check
}

// Report is the outcome of a gate run on one cluster.
type Report struct {
	App      string             `json:"app"`
	Tier     string             `json:"tier"`
	Cluster  string             `json:"cluster"`
	Digest   string             `json:"digest"`
	Version  string             `json:"version"`
	Results  []Result           `json:"results"`
	Metrics  map[string]float64 `json:"metrics,omitempty"` // recorded for the next run's baseline
	Started  time.Time          `json:"started"`
	Finished time.Time          `json:"finished"`
}

// Passed is true when no check failed (skips are fine).
func (r *Report) Passed() bool {
	for _, x := range r.Results {
		if x.Status != Pass && x.Status != Skip {
			return false
		}
	}
	return len(r.Results) > 0
}

// Failures returns the failed results.
func (r *Report) Failures() []Result {
	var out []Result
	for _, x := range r.Results {
		if x.Status != Pass && x.Status != Skip {
			out = append(out, x)
		}
	}
	return out
}

// Summary is a one-line description for a Deployment status (GitHub caps it at 140 chars).
func (r *Report) Summary() string {
	pass, skip := 0, 0
	for _, x := range r.Results {
		switch x.Status {
		case Pass:
			pass++
		case Skip:
			skip++
		}
	}
	var s string
	if r.Passed() {
		s = fmt.Sprintf("%d/%d checks passed (%d skipped)", pass, len(r.Results)-skip, skip)
	} else {
		f := r.Failures()
		s = fmt.Sprintf("%d failed: %s: %s", len(f), f[0].Check, f[0].Reason)
	}
	if len(s) > 140 {
		s = s[:137] + "..."
	}
	return s
}

// Run executes every check concurrently, polling Pending ones until they
// answer or MaxWait passes. Results keep the order of checks.
func Run(ctx context.Context, env *Env, checks []Check, opt Options) []Result {
	if opt.PollInterval <= 0 {
		opt.PollInterval = 10 * time.Second
	}
	if opt.MaxWait <= 0 {
		opt.MaxWait = 5 * time.Minute
	}
	results := make([]Result, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func(i int, c Check) {
			defer wg.Done()
			results[i] = runOne(ctx, env, c, opt)
		}(i, c)
	}
	wg.Wait()
	return results
}

func runOne(ctx context.Context, env *Env, c Check, opt Options) Result {
	start := env.now()
	deadline := start.Add(opt.MaxWait)
	for {
		r := safeRun(ctx, env, c)
		r.Check = c.Name()
		if r.Status != Pending {
			r.Elapsed = env.now().Sub(start).Round(time.Second)
			return r
		}
		if !env.now().Before(deadline) {
			r.Status = Fail
			r.Reason = fmt.Sprintf("no answer within %s: %s", opt.MaxWait, r.Reason)
			r.Elapsed = env.now().Sub(start).Round(time.Second)
			return r
		}
		select {
		case <-ctx.Done():
			r.Status = Fail
			r.Reason = "cancelled: " + r.Reason
			return r
		case <-env.after(opt.PollInterval):
		}
	}
}

func safeRun(ctx context.Context, env *Env, c Check) (r Result) {
	defer func() {
		if p := recover(); p != nil {
			r = Result{Status: Fail, Reason: fmt.Sprintf("check panicked: %v", p)}
		}
	}()
	return c.Run(ctx, env)
}
