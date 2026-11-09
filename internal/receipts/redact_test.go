package receipts_test

import (
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"

	"github.com/riyne/fifty-fifty/internal/receipts"
)

func TestRedact(t *testing.T) {
	cases := []struct {
		name, in, want        string
		cards, phones, emails int
	}{
		{"card, spaces", "VISA 4111 1111 1111 1111 APPROVED", "VISA [CARD] APPROVED", 1, 0, 0},
		{"card, dashes", "card 4111-1111-1111-1111", "card [CARD]", 1, 0, 0},
		{"card, run of digits", "4111111111111111", "[CARD]", 1, 0, 0},
		{"amex 15 digits", "AMEX 3782 822463 10005", "AMEX [CARD]", 1, 0, 0},
		{"19 digits", "6212345678901234567", "[CARD]", 1, 0, 0},
		{"phone, dashes", "Call 604-555-0199 now", "Call [PHONE] now", 0, 1, 0},
		{"phone, parentheses", "(604) 555-0199", "[PHONE]", 0, 1, 0},
		{"phone, dots", "604.555.0199", "[PHONE]", 0, 1, 0},
		{"phone, country code", "+1 604 555 0199", "[PHONE]", 0, 1, 0},
		{"phone, plain digits", "tel 6045550199", "tel [PHONE]", 0, 1, 0},
		{"phone, international", "+44 20 7946 0958", "[PHONE]", 0, 1, 0},
		{"email", "mail jane.doe+tips@mail.example.co.uk please", "mail [EMAIL] please", 0, 0, 1},
		{"short email", "a@b.io", "[EMAIL]", 0, 0, 1},
		{"all three", "4111 1111 1111 1111 / jane@example.com / 604-555-0199", "[CARD] / [EMAIL] / [PHONE]", 1, 1, 1},
		{"two cards", "4111 1111 1111 1111 and 5500 0000 0000 0004", "[CARD] and [CARD]", 2, 0, 0},
		// Things that look like numbers but are not PII.
		{"prices", "Pizza 15.00  Beer 8.50  TOTAL $1,234.56", "Pizza 15.00  Beer 8.50  TOTAL $1,234.56", 0, 0, 0},
		{"date and time", "2026-11-06 14:30:12", "2026-11-06 14:30:12", 0, 0, 0},
		{"9 digits is neither", "ORDER 123456789", "ORDER 123456789", 0, 0, 0},
		// Indistinguishable from a phone number with a country code (44 20 7946 0958 is 12
		// digits), and a missed phone is the worse mistake: redacted.
		{"12 digits reads as a phone", "ORDER 123456789012", "ORDER [PHONE]", 0, 1, 0},
		{"13 digits is a card", "ORDER 1234567890123", "ORDER [CARD]", 1, 0, 0},
		{"phone glued to letters", "SKU6045550199X", "SKU[PHONE]X", 0, 1, 0}, // over-redaction is the chosen side
		{"names are not redacted", "Server: Jane Doe, table 12", "Server: Jane Doe, table 12", 0, 0, 0},
		{"empty", "", "", 0, 0, 0},
	}
	for _, c := range cases {
		got, n := receipts.Redact(c.in)
		if got != c.want || n.Cards != c.cards || n.Phones != c.phones || n.Emails != c.emails {
			t.Errorf("%s: Redact(%q) = %q %+v, want %q cards=%d phones=%d emails=%d", c.name, c.in, got, n, c.want, c.cards, c.phones, c.emails)
		}
		if n.Total() != c.cards+c.phones+c.emails {
			t.Errorf("%s: Total = %d", c.name, n.Total())
		}
		if again, m := receipts.Redact(got); again != got || m.Total() != 0 {
			t.Errorf("%s: redacting twice changed %q to %q (%+v)", c.name, got, again, m)
		}
	}
}

