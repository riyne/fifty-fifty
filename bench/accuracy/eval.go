package main

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/riyne/fifty-fifty/internal/receipts"
	"github.com/riyne/fifty-fifty/internal/receipts/synth"
)

// What "correct" means here, because the whole point of the evaluation is the difference
// between a wrong answer and a wrong answer the system lets through.
//
// Everything is compared at the level the system stores: claimable units. A line "3 Beer
// 30.00" becomes three units (receipts.ExpandQuantity), on both sides, so a model that
// lists the same beer as one line of three or as three lines of one is not penalized.
//
//   - AmountsExact: the multiset of unit prices read equals the multiset on the receipt
//     (same count, same prices), and tax and tip are equal. This is what decides whether
//     the balances come out right once people claim the units, so a receipt that is not
//     AmountsExact is a WRONG parse. A misspelled description does not make it wrong.
//   - Exact: AmountsExact, and every unit's description (its line's) equals the printed
//     one (compared case-insensitively with runs of whitespace collapsed), and the
//     printed total was read right.
//
// What the system did with the answer is the status receipts.Interpret chose: claiming
// (the answer is valid and items + tax + tip equal the total it printed: accepted) or
// needs_review (a person must look: flagged). The four combinations with correctness are
// the result that matters: a wrong parse that is accepted is a silent error, and the
// review flag exists to make those rare.

// score is how one answer compares with the receipt's ground truth.
type score struct {
	Parsed        bool // the answer passed receipts.ParseExtraction
	TruthUnits    int
	ReadUnits     int
	MatchedStrict int // units matched on description and price
	MatchedAmount int // units matched on price alone
	TaxOK         bool
	TipOK         bool
	TotalOK       bool // the printed total was read right
	AmountsExact  bool
	Exact         bool
}

// unit is one claimable unit: its line's description and its price.
type unit struct {
	Description string
	PriceMinor  int64
}

// scoreAnswer compares a model's raw answer with the ground truth of a receipt.
func scoreAnswer(truth synth.Receipt, raw []byte) score {
	var s score
	truthUnits, _ := unitsOf(truthLines(truth))
	s.TruthUnits = len(truthUnits)
	ex, err := receipts.ParseExtraction(raw)
	if err != nil {
		return s
	}
	s.Parsed = true
	s.TaxOK, s.TipOK, s.TotalOK = ex.TaxMinor == truth.TaxMinor, ex.TipMinor == truth.TipMinor, ex.TotalMinor == truth.TotalMinor
	read, ok := unitsOf(ex.Lines)
	if !ok {
		return s // too many units to expand: the pipeline refuses it too, nothing to compare
	}
	s.ReadUnits = len(read)
	s.MatchedStrict = intersect(keys(truthUnits, true), keys(read, true))
	s.MatchedAmount = intersect(keys(truthUnits, false), keys(read, false))
	s.AmountsExact = s.ReadUnits == s.TruthUnits && s.MatchedAmount == s.TruthUnits && s.TaxOK && s.TipOK
	s.Exact = s.AmountsExact && s.MatchedStrict == s.TruthUnits && s.TotalOK
	return s
}

func truthLines(r synth.Receipt) []receipts.Line {
	out := make([]receipts.Line, len(r.Lines))
	for i, l := range r.Lines {
		out[i] = receipts.Line{Description: l.Description, Quantity: l.Quantity, LineTotalMinor: l.TotalMinor}
	}
	return out
}

// unitsOf turns lines into claimable units, with the prices the system would store
// (receipts.ExpandQuantity) and each line's own description. ok is false if a line
// cannot be expanded.
func unitsOf(lines []receipts.Line) (units []unit, ok bool) {
	for _, l := range lines {
		items, err := receipts.ExpandQuantity(l.Description, l.Quantity, l.LineTotalMinor)
		if err != nil {
			return nil, false
		}
		for _, it := range items {
			units = append(units, unit{Description: l.Description, PriceMinor: it.PriceMinor})
		}
	}
	return units, true
}

