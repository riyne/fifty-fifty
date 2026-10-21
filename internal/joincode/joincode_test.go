package joincode_test

import (
	"strings"
	"testing"

	"github.com/riyne/fifty-fifty/internal/joincode"
)

func TestGenerate_ShapeAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		c := joincode.Generate()
		if len(c) != 12 {
			t.Fatalf("len(%q) = %d, want 12", c, len(c))
		}
		if n, err := joincode.Normalize(c); err != nil || n != c {
			t.Fatalf("generated code does not round-trip: %q -> %q, %v", c, n, err)
		}
		if seen[c] {
			t.Fatalf("duplicate code %q in 1000 draws", c)
		}
		seen[c] = true
	}
}

func TestNormalize(t *testing.T) {
	const want = "ABCD1234EFGH"
	for _, in := range []string{
		"ABCD1234EFGH", "abcd1234efgh", "ABCD-1234-EFGH", "abcd 1234 efgh", " ABCD-1234-EFGH ",
		"ABCDI234EFGH", "ABCDl234EFGH", // I and L read as 1
	} {
		got, err := joincode.Normalize(in)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if got, _ := joincode.Normalize("OOOO-OOOO-OOOO"); got != "000000000000" {
		t.Errorf("O should read as 0, got %q", got)
	}
}

func TestNormalize_Rejects(t *testing.T) {
	for _, in := range []string{
		"", "ABCD", "ABCD-1234-EFGH-X", strings.Repeat("A", 13), "ABCD-1234-EFG!",
		"ABCD-1234-EFGU", // U is not in the alphabet
		"ABCD-1234-EFGé", // non-ASCII
		"ABCD-1234-EFG\x00",
	} {
		if got, err := joincode.Normalize(in); err == nil {
			t.Errorf("Normalize(%q) = %q, want error", in, got)
		}
	}
}

func TestHash_StableAndFormatIndependent(t *testing.T) {
	a, _ := joincode.Normalize("abcd-1234-efgh")
	b, _ := joincode.Normalize("ABCD1234EFGH")
	if string(joincode.Hash(a)) != string(joincode.Hash(b)) {
		t.Fatal("same code in different formats hashed differently")
	}
	if len(joincode.Hash(a)) != 32 {
		t.Fatal("expected SHA-256")
	}
	if got := joincode.Display(a); got != "ABCD-1234-EFGH" {
		t.Fatalf("Display = %q", got)
	}
}
