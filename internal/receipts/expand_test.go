package receipts_test

import (
	"encoding/binary"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/riyne/fifty-fifty/internal/allocator"
	"github.com/riyne/fifty-fifty/internal/receipts"
)

func prices(items []receipts.Item) []int64 {
	out := make([]int64, len(items))
	for i, it := range items {
		out[i] = it.PriceMinor
	}
	return out
}

func TestExpandQuantity(t *testing.T) {
	cases := []struct {
		desc  string
		qty   int
		total int64
		want  []int64
	}{
		{"Beer", 1, 500, []int64{500}},
		{"Beer", 3, 1500, []int64{500, 500, 500}},
		{"Beer", 3, 1000, []int64{334, 333, 333}}, // the extra cent goes to the first unit
		{"Beer", 3, 1001, []int64{334, 334, 333}},
		{"Beer", 2, 1, []int64{1, 0}},
		{"Beer", 4, 0, []int64{0, 0, 0, 0}},
		{"Beer", 1, 0, []int64{0}},
		{"Beer", 100, 100_000_000, nil}, // checked below
	}
	for _, c := range cases {
		got, err := receipts.ExpandQuantity(c.desc, c.qty, c.total)
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		if len(got) != c.qty {
			t.Fatalf("%+v: %d items", c, len(got))
		}
		if c.want != nil {
			p := prices(got)
			for i := range p {
				if p[i] != c.want[i] {
					t.Errorf("%s x%d of %d: prices %v, want %v", c.desc, c.qty, c.total, p, c.want)
					break
				}
			}
		}
	}
}

func TestExpandQuantity_Descriptions(t *testing.T) {
	one, _ := receipts.ExpandQuantity("Beer", 1, 500)
	if one[0].Description != "Beer" {
		t.Errorf("a single unit keeps its description: %q", one[0].Description)
	}
	three, _ := receipts.ExpandQuantity("Beer", 3, 1500)
	for i, want := range []string{"Beer (1/3)", "Beer (2/3)", "Beer (3/3)"} {
		if three[i].Description != want {
			t.Errorf("unit %d = %q, want %q", i, three[i].Description, want)
		}
	}
	// Long, multi-byte descriptions are cut on a character boundary to fit the limit.
	long := strings.Repeat("é", 200)
	units, err := receipts.ExpandQuantity(long, 12, 1200)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, u := range units {
		if !utf8.ValidString(u.Description) || utf8.RuneCountInString(u.Description) > receipts.MaxDescriptionRunes {
			t.Fatalf("description %q: valid=%v runes=%d", u.Description, utf8.ValidString(u.Description), utf8.RuneCountInString(u.Description))
		}
		if seen[u.Description] {
			t.Fatalf("two units are called %q", u.Description)
		}
		seen[u.Description] = true
		if !strings.HasSuffix(u.Description, "/12)") {
			t.Fatalf("suffix lost: %q", u.Description)
		}
	}
}

func TestExpandQuantity_Refusals(t *testing.T) {
	for _, c := range []struct {
		qty   int
		total int64
	}{{0, 100}, {-1, 100}, {receipts.MaxItems + 1, 100}, {1 << 30, 100}, {2, -1}} {
		if _, err := receipts.ExpandQuantity("x", c.qty, c.total); err == nil {
			t.Errorf("qty %d total %d was accepted", c.qty, c.total)
		}
	}
}

// Whatever the quantity and total: the units add up to the line total exactly, no unit
// differs from another by more than one minor unit, the extra units come first, and the
// result is what allocator.Apportion gives equal weights over ids in unit order (the
// split "3 x beer" shares with the way shared items are split between claimants).
func TestExpandQuantity_Properties(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	for iter := 0; iter < 20000; iter++ {
		qty := 1 + rng.IntN(receipts.MaxItems)
		total := rng.Int64N(receipts.MaxAmountMinor + 1)
		if iter%5 == 0 {
			total = rng.Int64N(int64(3 * qty)) // small totals, where remainders dominate
		}
		items, err := receipts.ExpandQuantity("x", qty, total)
		if err != nil {
			t.Fatal(err)
		}
		var sum int64
		for i, it := range items {
			sum += it.PriceMinor
			if i > 0 && it.PriceMinor > items[i-1].PriceMinor {
				t.Fatalf("qty %d total %d: prices not non-increasing: %v", qty, total, prices(items))
			}
		}
		if sum != total {
			t.Fatalf("qty %d total %d: units add up to %d", qty, total, sum)
		}
		if d := items[0].PriceMinor - items[qty-1].PriceMinor; d < 0 || d > 1 {
			t.Fatalf("qty %d total %d: spread %d", qty, total, d)
		}
		if iter%50 == 0 { // cross-check against the allocator
			ws := make([]allocator.Weight, qty)
			for i := range ws {
				var id uuid.UUID
				binary.BigEndian.PutUint32(id[:4], uint32(i)) // ids in unit order
				ws[i] = allocator.Weight{ID: id, W: 1}
			}
			shares, err := allocator.Apportion(total, ws)
			if err != nil {
				t.Fatal(err)
			}
			for i, sh := range shares {
				if sh.Amount != items[i].PriceMinor {
					t.Fatalf("qty %d total %d: unit %d is %d, allocator says %d", qty, total, i, items[i].PriceMinor, sh.Amount)
				}
			}
		}
	}
}
