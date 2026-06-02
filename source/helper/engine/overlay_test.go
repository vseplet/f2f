package engine

import "testing"

const knownPub = "e2dca0d029098ea33875f557bf0f7ac3452092cce46ee9f4f7063fde2e0ab89c"

func TestPubToV4Addr(t *testing.T) {
	addr, err := PubToV4Addr(knownPub)
	if err != nil {
		t.Fatal(err)
	}
	// Must be in 100.64.0.0/10.
	b := addr.As4()
	if b[0] != 100 || (b[1]&0xC0) != 0x40 {
		t.Errorf("addr %s not in 100.64.0.0/10", addr)
	}
}

func TestPubToV4Addr_Deterministic(t *testing.T) {
	a, _ := PubToV4Addr(knownPub)
	b, _ := PubToV4Addr(knownPub)
	if a != b {
		t.Errorf("not deterministic: %s vs %s", a, b)
	}
}

func TestPubToV4Addr_DifferentPubs(t *testing.T) {
	a, _ := PubToV4Addr(knownPub)
	b, _ := PubToV4Addr("e58ece2af195c0ca8fb1cce3450407d900784602e6e5b6b6f149edd57ce373a8")
	if a == b {
		t.Error("different pubs got same v4")
	}
}

func TestPubToV4Addr_BadPub(t *testing.T) {
	for _, bad := range []string{"", "zz", "12", knownPub + "ff"} {
		if _, err := PubToV4Addr(bad); err == nil {
			t.Errorf("expected error for pub=%q", bad)
		}
	}
}