// OCR sometimes puts one space beside a dot inside an address. Before the pattern tolerated them it
// missed these, or matched only the part after the space and left the start of the local
// part in the text. Each case below was seen in, or is the same kind as, the leaks in
// bench/results/redaction-20261107T044640Z.txt.
func TestRedact_EmailsWithAnOCRSpaceBesideADot(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"space after a dot in the local part", "Customer alex. k6@example.com", "Customer [EMAIL]"},
		{"space before a dot in the local part", "chris .o88@example.com", "[EMAIL]"},
		{"space after the dot before the tld", "r.singh36@example. org", "[EMAIL]"},
		{"space before the dot before the tld", "dana.w30@example .org", "[EMAIL]"},
		{"space in a middle label", "x@mail. example.co.uk", "[EMAIL]"},
		{"two spaces in one address", "alex. k6@example. com", "[EMAIL]"},
		{"unchanged: no spaces", "jane.doe+tips@mail.example.co.uk", "[EMAIL]"},
		// The capital after a dot is the next sentence, not the address.
		{"next sentence is kept", "write jo@example.com. Thank you", "write [EMAIL]. Thank you"},
		{"next word after a dot is kept when capital", "jo@example.com. Total 12.50", "[EMAIL]. Total 12.50"},
	} {
		got, n := receipts.Redact(c.in)
		if got != c.want || n.Emails != 1 || n.Cards != 0 || n.Phones != 0 {
			t.Errorf("%s: Redact(%q) = %q %+v, want %q with one email", c.name, c.in, got, n, c.want)
		}
	}
}

// The spaces are tolerated only inside an address: text with no @ is left alone, and a
// space beside the @ is not tolerated (a known limit, below).
func TestRedact_SpaceToleranceDoesNotTouchOrdinaryText(t *testing.T) {
	for _, in := range []string{
		"Subtotal. 12.50  Tax. 1.50",
		"Qty 2. milk. bread. eggs",
		"Total. 14.00 www. example. com",
		"thanks. see you. soon",
	} {
		if got, n := receipts.Redact(in); got != in || n.Total() != 0 {
			t.Errorf("Redact(%q) = %q %+v, want it unchanged", in, got, n)
		}
	}
}

// What pattern matching cannot do, pinned so that nobody comes to rely on it: spelled
// out addresses, digits written as words, and anything that is not one of the three kinds.
func TestRedact_KnownLimits(t *testing.T) {
	for _, in := range []string{
		"jane at example dot com",
		"jane doe @example.com", // a space beside the @ is not tolerated (only one beside a dot is allowed)
		"call six zero four five five five zero one nine nine",
		"Jane Doe, 123 Main Street, Vancouver BC V6B 1A1",
		"SIN 123 456 789",
	} {
		if got, n := receipts.Redact(in); got != in || n.Total() != 0 {
			t.Errorf("Redact(%q) = %q %+v: the limit changed; update the claim and the docs", in, got, n)
		}
	}
}

var digitRun = regexp.MustCompile(`[0-9]{13,}`)

// Planted PII in random surroundings: after Redact, no run of 13 or more digits is left,
// and none of the planted values is.
func TestRedact_PlantedValuesAreGone(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 5))
	words := []string{"Pizza", "15.00", "Beer", "x2", "TOTAL", "Server:", "Jane", "table", "12", "#", "-", "THANK YOU", "\n"}
	digits := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte('0' + rng.IntN(10))
		}
		return string(b)
	}
	for iter := 0; iter < 3000; iter++ {
		card := digits(13 + rng.IntN(7))
		if iter%2 == 0 { // grouped by fours with spaces or dashes
			sep := []string{" ", "-"}[rng.IntN(2)]
			var parts []string
			for i := 0; i < len(card); i += 4 {
				parts = append(parts, card[i:min(i+4, len(card))])
			}
			card = strings.Join(parts, sep)
		}
		email := "u" + digits(3) + "@host" + digits(2) + ".example.com"
		var b strings.Builder
		for i := 0; i < 6; i++ {
			b.WriteString(words[rng.IntN(len(words))] + " ")
		}
		b.WriteString(card + " " + words[rng.IntN(len(words))] + " " + email + "\n")
		in := b.String()
		out, _ := receipts.Redact(in)
		if strings.Contains(out, email) || digitRun.MatchString(out) || strings.Contains(out, strings.ReplaceAll(strings.ReplaceAll(card, " ", ""), "-", "")) {
			t.Fatalf("planted value survived:\n in: %q\nout: %q", in, out)
		}
	}
}
