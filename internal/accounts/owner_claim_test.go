package accounts

import (
	"testing"
)

func TestAssertLocalOwnerAllowsUnclaimedAndLocal(t *testing.T) {
	t.Setenv("SUBROUTER_HOST_ID", "host-a")
	ResetLocalHostIDForTest()
	defer ResetLocalHostIDForTest()

	if err := AssertLocalOwner("a@example.com", nil); err != nil {
		t.Fatalf("nil claim: %v", err)
	}
	local := &OwnerClaim{Host: "host-a", Epoch: 2}
	if err := AssertLocalOwner("a@example.com", local); err != nil {
		t.Fatalf("local claim: %v", err)
	}
}

func TestAssertLocalOwnerRejectsForeign(t *testing.T) {
	t.Setenv("SUBROUTER_HOST_ID", "host-a")
	ResetLocalHostIDForTest()
	defer ResetLocalHostIDForTest()

	err := AssertLocalOwner("burned@example.com", &OwnerClaim{Host: "host-b", Epoch: 3, ClaimedAt: "2026-07-18T01:32:01Z"})
	if err == nil {
		t.Fatal("expected foreign owner error")
	}
	var foreign *ForeignOwnerClaimError
	if !asForeignOwner(err, &foreign) {
		t.Fatalf("want ForeignOwnerClaimError, got %T %v", err, err)
	}
	if foreign.Claim.Host != "host-b" || foreign.Local != "host-a" {
		t.Fatalf("unexpected foreign error: %+v", foreign)
	}
}

func asForeignOwner(err error, target **ForeignOwnerClaimError) bool {
	if err == nil {
		return false
	}
	e, ok := err.(*ForeignOwnerClaimError)
	if !ok {
		return false
	}
	*target = e
	return true
}

func TestStampOwnerClaimDoesNotSilentTakeover(t *testing.T) {
	t.Setenv("SUBROUTER_HOST_ID", "host-a")
	ResetLocalHostIDForTest()
	defer ResetLocalHostIDForTest()

	foreign := &OwnerClaim{Host: "host-b", Epoch: 4, ClaimedAt: "old"}
	kept := StampOwnerClaim(foreign, false)
	if kept.Host != "host-b" || kept.Epoch != 4 {
		t.Fatalf("silent save must not steal claim: %+v", kept)
	}

	taken := TakeoverOwnerClaim(foreign)
	if taken.Host != "host-a" || taken.Epoch != 5 {
		t.Fatalf("takeover should bump epoch onto local host: %+v", taken)
	}
}

func TestStampOwnerClaimStampsUnclaimed(t *testing.T) {
	t.Setenv("SUBROUTER_HOST_ID", "host-a")
	ResetLocalHostIDForTest()
	defer ResetLocalHostIDForTest()

	claim := StampOwnerClaim(nil, false)
	if claim.Host != "host-a" || claim.Epoch != 1 || claim.ClaimedAt == "" {
		t.Fatalf("unclaimed stamp: %+v", claim)
	}
}
