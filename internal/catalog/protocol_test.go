package catalog

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestGenerationWireUsesCanonicalPositiveInt64Strings(t *testing.T) {
	for _, wire := range []string{`"1"`, `"9007199254740993"`, `"9223372036854775807"`} {
		var got Generation
		if err := json.Unmarshal([]byte(wire), &got); err != nil {
			t.Fatalf("decode %s: %v", wire, err)
		}
		out, err := json.Marshal(got)
		if err != nil || string(out) != wire {
			t.Fatalf("round trip %s = %s, %v", wire, out, err)
		}
	}
	for _, wire := range []string{`1`, `null`, `""`, `"0"`, `"-1"`, `"+1"`, `"01"`, `"1.0"`, `" 1"`, `"9223372036854775808"`} {
		var got Generation
		if err := json.Unmarshal([]byte(wire), &got); err == nil {
			t.Errorf("accepted invalid generation %s", wire)
		}
	}
	if _, err := json.Marshal(Generation(0)); err == nil {
		t.Error("encoded an unallocated generation")
	}
}

func TestObservationIdentityRejectsUnownedScopeAndMissingFence(t *testing.T) {
	valid := Identity{Scope: ScopeLocal, Epoch: uuid.New(), Generation: 1}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Identity){
		func(v *Identity) { v.Scope = "remote" },
		func(v *Identity) { v.Scope = "" },
		func(v *Identity) { v.Epoch = uuid.Nil },
		func(v *Identity) { v.Generation = 0 },
	} {
		bad := valid
		mutate(&bad)
		if err := bad.Validate(); err == nil {
			t.Errorf("accepted invalid identity %+v", bad)
		}
	}
}

func TestMembershipDigestIsSortedUniqueCanonicalIDs(t *testing.T) {
	a := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	b := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	ids := []uuid.UUID{b, a}
	got, err := MembershipDigest(ids)
	if err != nil {
		t.Fatal(err)
	}
	const want = "a929eae1f77de69cc99f751f774e5d3b63a10df5f5c9fecbfa924f4baedad3c3"
	if got != want {
		t.Errorf("digest = %s, want %s", got, want)
	}
	if ids[0] != b {
		t.Error("digest mutated the caller's page order")
	}
	empty, err := MembershipDigest(nil)
	if err != nil || empty != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("empty digest = %s, %v", empty, err)
	}
	for _, invalid := range [][]uuid.UUID{{a, a}, {uuid.Nil}} {
		if _, err := MembershipDigest(invalid); err == nil {
			t.Errorf("accepted invalid membership %v", invalid)
		}
	}
}
