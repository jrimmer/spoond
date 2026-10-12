package api

// The lease label (#119, #132): the comment a CI job's lease carries —
// "forgejo job <id> <url>", set by the runner before its create answers
// — and the seam it lands at in tests. Production receives the label on
// the usual routes; the helpers here let a test play the label's own
// call at the same point the runner's HTTPLeaseClientWithLabel does.

// WithLabel plays the runner's LeaseLabeler call (#119) in tests: it
// installs the pending label, bound to the lease id of the create whose
// admission is in flight (setAdmittingLeaseID brackets admitMemory), so
// the label lands on that create's commit and never on the next one.
// Each WithLabel overwrites the pending one; calling it with no create
// in flight leaves the label pending for the next create.
func (s *Service) WithLabel(label string) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	s.nextLabelText = label
	s.nextLabel = s.admittingLeaseID
}

// takePendingLabel takes the pending label for a committing lease id:
// the label whose admission matched this create, "" when none did.
// Called under the store lock at the grant's commit; a match consumes
// the label, so the next create starts clean.
func (s *Service) takePendingLabel(id string) string {
	if s.nextLabel != "" && s.nextLabel == id {
		label := s.nextLabelText
		s.nextLabel, s.nextLabelText = "", ""
		return label
	}
	return ""
}

// setAdmittingLeaseID binds the lease id to the admission about to run
// (and clears it afterwards), so a WithLabel call during that admission
// binds to this create. Guarded by the store lock; "" clears.
func (s *Service) setAdmittingLeaseID(id string) {
	s.store.mu.Lock()
	s.admittingLeaseID = id
	s.store.mu.Unlock()
}
