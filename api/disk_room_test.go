package api

import (
	"errors"
	"testing"
	"time"
)

func TestDiskRoomComputeEachTerm(t *testing.T) {
	base := computeDiskRoom(1000, 500, 0, 0, 0, 0)
	if base.Usable != 500 {
		t.Fatalf("base usable = %d", base.Usable)
	}
	cases := []struct {
		name string
		got  diskRoom
		want int64
	}{
		{"reserved", computeDiskRoom(1000, 500, 100, 0, 0, 0), 400},
		{"floor", computeDiskRoom(1000, 500, 0, 0, 0, 10), 400},
		{"pending", computeDiskRoom(1000, 500, 0, 70, 0, 0), 430},
		{"freeing", computeDiskRoom(1000, 500, 0, 0, 30, 0), 530},
		{"all", computeDiskRoom(1000, 500, 100, 70, 30, 10), 260},
	}
	for _, c := range cases {
		if c.got.Usable != c.want {
			t.Errorf("%s: usable = %d, want %d", c.name, c.got.Usable, c.want)
		}
	}
	if f := computeDiskRoom(1000, 0, 0, 0, 0, 5).Floor; f != 50 {
		t.Errorf("floor = %d", f)
	}
}

func TestDiskRoomNegativeAllowed(t *testing.T) {
	r := computeDiskRoom(1000, 100, 200, 50, 0, 5)
	if r.Usable != 100-200-50-50 {
		t.Fatalf("usable = %d", r.Usable)
	}
}

func TestDiskReservePctParsing(t *testing.T) {
	cases := map[string]float64{
		"": 5, "abc": 5, "NaN": 5, "10": 10, "2.5": 2.5, "0": 0,
		"-3": 0, "75": 50, "50": 50, " 7 ": 7,
	}
	for in, want := range cases {
		t.Setenv("DISK_RESERVE_PCT", in)
		if got := diskReservePct(); got != want {
			t.Errorf("DISK_RESERVE_PCT=%q: got %v, want %v", in, got, want)
		}
	}
}

func TestDiskInflightReserveReleaseIdempotent(t *testing.T) {
	var d diskInflight
	rel := d.reserve(100)
	rel2 := d.reserve(50)
	if p, _ := d.settle(time.Now(), 0); p != 150 {
		t.Fatalf("pending = %d", p)
	}
	rel()
	rel()
	if p, _ := d.settle(time.Now(), 0); p != 50 {
		t.Fatalf("pending after double release = %d", p)
	}
	rel2()
	if p, _ := d.settle(time.Now(), 0); p != 0 {
		t.Fatalf("pending = %d", p)
	}
}

func TestDiskInflightSettleDropsByGrowth(t *testing.T) {
	var d diskInflight
	t0 := time.Now()
	d.noteFreeing(100, t0, 1000)
	if _, f := d.settle(t0.Add(time.Second), 1050); f != 100 {
		t.Fatalf("freeing = %d, want 100", f)
	}
	if _, f := d.settle(t0.Add(2*time.Second), 1100); f != 0 {
		t.Fatalf("freeing = %d, want 0", f)
	}
}

func TestDiskInflightSettleDropsBy30s(t *testing.T) {
	var d diskInflight
	t0 := time.Now()
	d.noteFreeing(100, t0, 1000)
	if _, f := d.settle(t0.Add(29*time.Second), 1000); f != 100 {
		t.Fatalf("freeing = %d, want 100", f)
	}
	if _, f := d.settle(t0.Add(30*time.Second), 1000); f != 0 {
		t.Fatalf("freeing = %d, want 0", f)
	}
}

func TestDiskInflightStatfsLag(t *testing.T) {
	// A victim was released but statfs free has not moved yet: the
	// freeing credit keeps Usable up, so no second victim is needed.
	var d diskInflight
	t0 := time.Now()
	const need = 100
	free := int64(400)
	d.noteFreeing(need, t0, free)
	p, f := d.settle(t0.Add(time.Second), free)
	room := computeDiskRoom(1000, free, 0, p, f, 0)
	if room.Usable < free+need-1 {
		t.Fatalf("usable = %d, want >= %d", room.Usable, free+need-1)
	}
}

func dl(id string, b int64, pinned bool, age time.Duration) diskLease {
	return diskLease{ID: id, Bytes: b, Pinned: pinned, PausedAt: time.Unix(10000, 0).Add(-age)}
}

