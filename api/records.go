package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/jrimmer/spoond/forkd"
)

// Record is a bracketed checkpoint pair of a sandbox run (issue #55).
// A run is bracketed by two branch snapshots — "before" (start) and
// "after" (stop) — and the after state can be re-attached via replay.
// Checkpoints are forkd branch snapshots (see forkd.Client.Branch); the
// diff/live snapshot modes are a forkd-level follow-up, so each
// checkpoint is a full snapshot today.
//
// Records are in-memory bookkeeping (lost on backend restart) that point
// at durable forkd snapshots; snapshot retention is managed by forkd
// (see docs/records.md).
type Record struct {
	ID        string `json:"id"`
	Owner     string `json:"owner"`
	Label     string `json:"label,omitempty"`
	SandboxID string `json:"sandbox_id"`
	BeforeTag string `json:"before_tag,omitempty"`
	AfterTag  string `json:"after_tag,omitempty"`
	CreatedAt string `json:"created_at"` // RFC3339 (UTC)
	UpdatedAt string `json:"updated_at"` // RFC3339 (UTC)

	// stopping marks an in-flight stopRecord so a concurrent stop cannot
	// double-branch or double-decrement the active gauge (not serialized).
	stopping bool
}

// maxRecordsPerOwner bounds the number of live records per owner so
// checkpoints cannot accumulate unboundedly (each checkpoint is a full
// forkd snapshot).
const maxRecordsPerOwner = 50

// maxRecordLabelRunes caps a record label, matching the annotation
// endpoints' reject-oversize behavior.
const maxRecordLabelRunes = 128

var (
	errRecordNotFound         = errors.New("record not found")
	errRecordStopped          = errors.New("record already stopped")
	errRecordSuspended        = errors.New("sandbox is suspended; resume it first")
	errRecordSandboxGone      = errors.New("sandbox not found")
	errRecordQuota            = errors.New("record quota exceeded")
	errRecordAlreadyRecording = errors.New("sandbox already has an open record; stop it first")
	errRecordLabelTooLong     = fmt.Errorf("label must be <= %d characters", maxRecordLabelRunes)
)

// clone returns a copy so a caller can serialize it without racing a
// concurrent stop (which mutates fields under the store lock). The
// Record fields are all immutable value types, so a shallow copy is safe.
func (r *Record) clone() *Record {
	cp := *r
	return &cp
}

