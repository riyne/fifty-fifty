package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/riyne/fifty-fifty/internal/receiptparse"
	"github.com/riyne/fifty-fifty/internal/receipts"
	"github.com/riyne/fifty-fifty/internal/receipts/synth"
)

// answerOf builds a model answer from a receipt, letting a test damage it.
type ansItem struct {
	Description string `json:"description"`
	Quantity    int    `json:"quantity,omitempty"`
	LineTotal   int64  `json:"line_total_minor"`
}
type ans struct {
	Items []ansItem `json:"items"`
	Tax   int64     `json:"tax_minor,omitempty"`
	Tip   int64     `json:"tip_minor,omitempty"`
	Total int64     `json:"total_minor"`
}

func truthAnswer(r synth.Receipt) ans {
	a := ans{Tax: r.TaxMinor, Tip: r.TipMinor, Total: r.TotalMinor}
	for _, l := range r.Lines {
		it := ansItem{Description: l.Description, LineTotal: l.TotalMinor}
		if l.Quantity > 1 {
			it.Quantity = l.Quantity
		}
		a.Items = append(a.Items, it)
	}
	return a
}

func marshal(t *testing.T, a ans) []byte {
	t.Helper()
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// receiptWith finds a synthetic receipt with the properties a test needs.
func receiptWith(t *testing.T, ok func(synth.Receipt) bool) synth.Receipt {
	t.Helper()
	for ref := 5001; ref < 6000; ref++ {
		if r := synth.Generate(ref); ok(r) {
			return r
		}
	}
	t.Fatal("no receipt with the required properties")
	return synth.Receipt{}
}

func hasQuantity(r synth.Receipt) bool {
	for _, l := range r.Lines {
		if l.Quantity > 1 {
			return true
		}
	}
	return false
}

func TestScore_PerfectAnswerIsExact(t *testing.T) {
	for ref := 5001; ref < 5040; ref++ {
		r := synth.Generate(ref)
		s := scoreAnswer(r, []byte(r.Answer()))
		if !s.Parsed || !s.AmountsExact || !s.Exact || !s.TotalOK || s.MatchedStrict != s.TruthUnits || s.ReadUnits != s.TruthUnits {
			t.Fatalf("ref %d: the ground-truth answer is not exact: %+v", ref, s)
		}
	}
}

func TestScore_WhatCountsAsWrong(t *testing.T) {
	r := receiptWith(t, func(r synth.Receipt) bool { return len(r.Lines) >= 3 && r.TaxMinor > 0 && r.TipMinor > 0 })
	cases := []struct {
		name                       string
		damage                     func(a *ans)
		amounts, exact, parsed     bool
		readUnitsDelta             int
		wantTotalOK, wantMatchedLT bool // MatchedAmount below TruthUnits
	}{
		{"a misspelled description is not a money error", func(a *ans) { a.Items[0].Description = strings.ToUpper(a.Items[0].Description) + "!" }, true, false, true, 0, true, false},
		{"case and spacing in a description are ignored", func(a *ans) { a.Items[0].Description = "  " + strings.ToUpper(a.Items[0].Description) + "  " }, true, true, true, 0, true, false},
		{"one price wrong, total as printed", func(a *ans) { a.Items[0].LineTotal += 100 }, false, false, true, 0, true, true},
		{"price and total wrong together (consistent misread)", func(a *ans) { a.Items[0].LineTotal += 100; a.Total += 100 }, false, false, true, 0, false, true},
		{"an item missing", func(a *ans) { a.Items = a.Items[1:] }, false, false, true, -1, true, true},
		{"an item invented", func(a *ans) { a.Items = append(a.Items, ansItem{Description: "Ghost", LineTotal: 100}) }, false, false, true, 1, true, false},
		{"tax wrong", func(a *ans) { a.Tax++ }, false, false, true, 0, true, false},
		{"tip wrong", func(a *ans) { a.Tip++ }, false, false, true, 0, true, false},
		{"only the printed total wrong: prices are right", func(a *ans) { a.Total += 5 }, true, false, true, 0, false, false},
	}
	for _, c := range cases {
		a := truthAnswer(r)
		c.damage(&a)
		s := scoreAnswer(r, marshal(t, a))
		if s.Parsed != c.parsed || s.AmountsExact != c.amounts || s.Exact != c.exact || s.TotalOK != c.wantTotalOK {
			t.Errorf("%s: %+v", c.name, s)
		}
		if got := s.ReadUnits - s.TruthUnits; got != c.readUnitsDelta {
			t.Errorf("%s: read %d units, truth %d", c.name, s.ReadUnits, s.TruthUnits)
		}
		if (s.MatchedAmount < s.TruthUnits) != c.wantMatchedLT {
			t.Errorf("%s: matched %d of %d", c.name, s.MatchedAmount, s.TruthUnits)
		}
	}
}

func TestScore_AQuantityLineAndSeparateLinesAreTheSameMoney(t *testing.T) {
	r := receiptWith(t, hasQuantity)
	a := truthAnswer(r)
	var split []ansItem
	for _, it := range a.Items {
		if it.Quantity > 1 {
			for i := 0; i < it.Quantity; i++ {
				split = append(split, ansItem{Description: it.Description, LineTotal: it.LineTotal / int64(it.Quantity)})
			}
			continue
		}
		split = append(split, it)
	}
	a.Items = split
	s := scoreAnswer(r, marshal(t, a))
	if !s.AmountsExact || !s.Exact {
		t.Errorf("the same units listed as separate lines must score as right: %+v", s)
	}
}

func TestScore_UnusableAnswers(t *testing.T) {
	r := synth.Generate(5001)
	for name, raw := range map[string]string{
		"not json": "I could not read this.", "empty": "", "extra field": `{"items":[{"description":"a","line_total_minor":1}],"total_minor":1,"confidence":0.9}`,
		"no items": `{"items":[],"total_minor":0}`, "decimal price": `{"items":[{"description":"a","line_total_minor":12.5}],"total_minor":13}`,
	} {
		s := scoreAnswer(r, []byte(raw))
		if s.Parsed || s.AmountsExact || s.Exact || s.ReadUnits != 0 {
			t.Errorf("%s: %+v", name, s)
		}
		if s.TruthUnits == 0 {
			t.Errorf("%s: the truth must still be counted", name)
		}
	}
}

func rec(status string, amountsExact bool) record {
	return record{Attempts: 1, Status: status, Score: score{Parsed: true, AmountsExact: amountsExact, Exact: amountsExact, TruthUnits: 4, ReadUnits: 4, MatchedStrict: 4, MatchedAmount: 4, TotalOK: true}}
}

func TestSummarize_TheFourCombinationsAndFailures(t *testing.T) {
	recs := []record{
		rec(receipts.StatusClaiming, true), rec(receipts.StatusClaiming, true), // accepted, correct
		rec(receipts.StatusClaiming, false),                                            // silent error
		rec(receipts.StatusNeedsReview, false), rec(receipts.StatusNeedsReview, false), // caught
		rec(receipts.StatusNeedsReview, true),                // false alarm
		{Attempts: 4, CallError: "after 4 attempt(s): boom"}, // a failed call is counted apart
		{CallError: "ocr failed"},
	}
	tl := summarize(recs)
	if tl.Calls != 6 || tl.CallsFailed != 2 || tl.Retried != 1 {
		t.Fatalf("calls %+v", tl)
	}
	if tl.AcceptedCorrect != 2 || tl.AcceptedWrong != 1 || tl.FlaggedWrong != 2 || tl.FlaggedCorrect != 1 {
		t.Fatalf("combinations %+v", tl)
	}
	if tl.AmountsExact.K != 3 || tl.AmountsExact.N != 6 {
		t.Errorf("amounts exact %+v", tl.AmountsExact)
	}
	// 3 wrong parses, 2 of them flagged.
	if tl.CatchRate.K != 2 || tl.CatchRate.N != 3 {
		t.Errorf("catch rate %+v", tl.CatchRate)
	}
	if tl.SilentErrorRate.K != 1 || tl.SilentErrorRate.N != 6 {
		t.Errorf("silent error rate %+v", tl.SilentErrorRate)
	}
	if tl.FalseAlarmRate.K != 1 || tl.FalseAlarmRate.N != 3 {
		t.Errorf("false alarm rate %+v", tl.FalseAlarmRate)
	}
}

func TestWilson(t *testing.T) {
	lo, hi := Wilson(50, 50)
	if hi < 0.999999 || lo < 0.92 || lo > 0.93 {
		t.Errorf("50/50: [%v, %v]", lo, hi)
	}
	lo, hi = Wilson(0, 50)
	if lo != 0 || hi < 0.07 || hi > 0.08 {
		t.Errorf("0/50: [%v, %v]", lo, hi)
	}
	if lo, hi = Wilson(0, 0); lo != 0 || hi != 1 {
		t.Errorf("0/0: [%v, %v]", lo, hi)
	}
}

func TestNearestRank(t *testing.T) {
	s := []int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	if nearestRank(s, 0.5) != 50 || nearestRank(s, 0.99) != 100 || nearestRank(nil, 0.5) != 0 {
		t.Errorf("%d %d", nearestRank(s, 0.5), nearestRank(s, 0.99))
	}
}

func TestClassify_WhatKindOfWrong(t *testing.T) {
	r := receiptWith(t, func(r synth.Receipt) bool { return hasQuantity(r) && r.TaxMinor > 0 && len(r.Lines) >= 3 })
	cases := []struct {
		name   string
		damage func(a *ans)
		want   string
	}{
		{"a quantity dropped, the line total kept", func(a *ans) {
			for i := range a.Items {
				if a.Items[i].Quantity > 1 {
					a.Items[i].Quantity = 0
					return
				}
			}
		}, kindQuantityOnly},
		{"a quantity changed, the line total kept", func(a *ans) {
			for i := range a.Items {
				if a.Items[i].Quantity > 1 {
					a.Items[i].Quantity++
					return
				}
			}
		}, kindQuantityOnly},
		{"a line total wrong", func(a *ans) { a.Items[0].LineTotal += 100 }, kindAmounts},
		{"two line totals wrong by opposite amounts (sum kept)", func(a *ans) { a.Items[0].LineTotal += 100; a.Items[1].LineTotal -= 100 }, kindAmounts},
		{"tax wrong", func(a *ans) { a.Tax++ }, kindAmounts},
		{"an item missing", func(a *ans) { a.Items = a.Items[1:] }, kindAmounts},
		{"an item invented", func(a *ans) { a.Items = append(a.Items, ansItem{Description: "Ghost", LineTotal: 100}) }, kindAmounts},
		{"a line split in two with the same total", func(a *ans) {
			it := a.Items[0]
			a.Items[0].LineTotal = it.LineTotal / 2
			a.Items = append(a.Items, ansItem{Description: it.Description, LineTotal: it.LineTotal - it.LineTotal/2})
		}, kindAmounts},
	}
	for _, c := range cases {
		a := truthAnswer(r)
		c.damage(&a)
		if got := classify(r, marshal(t, a)); got != c.want {
			t.Errorf("%s: classified %q, want %q", c.name, got, c.want)
		}
	}
	if got := classify(r, []byte("I cannot read this")); got != kindUnparsed {
		t.Errorf("not json: %q", got)
	}
	if got := classify(r, nil); got != kindUnparsed {
		t.Errorf("no answer: %q", got)
	}
}

func TestParseAmount_OnlyWholeAmountTokensCount(t *testing.T) {
	good := map[string]int64{"36.49": 3649, "0.05": 5, "1,234.50": 123450, "12,345,678.00": 1234567800, "100.00": 10000}
	for tok, want := range good {
		if got, ok := parseAmount(tok); !ok || got != want {
			t.Errorf("parseAmount(%q) = %d, %v; want %d", tok, got, ok, want)
		}
	}
	for _, tok := range []string{"", "B8.77", "8.7", "8.777", "12", ".50", "1,23.00", "1234,567.00", "12,34.5", "x2", "36.4O", "-5.00", "+5.00", "--", "36.49.", "3 6.49", "1,2345.00", "1234567890123.00"} {
		if got, ok := parseAmount(tok); ok {
			t.Errorf("parseAmount(%q) = %d, true; want not an amount", tok, got)
		}
	}
}

func TestPrintedAmounts_TokensAndPunctuation(t *testing.T) {
	text := "Spring Rolls : 36.49\n2 Pizza:48.02\nSide ($5.00)\nB8.77 and 8.7 and 9.999\nTax 1,234.50, Tip 3.00.\n*7.25*\n"
	got := printedAmounts(text)
	for _, v := range []int64{3649, 4802, 500, 123450, 300, 725} {
		if !got[v] {
			t.Errorf("amount %d is printed and was not found (found %v)", v, got)
		}
	}
	for _, v := range []int64{877, 87, 8, 870, 999, 9999} {
		if got[v] {
			t.Errorf("amount %d is not printed as a whole amount token but was found", v)
		}
	}
}

// Whatever a perfect reader sends, a perfect answer contains no amount that is not printed: the
// metric cannot fire on the faithful case, on the text the worker would send (redacted).
func TestAmountsNotInText_AFaithfulAnswerHasNone(t *testing.T) {
	for ref := 5001; ref < 7000; ref++ {
		r := synth.Generate(ref)
		text, _ := receiptparse.PrepareText(r.Text(), 0)
		if m := amountsNotInText(text, []byte(r.Answer())); len(m) != 0 {
			t.Fatalf("ref %d: a faithful answer has amounts not in the text: %v\n%s", ref, m, text)
		}
	}
}

// The behavior the metric exists for: a price the text does not contain, whether the model
// guessed it, computed it from the subtotal or invented it.
func TestAmountsNotInText_CountsWhatIsSuppliedNotWhatIsRead(t *testing.T) {
	r := receiptWith(t, func(r synth.Receipt) bool { return r.TaxMinor > 0 && r.TipMinor > 0 && len(r.Lines) >= 3 })
	text := r.Text()
	price := fmt.Sprintf("%d.%02d", r.Lines[1].TotalMinor/100, r.Lines[1].TotalMinor%100)
	if strings.Count(text, price) != 1 {
		t.Skipf("price %s is printed more than once; pick another receipt", price)
	}
	garbled := strings.Replace(text, price, "B"+price[1:], 1)
	full := truthAnswer(r)

	// 1. The model reads the garbled price correctly from context: counted (it was not printed).
	if m := amountsNotInText(garbled, marshal(t, full)); len(m) != 1 || m[0] != r.Lines[1].TotalMinor {
		t.Errorf("a guessed price: %v, want exactly [%d]", m, r.Lines[1].TotalMinor)
	}
	// 2. It leaves the line out, as the v2 prompt asks: nothing is supplied, nothing counted.
	left := truthAnswer(r)
	left.Items = append(append([]ansItem{}, left.Items[:1]...), left.Items[2:]...)
	if m := amountsNotInText(garbled, marshal(t, left)); len(m) != 0 {
		t.Errorf("a line left out: %v, want none", m)
	}
	// 3. It back-solves a different value to make the sum match: counted.
	bs := truthAnswer(r)
	bs.Items[1].LineTotal += 3000
	if m := amountsNotInText(garbled, marshal(t, bs)); len(m) != 1 || m[0] != r.Lines[1].TotalMinor+3000 {
		t.Errorf("a back-solved price: %v", m)
	}
	// 4. An unreadable printed total filled with the sum: counted, like a price.
	noTotal := regexpTotalLine.ReplaceAllString(text, "TOTAL : ???")
	if m := amountsNotInText(noTotal, marshal(t, full)); len(m) != 1 || m[0] != r.TotalMinor {
		t.Errorf("an unreadable total filled in: %v, want [%d]", m, r.TotalMinor)
	}
	// 4b. The same for an unreadable tax and an unreadable tip (when the value is printed
	// nowhere else, so removing the line really removes it from the text).
	for _, c := range []struct {
		name string
		re   *regexp.Regexp
		v    int64
	}{{"tax", regexp.MustCompile(`(?m)^Tax.*$`), r.TaxMinor}, {"tip", regexp.MustCompile(`(?m)^Tip.*$`), r.TipMinor}} {
		if c.v == 0 {
			continue
		}
		cut := c.re.ReplaceAllString(text, c.name+" : ???")
		if printedAmounts(cut)[c.v] {
			continue // the value is also printed on another line of this receipt
		}
		if m := amountsNotInText(cut, marshal(t, full)); len(m) != 1 || m[0] != c.v {
			t.Errorf("an unreadable %s filled in: %v, want [%d]", c.name, m, c.v)
		}
	}
	// 5. Documented limit: a printed amount used where another belongs (the subtotal as a line
	// price) is not counted, because the text does contain it.
	sub := truthAnswer(r)
	var subtotal int64
	for _, l := range r.Lines {
		subtotal += l.TotalMinor
	}
	sub.Items[1].LineTotal = subtotal
	if m := amountsNotInText(text, marshal(t, sub)); len(m) != 0 {
		t.Errorf("a wrong but printed amount was counted: %v", m)
	}
	// 6. Not an answer: nothing to count.
	if m := amountsNotInText(text, []byte("not json")); m != nil {
		t.Errorf("an unusable answer: %v", m)
	}
}

var regexpTotalLine = regexp.MustCompile(`(?m)^TOTAL.*$`)

func TestSummarize_AmountNotInTextCounts(t *testing.T) {
	mk := func(status string, exact, parsed bool, amounts ...int64) record {
		r := rec(status, exact)
		r.Score.Parsed = parsed
		r.AmountsNotInText = amounts
		return r
	}
	got := summarize([]record{
		mk(receipts.StatusClaiming, false, true, 100),         // accepted, wrong, has one
		mk(receipts.StatusNeedsReview, false, true, 200, 300), // flagged, wrong, has two
		mk(receipts.StatusNeedsReview, true, true, 400),       // flagged, correct, has one (a correct guess)
		mk(receipts.StatusClaiming, true, true),               // clean
		mk(receipts.StatusNeedsReview, false, false),          // not valid for the schema: not in the denominator
	})
	if got.AmountNotInText.K != 3 || got.AmountNotInText.N != 4 {
		t.Errorf("rate %d/%d, want 3/4", got.AmountNotInText.K, got.AmountNotInText.N)
	}
	if got.AmountNotInTextAccepted != 1 || got.AmountNotInTextWrong != 2 || got.AmountsNotInTextTotal != 4 {
		t.Errorf("accepted %d wrong %d total %d, want 1, 2, 4", got.AmountNotInTextAccepted, got.AmountNotInTextWrong, got.AmountsNotInTextTotal)
	}
}
