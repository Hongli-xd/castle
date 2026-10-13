package service

import "sandcastle.dev/sandcastle/internal/runtime"

// The isolation self-check result is held here rather than re-derived per
// request: running it costs a container, so it is done once at startup and
// then served. A stale-but-labelled observation is more useful to an operator
// than no observation, provided the timestamp travels with it — which is why
// IsolationReport carries CheckedAt.

// SetIsolationReport records the outcome of the startup self-check.
func (s *Service) SetIsolationReport(r *runtime.IsolationReport) {
	s.isoMu.Lock()
	defer s.isoMu.Unlock()
	s.isoReport = r
}

// IsolationReport returns the last self-check, or nil if it never ran. A nil
// report must never be rendered as a passing check: "not verified" and
// "verified good" are different states, and conflating them is exactly the
// failure this whole mechanism exists to prevent.
func (s *Service) IsolationReport() *runtime.IsolationReport {
	s.isoMu.Lock()
	defer s.isoMu.Unlock()
	return s.isoReport
}
