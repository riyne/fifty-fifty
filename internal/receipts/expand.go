package receipts

import (
	"fmt"
	"unicode/utf8"
)

// MaxItems is the most items an expense can have (the expense API's limit). Expanding
// quantity lines that would exceed it is refused.
const MaxItems = 100

// MaxDescriptionRunes is the longest item description the expense API accepts.
const MaxDescriptionRunes = 200

// Item is one claimable unit.
type Item struct {
	Description string
	PriceMinor  int64
}

// ExpandQuantity splits a receipt line of qty units costing lineTotal in all into qty
// items, one per unit, so each can be claimed on its own. The unit prices sum to exactly
// lineTotal: every unit gets lineTotal/qty and the first lineTotal%qty units get one
// minor unit more (the same result as allocator.Apportion with equal weights, whose ties
// go to the lowest id, if units are numbered in order; a test pins that). With qty 1 the
// description is kept as it is; otherwise each unit is called "<description> (i/qty)",
// the description cut on a character boundary to fit the limit.
func ExpandQuantity(description string, qty int, lineTotal int64) ([]Item, error) {
	switch {
	case qty < 1:
		return nil, fmt.Errorf("receipts: quantity %d is not positive", qty)
	case qty > MaxItems:
		return nil, fmt.Errorf("receipts: quantity %d is more than the %d items an expense can have", qty, MaxItems)
	case lineTotal < 0:
		return nil, fmt.Errorf("receipts: negative line total")
	}
	if qty == 1 {
		return []Item{{Description: description, PriceMinor: lineTotal}}, nil
	}
	base, extra := lineTotal/int64(qty), lineTotal%int64(qty)
	out := make([]Item, qty)
	for i := range out {
		price := base
		if int64(i) < extra {
			price++
		}
		suffix := fmt.Sprintf(" (%d/%d)", i+1, qty)
		out[i] = Item{Description: truncateRunes(description, MaxDescriptionRunes-utf8.RuneCountInString(suffix)) + suffix, PriceMinor: price}
	}
	return out, nil
}

// truncateRunes cuts s to at most n runes, on a rune boundary.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}
