package gate

import (
	"context"
	"strings"
	"testing"
	"time"
)

type scripted struct {
	name  string
	steps []Status
	i     int
}

func (s *scripted) Name() string { return s.name }
func (s *scripted) Run(context.Context, *Env) Result {
	st := s.steps[min(s.i, len(s.steps)-1)]
	s.i++
	return Result{Status: st, Reason: "r"}
}

func fakeClock() (*Env, *time.Time) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := &Env{}
	env.Now = func() time.Time { return now }
	env.After = func(d time.Duration) <-chan time.Time {
		now = now.Add(d)
		c := make(chan time.Time, 1)
		c <- now
		return c
	}
	return env, &now
}

func TestRunPollsPendingUntilAnswer(t *testing.T) {
	env, _ := fakeClock()
	c := &scripted{name: "x", steps: []Status{Pending, Pending, Pass}}
	r := Run(context.Background(), env, []Check{c}, Options{MaxWait: time.Minute, PollInterval: 10 * time.Second})
	if r[0].Status != Pass || c.i != 3 {
		t.Fatalf("got %+v after %d calls", r[0], c.i)
	}
}

func TestRunTimesOutPendingAsFail(t *testing.T) {
	env, _ := fakeClock()
	c := &scripted{name: "x", steps: []Status{Pending}}
	r := Run(context.Background(), env, []Check{c}, Options{MaxWait: 30 * time.Second, PollInterval: 10 * time.Second})
	if r[0].Status != Fail || !strings.Contains(r[0].Reason, "no answer within 30s") {
		t.Fatalf("got %+v", r[0])
	}
}

func TestReport(t *testing.T) {
	rep := &Report{App: "a", Results: []Result{{Check: "one", Status: Pass}, {Check: "two", Status: Skip}}}
	if !rep.Passed() || !strings.Contains(rep.Summary(), "1/1 checks passed") {
		t.Fatal(rep.Summary())
	}
	rep.Results = append(rep.Results, Result{Check: "three", Status: Fail, Reason: "bad | thing"})
	if rep.Passed() || !strings.HasPrefix(rep.Summary(), "1 failed: three") {
		t.Fatal(rep.Summary())
	}
	if !strings.Contains(rep.Markdown(), `bad \| thing`) {
		t.Fatal("markdown must escape pipes")
	}
}