// startRecord checkpoints a sandbox's current state as the "before" side
// of a run record.
func (s *Service) startRecord(ctx context.Context, owner, sandboxID, label string) (*Record, error) {
	lease := s.lookup(owner, sandboxID)
	if lease == nil {
		return nil, errRecordSandboxGone
	}
	if lease.Suspended {
		return nil, errRecordSuspended
	}
	if len([]rune(label)) > maxRecordLabelRunes {
		return nil, errRecordLabelTooLong
	}
	if s.recordCount(owner) >= maxRecordsPerOwner {
		return nil, errRecordQuota
	}
	// Reject a second open record for the same sandbox (best-effort; the
	// unique record-id tag prevents aliasing if two starts truly race).
	if s.hasOpenRecord(owner, sandboxID) {
		return nil, errRecordAlreadyRecording
	}
	// Generate the record id before branching so the checkpoint tag can
	// be derived from it (unique even for same-sandbox same-second runs).
	now := time.Now().UTC().Format(time.RFC3339)
	rec := &Record{
		ID:        newID(),
		Owner:     owner,
		Label:     label,
		SandboxID: sandboxID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	newTag, err := s.forkd.Branch(ctx, lease.ForkdID, recordTag(rec.ID, "before"))
	if err != nil {
		return nil, fmt.Errorf("branch before checkpoint: %w", err)
	}
	rec.BeforeTag = newTag
	s.store.mu.Lock()
	s.store.records[rec.ID] = rec
	s.store.mu.Unlock()
	if s.metrics != nil {
		s.metrics.RecordCreated.Inc()
		s.metrics.RecordsActive.Inc()
	}
	return rec.clone(), nil
}

// stopRecord checkpoints the record's sandbox post-run state as the
// "after" side and closes the record.
func (s *Service) stopRecord(ctx context.Context, owner, recordID string) (*Record, error) {
	// Atomically claim the stop under the store lock before the slow
	// branch, so a concurrent stop cannot double-branch or double-
	// decrement the active gauge.
	s.store.mu.Lock()
	rec := s.store.records[recordID]
	if rec == nil || rec.Owner != owner {
		s.store.mu.Unlock()
		return nil, errRecordNotFound
	}
	if rec.AfterTag != "" || rec.stopping {
		s.store.mu.Unlock()
		return nil, errRecordStopped
	}
	rec.stopping = true
	sandboxID := rec.SandboxID
	s.store.mu.Unlock()

	lease := s.lookup(owner, sandboxID)
	if lease == nil {
		s.store.mu.Lock()
		rec.stopping = false
		s.store.mu.Unlock()
		return nil, errRecordSandboxGone
	}
	if lease.Suspended {
		s.store.mu.Lock()
		rec.stopping = false
		s.store.mu.Unlock()
		return nil, errRecordSuspended
	}
	newTag, err := s.forkd.Branch(ctx, lease.ForkdID, recordTag(rec.ID, "after"))
	if err != nil {
		s.store.mu.Lock()
		rec.stopping = false
		s.store.mu.Unlock()
		return nil, fmt.Errorf("branch after checkpoint: %w", err)
	}
	s.store.mu.Lock()
	rec.AfterTag = newTag
	rec.stopping = false
	rec.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.store.mu.Unlock()
	if s.metrics != nil {
		s.metrics.RecordsActive.Dec()
	}
	return rec.clone(), nil
}

func (s *Service) lookupRecord(owner, recordID string) *Record {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	rec := s.store.records[recordID]
	if rec == nil || rec.Owner != owner {
		return nil
	}
	return rec.clone()
}

// listRecords returns the owner's records, newest-first.
func (s *Service) listRecords(owner string) []*Record {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	out := make([]*Record, 0, len(s.store.records))
	for _, rec := range s.store.records {
		if rec.Owner == owner {
			out = append(out, rec.clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

func (s *Service) recordCount(owner string) int {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	n := 0
	for _, rec := range s.store.records {
		if rec.Owner == owner {
			n++
		}
	}
	return n
}

// hasOpenRecord reports whether the owner already has an open (un-stopped)
// record for the given sandbox.
func (s *Service) hasOpenRecord(owner, sandboxID string) bool {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	for _, rec := range s.store.records {
		if rec.Owner == owner && rec.SandboxID == sandboxID && rec.AfterTag == "" {
			return true
		}
	}
	return false
}

func (s *Service) deleteRecord(owner, recordID string) bool {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	rec := s.store.records[recordID]
	if rec == nil || rec.Owner != owner {
		return false
	}
	delete(s.store.records, recordID)
	if s.metrics != nil && rec.AfterTag == "" {
		s.metrics.RecordsActive.Dec()
	}
	return true
}

// replayRecord spawns a fresh sandbox from a record's after checkpoint
// (falling back to the before checkpoint when the run was never
// stopped), re-attaching the operator/agent to the recorded state. It
// returns the checkpoint tag that was used.
func (s *Service) replayRecord(ctx context.Context, owner, recordID string) (*Lease, string, error) {
	rec := s.lookupRecord(owner, recordID)
	if rec == nil {
		return nil, "", errRecordNotFound
	}
	tag := rec.AfterTag
	if tag == "" {
		tag = rec.BeforeTag
	}
	lease, err := s.grantFromSnapshot(ctx, owner, tag, s.maxTTL, true)
	if err != nil {
		return nil, "", err
	}
	if s.metrics != nil {
		s.metrics.RecordReplay.Inc()
	}
	return lease, tag, nil
}

// recordTag builds a branch tag for a checkpoint snapshot. It derives
// from the record id (random 32-hex) rather than the sandbox id, so
// same-sandbox same-second checkpoints cannot collide.
func recordTag(recordID, phase string) string {
	short := recordID
	if len(short) > 12 {
		short = short[:12]
	}
	return fmt.Sprintf("rec-%s-%s-%d", phase, short, time.Now().Unix())
}

// ---------- HTTP handlers ----------

func (s *Server) handleRecordStart(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	id := r.PathValue("id")
	var req struct {
		Label string `json:"label"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // optional body
	rec, err := s.svc.startRecord(r.Context(), owner, id, req.Label)
	if err != nil {
		s.writeRecordError(w, "record start", err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) handleRecordStop(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	rec, err := s.svc.stopRecord(r.Context(), owner, r.PathValue("id"))
	if err != nil {
		s.writeRecordError(w, "record stop", err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handleRecordsList(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"records": s.svc.listRecords(owner)})
}

func (s *Server) handleRecordGet(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	rec := s.svc.lookupRecord(owner, r.PathValue("id"))
	if rec == nil {
		writeError(w, http.StatusNotFound, "record not found")
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handleRecordReplay(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	recordID := r.PathValue("id")
	lease, tag, err := s.svc.replayRecord(r.Context(), owner, recordID)
	if err != nil {
		s.writeRecordError(w, "record replay", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         lease.ID,
		"image":      lease.Image,
		"source":     recordID,
		"branch_tag": tag,
		"persistent": lease.Persistent,
		"expires_at": lease.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleRecordDelete(w http.ResponseWriter, r *http.Request) {
	owner := ownerFrom(r.Context())
	if !s.svc.deleteRecord(owner, r.PathValue("id")) {
		writeError(w, http.StatusNotFound, "record not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeRecordError maps a record service error to an HTTP status using
// typed sentinel errors (errors.Is) and a forkd.Error downcast.
func (s *Server) writeRecordError(w http.ResponseWriter, op string, err error) {
	s.svc.log.Printf("%s: %v", op, err)
	switch {
	case errors.Is(err, errRecordNotFound), errors.Is(err, errRecordSandboxGone):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errRecordSuspended), errors.Is(err, errRecordStopped),
		errors.Is(err, errRecordAlreadyRecording):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, errRecordLabelTooLong):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, errQuotaExceeded), errors.Is(err, errRecordQuota):
		writeError(w, http.StatusTooManyRequests, err.Error())
	default:
		// A forkd 404 means the snapshot was removed (e.g. a replayed
		// checkpoint was GC'd) — surface it as 404, not a generic 500.
		var fe *forkd.Error
		if errors.As(err, &fe) && fe.StatusCode == http.StatusNotFound {
			writeError(w, http.StatusNotFound, "snapshot no longer exists")
			return
		}
		writeError(w, http.StatusInternalServerError, op+" failed")
	}
}