func keys(units []unit, withDescription bool) map[string]int {
	m := map[string]int{}
	for _, u := range units {
		k := strconv.FormatInt(u.PriceMinor, 10)
		if withDescription {
			k = normalize(u.Description) + "\x00" + k
		}
		m[k]++
	}
	return m
}

func normalize(s string) string {
	return strings.ToLower(strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " "))
}

// intersect is the size of the multiset intersection.
func intersect(a, b map[string]int) int {
	n := 0
	for k, x := range a {
		n += min(x, b[k])
	}
	return n
}

// The back-solving metric. The first recording found that, when a price was garbled and
// a printed subtotal was visible, the model sometimes filled the gap so that the numbers added
// up, which defeats the arithmetic check. A silent error is too rare to show whether an
// instruction removes that (3 in 250), so this counts the behavior itself: an amount in the
// answer (a line total, the tax, the tip or the total) that is not a number printed in the text
// that was sent. It is computed from the text sent and the answer, never from the ground truth.
//
// "Printed" means a whole token of the text, split at white space and colons, that is an
// amount: digits, a point, exactly two decimals, optionally thousands commas, and with the
// punctuation around it ($, brackets, a trailing comma or point) removed. "B8.77" is not an
// amount token (a letter is stuck to it), so a model that reads it as 8.77 has supplied a price
// the text does not contain. The synthetic receipts print every amount this way, so a model
// that reads what is there is never counted. What it does and does not mean:
//   - it counts amounts the text does not contain, whatever the reason: a model that guesses a
//     garbled price (even correctly, from context) is counted, and so is one that mistypes a
//     clean token (copies 36.49 as 36.94), because 3694 is not in the text;
//   - an amount that IS in the text but is the wrong one (the subtotal used as the total, a price
//     taken from the next line) is not counted: nothing here can see it.
func amountsNotInText(text string, raw []byte) []int64 {
	ex, err := receipts.ParseExtraction(raw)
	if err != nil {
		return nil
	}
	printed := printedAmounts(text)
	var missing []int64
	check := func(v int64) {
		if !printed[v] {
			missing = append(missing, v)
		}
	}
	for _, l := range ex.Lines {
		check(l.LineTotalMinor)
	}
	// Tax and tip are optional (0 means the receipt has none); the total is the printed one.
	for _, v := range []int64{ex.TaxMinor, ex.TipMinor, ex.TotalMinor} {
		if v > 0 {
			check(v)
		}
	}
	return missing
}

// printedAmounts is the set of amounts (minor units) that appear as whole tokens in text.
func printedAmounts(text string) map[int64]bool {
	out := map[int64]bool{}
	for _, tok := range strings.FieldsFunc(text, func(r rune) bool { return unicode.IsSpace(r) || r == ':' }) {
		tok = strings.TrimLeft(tok, "$([*'\"")
		tok = strings.TrimRight(tok, ")]*,;.'\"")
		if v, ok := parseAmount(tok); ok {
			out[v] = true
		}
	}
	return out
}

