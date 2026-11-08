package main

import (
	"encoding/json"
	"strings"
	"testing"

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
