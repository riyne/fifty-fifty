package receiptparse_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/riyne/fifty-fifty/internal/receiptparse"
)

// PrepareText is the one function the worker and the accuracy harness share. These tests
// need no database.

// Redaction runs on the whole text before the cut. With the cut at every position across a
// card number, no digit of it may survive: cut first and the limit would leave "4111 11".
func TestPrepareText_RedactsBeforeCutting(t *testing.T) {
	const card = "4111 1111 1111 1111"
	text := strings.Repeat("x", 40) + " " + card + " and more text after it"
	start := strings.Index(text, card)
	for limit := start - 2; limit <= start+len(card)+2; limit++ {
		got, counts := receiptparse.PrepareText(text, limit)
		if strings.Contains(got, "1111") || strings.Contains(got, "4111") {
			t.Fatalf("limit %d: part of a card number was kept: %q", limit, got)
		}
		if counts.Cards != 1 {
			t.Fatalf("limit %d: counted %d cards, want 1 (counts describe the whole text)", limit, counts.Cards)
		}
	}
}

func TestPrepareText_TrimsAfterTheCut(t *testing.T) {
	got, _ := receiptparse.PrepareText("  \n Milk 3.50\n\n   ", 0)
	if got != "Milk 3.50" {
		t.Errorf("got %q, want the text trimmed", got)
	}
	// A cut that ends in white space is trimmed too.
	got, _ = receiptparse.PrepareText("abc          def", 6)
	if got != "abc" {
		t.Errorf("got %q, want %q", got, "abc")
	}
	if got, _ := receiptparse.PrepareText(" \n\t ", 0); got != "" {
		t.Errorf("white space only: got %q, want empty (the worker then records no_text)", got)
	}
}

// Zero or less means the default cap, which is what the harness passes; the cut counts
// characters, not bytes, and never splits one.
func TestPrepareText_DefaultCapCountsCharactersNotBytes(t *testing.T) {
	text := strings.Repeat("é", receiptparse.DefaultMaxOCRChars+500) // two bytes each
	for _, limit := range []int{0, -1} {
		got, _ := receiptparse.PrepareText(text, limit)
		if n := utf8.RuneCountInString(got); n != receiptparse.DefaultMaxOCRChars {
			t.Errorf("limit %d: %d characters, want %d", limit, n, receiptparse.DefaultMaxOCRChars)
		}
		if !utf8.ValidString(got) {
			t.Errorf("limit %d: the cut split a character", limit)
		}
	}
	if got, _ := receiptparse.PrepareText(text, 10); utf8.RuneCountInString(got) != 10 {
		t.Errorf("an explicit limit of 10 gave %d characters", utf8.RuneCountInString(got))
	}
}

func TestPrepareText_CountsWhatWasRedacted(t *testing.T) {
	_, c := receiptparse.PrepareText("card 4111 1111 1111 1111 mail jo@example.com call 604-555-0187", 0)
	if c.Cards != 1 || c.Emails != 1 || c.Phones != 1 {
		t.Errorf("counts = %+v, want one of each", c)
	}
}
