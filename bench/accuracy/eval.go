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
	var truthU, readU, strict, amount int
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