// parseAmount reads "12.50" or "1,234.50" as minor units. Anything else is not an amount.
func parseAmount(tok string) (int64, bool) {
	dot := strings.LastIndexByte(tok, '.')
	if dot < 1 || len(tok)-dot != 3 {
		return 0, false
	}
	whole, cents := tok[:dot], tok[dot+1:]
	if strings.Contains(whole, ",") {
		parts := strings.Split(whole, ",")
		if len(parts[0]) < 1 || len(parts[0]) > 3 {
			return 0, false
		}
		for _, p := range parts[1:] {
			if len(p) != 3 {
				return 0, false
			}
		}
		whole = strings.Join(parts, "")
	}
	if len(whole) > 12 {
		return 0, false
	}
	for _, r := range whole + cents {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	w, err1 := strconv.ParseInt(whole, 10, 64)
	c, err2 := strconv.ParseInt(cents, 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return w*100 + c, true
}

// Wilson returns the 95% Wilson score interval for k successes in n trials.
func Wilson(k, n int) (lo, hi float64) {
	if n == 0 {
		return 0, 1
	}
	const z = 1.959964
	p := float64(k) / float64(n)
	nf := float64(n)
	den := 1 + z*z/nf
	centre := (p + z*z/(2*nf)) / den
	half := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / den
	lo, hi = centre-half, centre+half
	return math.Max(lo, 0), math.Min(hi, 1)
}

// rate is a count out of a total with its Wilson interval.
type rate struct {
	K    int     `json:"k"`
	N    int     `json:"n"`
	P    float64 `json:"p"`
	Low  float64 `json:"wilson95_low"`
	High float64 `json:"wilson95_high"`
}

func newRate(k, n int) rate {
	r := rate{K: k, N: n}
	if n > 0 {
		r.P = float64(k) / float64(n)
	}
	r.Low, r.High = Wilson(k, n)
	return r
}

// tally is one variant's result over the calls that got an answer.
type tally struct {
	Calls        int  `json:"calls_answered"`
	CallsFailed  int  `json:"calls_failed"`
	Retried      int  `json:"calls_that_needed_a_retry"`
	NotParsed    int  `json:"answers_not_valid_json_of_the_schema"`
	AmountsExact rate `json:"amounts_exact"`
	Exact        rate `json:"exact"`
	TotalRead    rate `json:"printed_total_read_right"`

	// The four combinations of what the system did and whether the parse was right.
	AcceptedCorrect int `json:"accepted_and_correct"`
	AcceptedWrong   int `json:"accepted_and_wrong_silent_errors"`
	FlaggedCorrect  int `json:"flagged_but_correct_false_alarms"`
	FlaggedWrong    int `json:"flagged_and_wrong_caught"`
	// SilentErrorRate: accepted and wrong, out of all answered receipts.
	SilentErrorRate rate `json:"silent_error_rate"`
	// CatchRate: of the wrong parses, how many were flagged for a person to look at.
	CatchRate rate `json:"review_flag_catch_rate"`
	// FalseAlarmRate: of the correct parses, how many were flagged anyway.
	FalseAlarmRate rate `json:"review_flag_false_alarm_rate"`

	UnitPrecision float64 `json:"unit_precision_strict"`
	UnitRecall    float64 `json:"unit_recall_strict"`
	AmountPrec    float64 `json:"unit_precision_amount"`
	AmountRecall  float64 `json:"unit_recall_amount"`

	// The back-solving metric (see amountsNotInText): of the answers that were valid for the
	// schema, how many contain an amount that is not printed in the text sent, how many of those
	// the system accepted (the dangerous ones: the review flag cannot see an amount it was
	// handed consistently) and how many were wrong anyway. Zero in the first recording,
	// which did not compute it.
	AmountNotInText         rate `json:"answers_with_an_amount_not_in_the_text_sent"`
	AmountNotInTextAccepted int  `json:"answers_with_an_amount_not_in_the_text_sent_accepted"`
	AmountNotInTextWrong    int  `json:"answers_with_an_amount_not_in_the_text_sent_wrong"`
	AmountsNotInTextTotal   int  `json:"amounts_not_in_the_text_sent_total"`

	Reasons map[string]int `json:"review_reasons"`

	InputTokens   int     `json:"input_tokens_total"`
	OutputTokens  int     `json:"output_tokens_total"`
	MeanInput     float64 `json:"input_tokens_mean"`
	MeanOutput    float64 `json:"output_tokens_mean"`
	LatencyP50ms  int64   `json:"latency_p50_ms"`
	LatencyP99ms  int64   `json:"latency_p99_ms"`
	AttemptsTotal int     `json:"attempts_total"`
}

// summarize folds records into a tally. Records of failed calls count only as failures.
func summarize(recs []record) tally {
	t := tally{Reasons: map[string]int{}}
	var truthU, readU, strict, amount, parsed, notInText int
	var lat []int64
	for _, r := range recs {
		t.AttemptsTotal += r.Attempts
		if r.Attempts > 1 {
			t.Retried++
		}
		if r.CallError != "" {
			t.CallsFailed++
			continue
		}
		t.Calls++
		t.InputTokens += r.InputTokens
		t.OutputTokens += r.OutputTokens
		if r.Attempts > 0 { // a receipt with no text never reached the model
			lat = append(lat, r.LatencyMS)
		}
		if !r.Score.Parsed {
			t.NotParsed++
		}
		truthU += r.Score.TruthUnits
		readU += r.Score.ReadUnits
		strict += r.Score.MatchedStrict
		amount += r.Score.MatchedAmount
		for _, why := range r.Reasons {
			t.Reasons[why]++
		}
		if r.Score.Parsed {
			parsed++
			if len(r.AmountsNotInText) > 0 {
				notInText++
				t.AmountsNotInTextTotal += len(r.AmountsNotInText)
				if r.Status == receipts.StatusClaiming {
					t.AmountNotInTextAccepted++
				}
				if !r.Score.AmountsExact {
					t.AmountNotInTextWrong++
				}
			}
		}
		accepted, correct := r.Status == receipts.StatusClaiming, r.Score.AmountsExact
		switch {
		case accepted && correct:
			t.AcceptedCorrect++
		case accepted && !correct:
			t.AcceptedWrong++
		case !accepted && correct:
			t.FlaggedCorrect++
		default:
			t.FlaggedWrong++
		}
	}
	n := t.Calls
	wrong := t.AcceptedWrong + t.FlaggedWrong
	right := t.AcceptedCorrect + t.FlaggedCorrect
	t.AmountsExact = newRate(right, n)
	t.AmountNotInText = newRate(notInText, parsed)
	t.SilentErrorRate = newRate(t.AcceptedWrong, n)
	t.CatchRate = newRate(t.FlaggedWrong, wrong)
	t.FalseAlarmRate = newRate(t.FlaggedCorrect, right)
	exact, total := 0, 0
	for _, r := range recs {
		if r.CallError == "" && r.Score.Exact {
			exact++
		}
		if r.CallError == "" && r.Score.TotalOK {
			total++
		}
	}
	t.Exact = newRate(exact, n)
	t.TotalRead = newRate(total, n)
	if readU > 0 {
		t.UnitPrecision, t.AmountPrec = float64(strict)/float64(readU), float64(amount)/float64(readU)
	}
	if truthU > 0 {
		t.UnitRecall, t.AmountRecall = float64(strict)/float64(truthU), float64(amount)/float64(truthU)
	}
	if n > 0 {
		t.MeanInput, t.MeanOutput = float64(t.InputTokens)/float64(n), float64(t.OutputTokens)/float64(n)
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		t.LatencyP50ms, t.LatencyP99ms = nearestRank(lat, 0.50), nearestRank(lat, 0.99)
	}
	return t
}

// nearestRank is the p-quantile by the nearest-rank method of a sorted slice.
func nearestRank(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}

// What kind of wrong a parse is. Not every wrong parse hurts the same:
//
//   - quantity_only: every line's total, and the tax and the tip, are right; only how many
//     units a line is differs (a quantity digit that OCR dropped or misread: "5 Salmon
//     Nigiri 174.25" read as one unit). The expense total and each line's price are right;
//     what is lost is claiming one unit at a time.
//   - amounts: a line total, the tax or the tip is wrong, or a line is missing or invented:
//     somebody would be charged the wrong amount.
//   - unparsed: the answer was not valid for the schema, so nothing was read.
const (
	kindQuantityOnly = "quantity_only"
	kindAmounts      = "amounts"
	kindUnparsed     = "unparsed"
)

// classify says what kind of wrong a parse is. It is meant for parses that are not
// amounts-exact; for one that is, the answer is meaningless.
func classify(truth synth.Receipt, raw []byte) string {
	ex, err := receipts.ParseExtraction(raw)
	if err != nil {
		return kindUnparsed
	}
	if ex.TaxMinor != truth.TaxMinor || ex.TipMinor != truth.TipMinor || len(ex.Lines) != len(truth.Lines) {
		return kindAmounts
	}
	want, got := map[string]int{}, map[string]int{}
	for _, l := range truth.Lines {
		want[strconv.FormatInt(l.TotalMinor, 10)]++
	}
	for _, l := range ex.Lines {
		got[strconv.FormatInt(l.LineTotalMinor, 10)]++
	}
	if intersect(want, got) == len(truth.Lines) {
		return kindQuantityOnly
	}
	return kindAmounts
}
