package session

import "testing"

// TestAdmissionsSingleFlight pins the busy policy Accept enforces: exactly one
// held slot, released by the returned callback, idempotently.
func TestAdmissionsSingleFlight(t *testing.T) {
	var a Admissions
	release, ok := a.acquire()
	if !ok {
		t.Fatal("first acquire must admit")
	}
	if _, ok := a.acquire(); ok {
		t.Fatal("second acquire must be rejected while the slot is held")
	}
	release()
	release() // idempotent
	release2, ok := a.acquire()
	if !ok {
		t.Fatal("acquire after release must admit")
	}
	release2()
}

// TestAdmissionsNilAlwaysAdmits pins the documented nil policy: a caller that
// wants no busy check admits unconditionally.
func TestAdmissionsNilAlwaysAdmits(t *testing.T) {
	var a *Admissions
	for i := 0; i < 3; i++ {
		release, ok := a.acquire()
		if !ok {
			t.Fatalf("nil admissions must always admit (attempt %d)", i+1)
		}
		release()
	}
}
