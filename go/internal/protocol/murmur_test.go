package protocol

import (
	"math/big"
	"testing"
)

func TestGoldenMurmur(t *testing.T) {
	cases := loadGolden[struct {
		S string `json:"s"`
		H string `json:"h"`
	}](t, "golden_murmur.json")
	if len(cases) < 100 {
		t.Fatalf("too few murmur cases: %d", len(cases))
	}
	for _, c := range cases {
		want := new(big.Int)
		if _, ok := want.SetString(c.H, 10); !ok {
			t.Fatalf("bad hash number: %q", c.H)
		}
		got := MurmurHashString(c.S)
		if new(big.Int).SetUint64(got).Cmp(want) != 0 {
			t.Fatalf("murmur mismatch for %q: got %d want %s", c.S, got, c.H)
		}
	}
}
