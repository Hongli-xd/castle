package store

import (
	"testing"

	"sandcastle.dev/sandcastle/internal/domain"
)

// TestUsageRoundTripsAcrossRestartAndCompact proves the metering record is a
// first-class durable entity: it survives a WAL replay and a compaction, which
// is what makes "kill -9 之后谁负责" answerable for billing numbers too.
func TestUsageRoundTripsAcrossRestartAndCompact(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tn := domain.NewTenant("metered")
	if err := s.PutTenant(tn); err != nil {
		t.Fatal(err)
	}
	// Two updates accumulate, exactly as the meter's flush loop does.
	if _, err := s.UpdateUsage(tn.ID, func(u *domain.Usage) error {
		u.SandboxSeconds += 30
		u.CPUMillicoreSeconds += 15000
		u.ExecCount++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateUsage(tn.ID, func(u *domain.Usage) error {
		u.ExecCount++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	u, ok := s2.GetUsage(tn.ID)
	if !ok {
		t.Fatal("usage lost across compact+restart")
	}
	if u.ExecCount != 2 || u.SandboxSeconds != 30 || u.CPUMillicoreSeconds != 15000 {
		t.Fatalf("usage corrupted across restart: %+v", u)
	}
	if u.Version != 2 {
		t.Fatalf("version = %d, want 2 (two updates)", u.Version)
	}
}