func victimIDs(v []diskVictim) []string {
	var ids []string
	for _, x := range v {
		ids = append(ids, x.ID)
	}
	return ids
}

func TestDiskVictimsInsideSliceUntouched(t *testing.T) {
	owners := []diskOwner{
		{Owner: "req", UsedBytes: 10, SliceBytes: 100},
		{Owner: "a", UsedBytes: 90, SliceBytes: 100, Paused: []diskLease{dl("a1", 50, false, time.Hour)}},
	}
	if v := diskVictims(owners, "req", 10, 10); v != nil {
		t.Fatalf("victims = %v", victimIDs(v))
	}
}

func TestDiskVictimsStopsAtSlice(t *testing.T) {
	owners := []diskOwner{
		{Owner: "req", UsedBytes: 0, SliceBytes: 100},
		{Owner: "a", UsedBytes: 110, SliceBytes: 100, Paused: []diskLease{
			dl("a1", 20, false, 3*time.Hour), dl("a2", 20, false, 2*time.Hour)}},
	}
	// After a1 the owner sits at 90, inside its slice, so a2 is safe.
	if v := diskVictims(owners, "req", 40, 40); v != nil {
		t.Fatalf("victims = %v", victimIDs(v))
	}
}

func TestDiskVictimsRatioOrdering(t *testing.T) {
	owners := []diskOwner{
		{Owner: "req", UsedBytes: 0, SliceBytes: 100},
		{Owner: "a", UsedBytes: 150, SliceBytes: 100, Paused: []diskLease{dl("a1", 10, false, time.Hour)}},
		{Owner: "b", UsedBytes: 300, SliceBytes: 100, Paused: []diskLease{dl("b1", 10, false, time.Hour)}},
		{Owner: "c", UsedBytes: 200, SliceBytes: 100, Paused: []diskLease{dl("c1", 10, false, time.Hour)}},
	}
	got := victimIDs(diskVictims(owners, "req", 30, 30))
	want := []string{"b1", "c1", "a1"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("victims = %v, want %v", got, want)
	}
}

func TestDiskVictimsRequesterRatioBlocks(t *testing.T) {
	// Requester would sit at (150+50)/100 = 2.0; owner at 1.5 is not above.
	owners := []diskOwner{
		{Owner: "req", UsedBytes: 150, SliceBytes: 100},
		{Owner: "a", UsedBytes: 150, SliceBytes: 100, Paused: []diskLease{dl("a1", 50, false, time.Hour)}},
	}
	if v := diskVictims(owners, "req", 50, 50); v != nil {
		t.Fatalf("victims = %v", victimIDs(v))
	}
}

func TestDiskVictimsOldestFirstPinnedNever(t *testing.T) {
	owners := []diskOwner{
		{Owner: "req", UsedBytes: 0, SliceBytes: 100},
		{Owner: "a", UsedBytes: 300, SliceBytes: 100, Paused: []diskLease{
			dl("new", 10, false, time.Hour),
			dl("pinned", 100, true, 10*time.Hour),
			dl("old", 10, false, 5*time.Hour),
		}},
	}
	got := victimIDs(diskVictims(owners, "req", 15, 15))
	if len(got) != 2 || got[0] != "old" || got[1] != "new" {
		t.Fatalf("victims = %v", got)
	}
	if v := diskVictims(owners, "req", 25, 25); v != nil {
		t.Fatalf("pinned must never be chosen, got %v", victimIDs(v))
	}
}

func TestDiskVictimsNilWhenImpossible(t *testing.T) {
	if v := diskVictims(nil, "req", 10, 10); v != nil {
		t.Fatal("want nil")
	}
	owners := []diskOwner{{Owner: "a", UsedBytes: 300, SliceBytes: 100, Paused: []diskLease{dl("a1", 5, false, time.Hour)}}}
	if v := diskVictims(owners, "req", 10, 10); v != nil {
		t.Fatal("want nil")
	}
}

func TestDiskRoomBoxFullError(t *testing.T) {
	var err error = &boxFullError{Owner: "x", UsedBytes: 2, SliceBytes: 1}
	if !errors.Is(err, errBoxFull) || err.Error() == "" {
		t.Fatal("errBoxFull should match")
	}
}
