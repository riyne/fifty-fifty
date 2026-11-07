package main

import (
	"math"
	"strings"
	"testing"

	"github.com/riyne/fifty-fifty/internal/receipts"
	"github.com/riyne/fifty-fifty/internal/receipts/synth"
)

func p(kind synth.Kind, v string) synth.Planted { return synth.Planted{Kind: kind, Value: v} }

func TestLeaked_CardsAndPhonesByDigitRuns(t *testing.T) {
	card := p(synth.Card, "5502 7965 8099 2206")
	phone := p(synth.Phone, "(604) 555-0142")
	for _, tc := range []struct {
		name     string
		planted  synth.Planted
		redacted string
		want     bool
	}{
		{"fully replaced", card, "CARD [CARD] end", false},
		{"unrelated digits", card, "TOTAL 268.63 Table 25", false},
		{"six digits survive", card, "x 796580 y", true},
		{"six digits survive with a separator inside", card, "x 7965 80 y", true}, // separators are ignored: it is still readable
		{"five digits are not enough", card, "x 79658 y", false},
		{"the whole number survives", card, "CARD 5502 7965 8099 2206", true},
		{"phone replaced", phone, "Tel [PHONE]", false},
		{"phone with a digit dropped by OCR", phone, "Tel 604 555 142", true}, // 604555 is still readable
		{"phone, only the area code", phone, "Tel 604", false},
		{"phone, area code and exchange", phone, "Tel (604) 555", true},
		{"nothing left", phone, "", false},
	} {
		if got := leaked(tc.planted, tc.redacted); got != tc.want {
			t.Errorf("%s: leaked=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLeaked_EmailsByLocalPart(t *testing.T) {
	email := p(synth.Email, "Jane.Doe12@example.com")
	for _, tc := range []struct {
		name     string
		redacted string
		want     bool
	}{
		{"replaced", "Receipt sent to\n[EMAIL]", false},
		{"only the domain survives", "sent to example.com", false}, // not counted: stated in the leak rule
		{"whole address survives", "jane.doe12@example.com", true},
		{"upper case survives", "JANE.DOE12@EXAMPLE.COM", true},
		{"address split by a space", "jane.doe12@ example.com", true},
		{"a five-character piece of the local part", "x jane. y", true},
		{"four characters are not enough", "x jane y", false},
		{"unrelated text", "Server Dana", false},
	} {
		if got := leaked(email, tc.redacted); got != tc.want {
			t.Errorf("%s: leaked=%v, want %v", tc.name, got, tc.want)
		}
	}
	// A local part shorter than the run counts only whole.
	short := p(synth.Email, "a.k1@example.org")
	if !leaked(short, "x a.k1 y") || leaked(short, "x a.k y") || leaked(short, "") {
		t.Error("short local part rule")
	}
}

func TestHasRun(t *testing.T) {
	for _, tc := range []struct {
		needle, hay string
		n           int
		want        bool
	}{
		{"", "anything", 3, false},
		{"ab", "xxabxx", 3, true}, // shorter than the run: whole
		{"ab", "xxaxbx", 3, false},
		{"abcdef", "xxcdexx", 3, true},
		{"abcdef", "xxcdxx", 3, false},
		{"abcdef", "abcdef", 6, true},
		{"abcdef", "abcdeg", 6, false},
	} {
		if got := hasRun(tc.needle, tc.hay, tc.n); got != tc.want {
			t.Errorf("hasRun(%q, %q, %d) = %v", tc.needle, tc.hay, tc.n, got)
		}
	}
}

func TestReadExactly_CollapsesWhitespace(t *testing.T) {
	v := p(synth.Phone, "+1 604 555 0142")
	if !readExactly(v, "Tel   +1 604\n555 0142 ok") || readExactly(v, "Tel +1 604 555 142") {
		t.Error("readExactly")
	}
}

func TestWilson(t *testing.T) {
	near := func(got, want float64) bool { return math.Abs(got-want) < 0.0005 }
	for _, tc := range []struct {
		k, n   int
		lo, hi float64
	}{
		{0, 10, 0, 0.2775},
		{10, 10, 0.7225, 1},
		{50, 100, 0.4038, 0.5962},
		{0, 0, 0, 1},
		{990, 1000, 0.9818, 0.9946},
	} {
		lo, hi := Wilson(tc.k, tc.n)
		if lo < 0 || hi > 1 || lo > hi {
			t.Errorf("Wilson(%d, %d) = [%v, %v] is outside [0, 1]", tc.k, tc.n, lo, hi)
		}
		if !near(lo, tc.lo) || !near(hi, tc.hi) {
			t.Errorf("Wilson(%d, %d) = [%.4f, %.4f], want [%.4f, %.4f]", tc.k, tc.n, lo, hi, tc.lo, tc.hi)
		}
	}
}

func tallies() (map[synth.Kind]*kindTally, *kindTally) {
	return map[synth.Kind]*kindTally{synth.Card: {}, synth.Email: {}, synth.Phone: {}}, &kindTally{}
}

func TestEvaluate_CountsLeaksAndTheirCause(t *testing.T) {
	rc := synth.Receipt{Ref: 7, Planted: []synth.Planted{
		p(synth.Phone, "(604) 555-0142"),     // read intact, caught
		p(synth.Card, "5502 7965 8099 2206"), // garbled by "OCR" so the pattern misses it, most digits readable
		p(synth.Email, "jane.doe12@example.com"),
	}}
	truth := "Tel (604) 555-0142\nCARD 5502 7965 8099 2206\nsent to jane.doe12@example.com\n"
	text := "Tel (604) 555-0142\nCARD 5502 7965 8O99 2206\nsent to jane.doe12@example.com\n"
	per, overall := tallies()
	var ex []example
	evaluate(rc, truth, text, per, overall, &ex, 10)

	if c := per[synth.Phone]; c.Planted != 1 || c.Leaked != 0 || c.ReadExactly != 1 || c.Replacements != 1 {
		t.Errorf("phone %+v", c)
	}
	// "5502 7965 8O99 2206" has a letter O: the card pattern needs digits and separators
	// only, so it does not match; "55027965" survives.
	if c := per[synth.Card]; c.Planted != 1 || c.Leaked != 1 || c.ReadExactly != 0 || c.LeakedWhenGarbled != 1 || c.LeakedWhenReadExactly != 0 || c.Replacements != 0 {
		t.Errorf("card %+v", c)
	}
	if c := per[synth.Email]; c.Planted != 1 || c.Leaked != 0 || c.Replacements != 1 {
		t.Errorf("email %+v", c)
	}
	if overall.Planted != 3 || overall.Leaked != 1 || overall.Replacements != 2 {
		t.Errorf("overall %+v", overall)
	}
	if len(ex) != 1 || ex[0].Ref != 7 || ex[0].Kind != "card" || ex[0].ReadExactly || !strings.Contains(ex[0].RedactedLine, "5502") {
		t.Errorf("examples %+v", ex)
	}

	// A value read intact that the pattern misses is counted as a pattern miss, not as
	// OCR's: a layout the phone pattern was not written for.
	rc2 := synth.Receipt{Ref: 8, Planted: []synth.Planted{p(synth.Phone, "604 . 555 . 0142")}}
	per, overall = tallies()
	ex = nil
	evaluate(rc2, "Tel 604 . 555 . 0142", "Tel 604 . 555 . 0142", per, overall, &ex, 10)
	if c := per[synth.Phone]; c.Leaked != 1 || c.ReadExactly != 1 || c.LeakedWhenReadExactly != 1 || c.LeakedWhenGarbled != 0 {
		t.Errorf("phone in a layout the pattern misses: %+v", c)
	}
}

func TestEvaluate_ExampleLimit(t *testing.T) {
	rc := synth.Receipt{Ref: 1, Planted: []synth.Planted{p(synth.Card, "5502796580992206"), p(synth.Card, "4111222233334444")}}
	per, overall := tallies()
	var ex []example
	// Neither value is redacted if the text holds them with a letter in the middle.
	evaluate(rc, "5502796580992206 4111222233334444", "5502796O80992206 4111222Z33334444", per, overall, &ex, 1)
	if per[synth.Card].Leaked != 2 || len(ex) != 1 {
		t.Errorf("leaked %d, %d examples kept, want 2 and 1", per[synth.Card].Leaked, len(ex))
	}
}

func TestFinish_RecallAndInterval(t *testing.T) {
	k := &kindTally{Planted: 200, Leaked: 4}
	k.finish()
	if math.Abs(k.Recall-0.98) > 1e-9 || !(k.RecallLow95 < 0.98 && 0.98 < k.RecallHigh95) {
		t.Errorf("%+v", k)
	}
	// Values lost by OCR are not leaks: they stay in the denominator and count as protected.
	lost := &kindTally{Planted: 200, Leaked: 4, LostByOCR: 10}
	lost.finish()
	if math.Abs(lost.Recall-0.98) > 1e-9 {
		t.Errorf("recall with lost values: %v", lost.Recall)
	}
	empty := &kindTally{}
	empty.finish()
	if empty.Recall != 0 || empty.RecallLow95 != 0 || empty.RecallHigh95 != 1 {
		t.Errorf("no data: %+v", empty)
	}
}

func TestLongestCommonSubsequence(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"", "abc", 0}, {"abc", "abc", 3}, {"abc", "xaybzc", 3}, {"abc", "cba", 1}, {"14155550195", "1415555195", 10}, {"14155550195", "4155550189", 9},
	} {
		if got := longestCommonSubsequence(tc.a, tc.b); got != tc.want {
			t.Errorf("longestCommonSubsequence(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestLongestCommonRun(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"", "abc", 0}, {"abc", "", 0}, {"abc", "xyz", 0}, {"abcdef", "xxcdexx", 3}, {"6045550142", "604555142", 6},
		{"abab", "baba", 3}, {"aaaa", "aa", 2}, {"abc", "abc", 3},
	} {
		if got := longestCommonRun(tc.a, tc.b); got != tc.want {
			t.Errorf("longestCommonRun(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// Two numbers on one receipt share most of their digits (same area code, same 555-01xx
// block). A leftover of one must not be counted against the other.
func TestEvaluate_ANeighbouringNumbersLeftoverIsNotThisNumbersLeak(t *testing.T) {
	rc := synth.Receipt{Ref: 1, Planted: []synth.Planted{
		p(synth.Phone, "4155550189"),      // printed on the Tel line, read intact, redacted
		p(synth.Phone, "+1 415 555 0195"), // printed on the Customer line, 0 read as @
	}}
	truth := "Tel 4155550189\nCustomer +1 415 555 0195\n"
	text := "Tel 4155550189\nCustomer +1 415 555 @195\n"
	per, overall := tallies()
	var ex []example
	evaluate(rc, truth, text, per, overall, &ex, 10)
	ph := per[synth.Phone]
	if ph.Planted != 2 || ph.Leaked != 1 || ph.LeakedWhenGarbled != 1 || ph.LeakedWhenReadExactly != 0 {
		t.Fatalf("phone %+v", ph)
	}
	if len(ex) != 1 || ex[0].Value != "+1 415 555 0195" || !strings.Contains(ex[0].RedactedLine, "Customer") {
		t.Errorf("examples %+v", ex)
	}
}

func TestEvaluate_AValueOCRDestroyedIsLostNotLeaked(t *testing.T) {
	rc := synth.Receipt{Ref: 1, Planted: []synth.Planted{
		p(synth.Card, "5502796580992206"), p(synth.Email, "jane.doe12@example.com"), p(synth.Phone, "6045550142"),
	}}
	truth := "CARD 5502796580992206\nsent to jane.doe12@example.com\nTel 6045550142\nThank you\n"
	per, overall := tallies()
	var ex []example
	// Only a couple of stray characters of each survive.
	evaluate(rc, truth, "CARD 55 0\nsent to jan\nTel 60\nThank you\n", per, overall, &ex, 10)
	for kind, c := range per {
		if c.Planted != 1 || c.LostByOCR != 1 || c.Leaked != 0 {
			t.Errorf("%s: %+v", kind, c)
		}
	}
	if overall.LostByOCR != 3 || overall.Leaked != 0 || len(ex) != 0 {
		t.Errorf("overall %+v, examples %v", overall, ex)
	}
	// A line the OCR dropped altogether is lost too.
	per, overall = tallies()
	evaluate(synth.Receipt{Planted: []synth.Planted{p(synth.Phone, "6045550142")}}, "a line\nTel 6045550142\nlast\n", "a line\nlast\n", per, overall, &ex, 10)
	if per[synth.Phone].LostByOCR != 1 {
		t.Errorf("dropped line: %+v", per[synth.Phone])
	}
	// Four digits are enough to anchor a value to its line; three are not.
	per, overall = tallies()
	evaluate(synth.Receipt{Planted: []synth.Planted{p(synth.Phone, "6045550142")}}, "Tel 6045550142", "Tel 6045", per, overall, &ex, 10)
	if per[synth.Phone].LostByOCR != 0 {
		t.Errorf("4 digits should anchor: %+v", per[synth.Phone])
	}
	per, overall = tallies()
	evaluate(synth.Receipt{Planted: []synth.Planted{p(synth.Phone, "6045550142")}}, "Tel 6045550142", "Tel 604", per, overall, &ex, 10)
	if per[synth.Phone].LostByOCR != 1 {
		t.Errorf("3 digits should not: %+v", per[synth.Phone])
	}
}

func TestAlignLines(t *testing.T) {
	truth := []string{"HARBOUR SUSHI", "Tel +1 416 555 0148", "REF 1", "------", "Banh Mi 23.32", "------", "TOTAL 378.11", "CARD 5502796580992206"}
	eq := func(got, want []int) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	for _, tc := range []struct {
		name string
		read []string
		want []int
	}{
		{"identical", truth, []int{0, 1, 2, 3, 4, 5, 6, 7}},
		{"blank lines inserted", []string{"HARBOUR SUSHI", "", "Tel +1 416 555 0148", "", "REF 1", "------", "Banh Mi 23.32", "------", "", "TOTAL 378.11", "CARD 5502796580992206"}, []int{0, 2, 4, 5, 6, 7, 9, 10}},
		{"a line dropped", []string{"HARBOUR SUSHI", "REF 1", "------", "Banh Mi 23.32", "------", "TOTAL 378.11", "CARD 5502796580992206"}, []int{0, -1, 1, 2, 3, 4, 5, 6}},
		{"garbled lines still match", []string{"HARB0UR SUSH1", "Tel +1 416 555 @148", "REF 1", "-----", "Banh Mi 2332", "------", "TOTAL 378.11", "CARD 5502796580992206"}, []int{0, 1, 2, 3, 4, 5, 6, 7}},
		{"nothing read", nil, []int{-1, -1, -1, -1, -1, -1, -1, -1}},
		{"unrelated text", []string{"zzzz qqqq", "xxxx"}, []int{-1, -1, -1, -1, -1, -1, -1, -1}},
		{"a line split in two", []string{"HARBOUR SUSHI", "Tel +1 416", "555 0148", "REF 1", "------", "Banh Mi 23.32", "------", "TOTAL 378.11", "CARD 5502796580992206"}, []int{0, 1, 3, 4, 5, 6, 7, 8}},
	} {
		got := alignLines(truth, tc.read)
		if !eq(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The scoring redacts line by line. That is only valid if it gives the same text as
// redacting everything at once, on the receipts it scores.
func TestRedactingLineByLineEqualsRedactingTheWholeText(t *testing.T) {
	for ref := 1; ref <= 400; ref++ {
		text := synth.Generate(ref).Text()
		whole, wc := receipts.Redact(text)
		var parts []string
		var total receipts.Redactions
		for _, l := range strings.Split(text, "\n") {
			r, c := receipts.Redact(l)
			parts = append(parts, r)
			total.Cards += c.Cards
			total.Emails += c.Emails
			total.Phones += c.Phones
		}
		if whole != strings.Join(parts, "\n") || wc != total {
			t.Fatalf("ref %d: line by line differs from whole text", ref)
		}
	}
}

// On the ground truth text every planted value is redacted: a regression guard for the
// patterns on this generator's formats (the benchmark reports the same number).
func TestGroundTruthTextLeaksNothing(t *testing.T) {
	per, overall := tallies()
	var ex []example
	for ref := 1; ref <= 300; ref++ {
		rc := synth.Generate(ref)
		evaluate(rc, rc.Text(), rc.Text(), per, overall, &ex, 10)
	}
	if overall.Planted < 300 || overall.Leaked != 0 || overall.LostByOCR != 0 || overall.ReadExactly != overall.Planted {
		t.Errorf("overall %+v, examples %+v", overall, ex)
	}
	// One placeholder per planted value, of the right kind: the patterns replace nothing else.
	for kind, c := range per {
		if c.Planted == 0 || c.Replacements != c.Planted {
			t.Errorf("%s: %d planted, %d placeholders", kind, c.Planted, c.Replacements)
		}
	}
	if overall.Replacements != overall.Planted {
		t.Errorf("%d placeholders for %d planted values", overall.Replacements, overall.Planted)
	}
}

// The example shows the line as it is after redaction: what a reader would see leaking.
func TestEvaluate_TheExampleLineIsTheRedactedLine(t *testing.T) {
	rc := synth.Receipt{Ref: 3, Planted: []synth.Planted{p(synth.Phone, "6045550142")}}
	truth := "Tel 6045550142 mail bob@example.com\n"
	text := "Tel 6O45550142 mail bob@example.com\n" // the O keeps the phone pattern from matching; the email is redacted
	per, overall := tallies()
	var ex []example
	evaluate(rc, truth, text, per, overall, &ex, 10)
	if len(ex) != 1 || !strings.Contains(ex[0].RedactedLine, "[EMAIL]") || strings.Contains(ex[0].RedactedLine, "bob@") {
		t.Errorf("examples %+v", ex)
	}
}

// A value is judged on the line it is first printed on.
func TestEvaluate_UsesTheFirstLineThatHoldsTheValue(t *testing.T) {
	rc := synth.Receipt{Planted: []synth.Planted{p(synth.Phone, "6045550142")}}
	truth := "Tel 6045550142\nfiller line here\nagain 6045550142\n"
	text := "Tel 6O45550142\nfiller line here\nagain [PHONE]\n" // garbled and leaking on the first line only
	per, overall := tallies()
	var ex []example
	evaluate(rc, truth, text, per, overall, &ex, 10)
	if per[synth.Phone].Leaked != 1 {
		t.Errorf("%+v", per[synth.Phone])
	}
}

func TestAlignLines_KeepsTheOrder(t *testing.T) {
	got := alignLines([]string{"alpha one two", "beta three four"}, []string{"beta three four", "alpha one two"})
	matched := 0
	for _, g := range got {
		if g >= 0 {
			matched++
		}
	}
	if matched != 1 {
		t.Errorf("alignment %v crosses: a matching must keep the order of the lines", got)
	}
}
