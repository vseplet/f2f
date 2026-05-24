package overlay

import "testing"

// Known vectors. Pub is the one persisted in ~/.f2f/12345.config.json
// during initial development; fingerprint of this pub is
// 12f3478eae314826 — i.e. the v6 host part below matches.
const knownPub = "e2dca0d029098ea33875f557bf0f7ac3452092cce46ee9f4f7063fde2e0ab89c"

func TestPubToAddr_KnownVectors(t *testing.T) {
	cases := []struct {
		camp string
		want string
	}{
		// sha256("12345")[:5]    = 59 94 47 1a bb
		{camp: "12345", want: "fd59:9447:1abb:0:12f3:478e:ae31:4826"},
		// sha256("testcamp")[:5] = 5e fb df 99 ec
		{camp: "testcamp", want: "fd5e:fbdf:99ec:0:12f3:478e:ae31:4826"},
	}
	for _, tc := range cases {
		got, err := PubToAddr(tc.camp, knownPub)
		if err != nil {
			t.Fatalf("camp=%q: %v", tc.camp, err)
		}
		if got.String() != tc.want {
			t.Errorf("camp=%q: got %s, want %s", tc.camp, got, tc.want)
		}
	}
}

func TestPubToAddr_HostPartStableAcrossCamps(t *testing.T) {
	a, err := PubToAddr("camp-a", knownPub)
	if err != nil {
		t.Fatal(err)
	}
	b, err := PubToAddr("camp-b", knownPub)
	if err != nil {
		t.Fatal(err)
	}
	ab := a.As16()
	bb := b.As16()
	// Host bytes (8..16) must be identical, prefix (0..6) different.
	for i := 8; i < 16; i++ {
		if ab[i] != bb[i] {
			t.Errorf("host byte %d differs: %02x vs %02x", i, ab[i], bb[i])
		}
	}
	samePrefix := true
	for i := 1; i < 6; i++ {
		if ab[i] != bb[i] {
			samePrefix = false
			break
		}
	}
	if samePrefix {
		t.Error("camp prefix unexpectedly identical for different camp_ids")
	}
}

func TestPubToAddr_BadPub(t *testing.T) {
	for _, bad := range []string{"", "zz", "12", knownPub + "ff"} {
		if _, err := PubToAddr("camp", bad); err == nil {
			t.Errorf("expected error for pub=%q", bad)
		}
	}
}

func TestCampPrefix(t *testing.T) {
	p := CampPrefix("12345")
	if p.Bits() != 48 {
		t.Errorf("bits = %d, want 48", p.Bits())
	}
	if want := "fd59:9447:1abb::"; p.Addr().String() != want {
		t.Errorf("prefix addr = %s, want %s", p.Addr(), want)
	}
}
