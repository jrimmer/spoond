package api

// A memory box_full a create answers after an admission wait
// (spoond-2rkm): writeCreateRefusal stamps waited_ms inside the JSON
// body writeBoxFull builds — one closed object, never appended bytes
// after it. The broken shape was `…,"own_leases":[…]}\n,"waited_ms":150}`
// — invalid JSON.

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

// stampWriter is a response recorder exposing its bytes.
type stampWriter struct {
	*httptest.ResponseRecorder
}

// stampErrWriter records a writeBoxFull body.
func stampErrWriter() *stampWriter {
	return &stampWriter{ResponseRecorder: httptest.NewRecorder()}
}

// TestBoxFullAfterWaitValidJSON: an admission-wait timeout answered with
// the biggest-borrower box_full carries waited_ms beside own_leases in
// one valid JSON object (stamped inside the body, at unit level).
func TestBoxFullAfterWaitValidJSON(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	rec := stampErrWriter()
	writeBoxFull(rec, &memBoxFullError{
		Owner: "alice", UsedMiB: 4096, SliceMiB: 2048, RequestMiB: 2048,
		Leases: []BoxFullLease{{ID: "a-1", MemoryMB: 4096, LastActive: now, Pinned: true}},
	}, 1500*time.Millisecond)
	got := rec.Body.String()
	var decoded struct {
		Error    string `json:"error"`
		Code     string `json:"code"`
		WaitedMS int64  `json:"waited_ms"`
		Leases   []struct {
			ID string `json:"id"`
		} `json:"own_leases"`
	}
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %v (%s)", err, got)
	}
	if decoded.Code != "box_full" || decoded.WaitedMS != 1500 || len(decoded.Leases) != 1 || decoded.Leases[0].ID != "a-1" {
		t.Fatalf("body lost a field: %s", got)
	}
}

// TestAdmitQueueForwardsBoxFullRetry: the queue forwards a retry's own
// refusal to the handler, and takeBackForAdmission's bounded re-plans
// return the memBoxFullError unguarded (admit_memory.go's every return
// from the loop), so a capacity-wait queued create, retried while still
// short over its own slice, is refused with the owner's own
// memBoxFullError when biggestBorrowerRefusal fires inside the loop —
// exactly the case append-on-a-closed-body corrupted.
func TestAdmitQueueForwardsBoxFullRetry(t *testing.T) {
	owners := []takeBackOwner{
		{Owner: "alice", UsedMiB: 4096, SliceMiB: 2048, Leases: []takeBackLease{
			{ID: "a-1", MemoryMiB: 4096, LastActive: time.Unix(1_000_000, 0)},
		}},
		{Owner: "bob", UsedMiB: 512, SliceMiB: 2048, Leases: []takeBackLease{
			{ID: "b-1", MemoryMiB: 512, LastActive: time.Unix(1_000_000, 0)},
		}},
	}
	err := biggestBorrowerRefusal(owners, "alice", 4096)
	var bf *memBoxFullError
	if !errors.As(err, &bf) || len(bf.Leases) != 1 || bf.Leases[0].ID != "a-1" {
		t.Fatalf("the loop's refusal lost the requester's own lease row: %v", err)
	}
}

// TestWriteBoxFullStampShape: a refusal with no wait behind it carries
// no waited_ms key (the key is not stubbed to 0 for callers that never
// waited).
func TestWriteBoxFullNoStampWithoutWait(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	rec := stampErrWriter()
	writeBoxFull(rec, &memBoxFullError{
		Owner: "alice", UsedMiB: 4096, SliceMiB: 2048, RequestMiB: 1024,
		Leases: []BoxFullLease{{ID: "a-1", MemoryMB: 1024, LastActive: now}},
	}, 0)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if _, ok := body["waited_ms"]; ok {
		t.Fatalf("a zero wait must not emit waited_ms: %v", body)
	}
	if _, ok := body["own_leases"]; !ok {
		t.Fatalf("own_leases lost: %v", body)
	}
}

// TestWriteBoxFullNoLeasesNoStampKey: a disk/pinned box_full (errBoxFull,
// not a *memBoxFullError) answers without own_leases and stays valid
// JSON: no waited_ms either — the queue only ever forwards a
// substrate.ErrCapacity refusal (not a box_full), so a wait answer for
// the leaseless flavour is not a shape any caller produces.
func TestWriteBoxFullNoLeasesNoStampKey(t *testing.T) {
	rec := stampErrWriter()
	writeBoxFull(rec, errBoxFull, 0)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if _, ok := body["own_leases"]; ok {
		t.Fatalf("a leaseless box_full must not carry own_leases: %v", body)
	}
	if _, ok := body["waited_ms"]; ok {
		t.Fatalf("a leaseless box_full answered without a wait must not carry waited_ms: %v", body)
	}
}
