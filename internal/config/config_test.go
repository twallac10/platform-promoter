package config

import (
	"testing"
	"time"
)

func TestLoadFleet(t *testing.T) {
	f, err := Load("../../../platform-fleet")
	if err != nil {
		t.Skip("workspace layout not present:", err)
	}
	if f.Owner != "twallac10" || len(f.Tiers) < 2 {
		t.Fatalf("%+v", f)
	}
	next, ok, _ := f.Next(f.Tiers[0].Name)
	if !ok || next.Name != f.Tiers[1].Name {
		t.Fatal("Next")
	}
	if !f.IsLast(f.Tiers[len(f.Tiers)-1].Name) || f.Gate.MaxWait.D() != 5*time.Minute {
		t.Fatal("IsLast / gate")
	}
	for _, a := range f.Apps {
		if len(a.Checks) == 0 {
			t.Errorf("app %s has no app-specific checks", a.Name)
		}
	}
}

func TestDefaultsAndValidation(t *testing.T) {
	f, err := Parse([]byte("tiers:\n- {name: a, clusters: [c]}\n"), []byte("owner: o\napps: []\n"))
	if err != nil || f.Gate.MinScrapes != 3 || f.Gate.SampleInterval.D() != 30*time.Second {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err := Parse([]byte("tiers:\n- {name: a}\n"), []byte("owner: o\n")); err == nil {
		t.Error("tier without clusters must be rejected")
	}
}
