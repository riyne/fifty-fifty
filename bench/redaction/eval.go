package main

import (
	"math"
	"strings"
	"unicode"

	"github.com/riyne/fifty-fifty/internal/receipts"
	"github.com/riyne/fifty-fifty/internal/receipts/synth"
)

// Leak definitions. "Recall" below is the share of planted values with no leak.
//
// A planted card or phone number leaks if any run of digitRun consecutive digits of it
// (separators ignored) is still present, in order, in the digits of the redacted text.
// An email address leaks if any emailRun consecutive characters of its local part
// (lower-cased) are still present in the lower-cased redacted text, or the whole local
// part when it is shorter than that. These are deliberately stricter than "the exact
// string survives": an OCR error that makes the redaction pattern miss (a dropped digit,
// a space inside an address) leaves most of the value readable, and that counts.
//
// They are still weaker than "no trace of the value": the domain of an email, fewer than
// digitRun digits and a name next to a phone number are not counted, and the planted
// values contain no names or addresses at all (the redaction does not touch those).
//
// Which part of the text a value is judged against. The synthetic phone numbers all come
// from the fictional 555-01xx block, so two numbers on one receipt share most of their
// digits, and judging a value against the whole redacted text (or against "the line that
// looks most like it") would count a leftover of one number as a leak of the other. Instead
// the lines of the OCR text are aligned with the lines of the ground-truth text (an order-
// preserving matching of similar lines, so a dropped or split line does not shift the rest),
// each planted value is judged on the OCR line aligned with the line it was printed on, and
// only that line, redacted, is examined. Redaction patterns never cross a line break, so
// redacting line by line gives the same text as redacting the whole (tested). A value whose
// line has no counterpart in the OCR text, or whose counterpart holds fewer than anchorMin
// of its digits or characters, is counted as lost by OCR: it is not in the redacted text,
// so it cannot leak, but the redaction did not remove it either, and it is reported
// separately.
const (
	digitRun  = 6
	emailRun  = 5
	anchorMin = 4
)

func digitsOf(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// leaked reports whether the planted value is still readable in the redacted text.
func leaked(p synth.Planted, redacted string) bool {
	switch p.Kind {
	case synth.Card, synth.Phone:
		return hasRun(digitsOf(p.Value), digitsOf(redacted), digitRun)
	case synth.Email:
		local, _, _ := strings.Cut(strings.ToLower(p.Value), "@")
		return hasRun(local, strings.ToLower(redacted), emailRun)
	}
	panic("unknown kind " + string(p.Kind))
}

// hasRun reports whether any window of n consecutive characters of needle (the whole
// needle if it is shorter) occurs in haystack.
func hasRun(needle, haystack string, n int) bool {
	if len(needle) <= n {
		return needle != "" && strings.Contains(haystack, needle)
	}
	for i := 0; i+n <= len(needle); i++ {
		if strings.Contains(haystack, needle[i:i+n]) {
			return true
		}
	}
	return false
}

// readExactly: the planted string, as printed, appears in the text OCR produced
// (whitespace runs collapsed). When it does not, OCR changed the value, and a miss by the
// redaction pattern is a miss on a garbled string.
func readExactly(p synth.Planted, text string) bool {
	return strings.Contains(collapse(text), collapse(p.Value))
}

func collapse(s string) string {
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
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
	if lo < 0 {
		lo = 0
	}
	if hi > 1 {
		hi = 1
	}
	return lo, hi
}

// kindTally counts one kind of planted value.
type kindTally struct {
	Planted int `json:"planted"`
	Leaked  int `json:"leaked"`
	// LostByOCR: planted values of which fewer than anchorMin digits or characters appear
	// anywhere in the OCR text. They are counted as not leaked and reported here.
	LostByOCR int `json:"lost_by_ocr_not_in_text"`
	// ReadExactly: planted values whose exact string is in the OCR text (all of them for
	// the ground-truth variant). LeakedWhenReadExactly and LeakedWhenGarbled split Leaked
	// by whether the value reached the redaction intact.
	ReadExactly           int     `json:"read_exactly"`
	LeakedWhenReadExactly int     `json:"leaked_when_read_exactly"`
	LeakedWhenGarbled     int     `json:"leaked_when_garbled_by_ocr"`
	Recall                float64 `json:"recall"`
	RecallLow95           float64 `json:"recall_wilson95_low"`
	RecallHigh95          float64 `json:"recall_wilson95_high"`
	// Replacements is how many placeholders of this kind the redaction inserted; the
	// excess over Planted is what it replaced that was not a planted value (or one value
	// replaced twice).
	Replacements int `json:"placeholders_inserted"`
}

func (t *kindTally) finish() {
	if t.Planted > 0 {
		t.Recall = float64(t.Planted-t.Leaked) / float64(t.Planted)
	}
	t.RecallLow95, t.RecallHigh95 = Wilson(t.Planted-t.Leaked, t.Planted)
}

// example is a leaked value kept for the record (all values are synthetic).
type example struct {
	Ref          int    `json:"ref"`
	Kind         string `json:"kind"`
	Value        string `json:"planted_value"`
	ReadExactly  bool   `json:"read_exactly_by_ocr"`
	RedactedLine string `json:"redacted_text_line_with_the_leak"`
}

// evaluate scores one receipt. truth is the receipt's ground-truth text and text what was
// read (the same, for the ground-truth variant). It redacts the text line by line and
// judges each planted value on the line aligned with the one it was printed on.
func evaluate(rc synth.Receipt, truth, text string, per map[synth.Kind]*kindTally, overall *kindTally, examples *[]example, maxExamples int) {
	truthLines, lines := strings.Split(truth, "\n"), strings.Split(text, "\n")
	align := alignLines(truthLines, lines)
	redactedLines := make([]string, len(lines))
	for i, l := range lines {
		r, c := receipts.Redact(l)
		redactedLines[i] = r
		per[synth.Card].Replacements += c.Cards
		per[synth.Email].Replacements += c.Emails
		per[synth.Phone].Replacements += c.Phones
		overall.Replacements += c.Total()
	}
	for _, p := range rc.Planted {
		exact := readExactly(p, text)
		idx := -1
		for ti, l := range truthLines {
			if strings.Contains(l, p.Value) {
				idx = align[ti]
				break
			}
		}
		lost := idx < 0 || runWith(p, lines[idx]) < anchorMin
		lk := !lost && leaked(p, redactedLines[idx])
		for _, t := range []*kindTally{per[p.Kind], overall} {
			t.Planted++
			if exact {
				t.ReadExactly++
			}
			switch {
			case lost:
				t.LostByOCR++
			case lk:
				t.Leaked++
				if exact {
					t.LeakedWhenReadExactly++
				} else {
					t.LeakedWhenGarbled++
				}
			}
		}
		if lk && len(*examples) < maxExamples {
			*examples = append(*examples, example{Ref: rc.Ref, Kind: string(p.Kind), Value: p.Value, ReadExactly: exact, RedactedLine: strings.TrimSpace(redactedLines[idx])})
		}
	}
}

// runWith is the longest common run between the planted value and a line (digits for a
// card or phone number, lower-case characters of the local part for an email).
func runWith(p synth.Planted, line string) int {
	if p.Kind == synth.Email {
		local, _, _ := strings.Cut(strings.ToLower(p.Value), "@")
		return longestCommonRun(local, strings.ToLower(line))
	}
	return longestCommonRun(digitsOf(p.Value), digitsOf(line))
}

// minLineSimilarity is how alike two lines must be to be matched by alignLines.
const minLineSimilarity = 0.3

// alignLines matches lines of the ground truth to lines of what was read, in order, and
// returns for each truth line the index of its match, or -1. It maximizes the total
// similarity of matched pairs (similarity: twice the longest common subsequence over the
// two lengths, on trimmed lower-case lines), and only pairs at least minLineSimilarity
// alike may be matched, so a dropped line stays unmatched and does not drag its
// neighbours with it.
func alignLines(truth, read []string) []int {
	norm := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	n, m := len(truth), len(read)
	sim := make([][]float64, n)
	for i := range sim {
		sim[i] = make([]float64, m)
		a := norm(truth[i])
		for j := range sim[i] {
			b := norm(read[j])
			if len(a)+len(b) > 0 {
				sim[i][j] = 2 * float64(longestCommonSubsequence(a, b)) / float64(len(a)+len(b))
			}
		}
	}
	// dp[i][j]: best total similarity using the first i truth lines and first j read lines.
	dp := make([][]float64, n+1)
	for i := range dp {
		dp[i] = make([]float64, m+1)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			best := max(dp[i-1][j], dp[i][j-1])
			if sim[i-1][j-1] >= minLineSimilarity {
				best = max(best, dp[i-1][j-1]+sim[i-1][j-1])
			}
			dp[i][j] = best
		}
	}
	out := make([]int, n)
	for i := range out {
		out[i] = -1
	}
	for i, j := n, m; i > 0 && j > 0; {
		switch {
		case sim[i-1][j-1] >= minLineSimilarity && dp[i][j] == dp[i-1][j-1]+sim[i-1][j-1]:
			out[i-1] = j - 1
			i, j = i-1, j-1
		case dp[i][j] == dp[i-1][j]:
			i--
		default:
			j--
		}
	}
	return out
}

// longestCommonSubsequence is the length of the longest subsequence of a that is also one of b.
func longestCommonSubsequence(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
			} else {
				cur[j] = max(prev[j], cur[j-1])
			}
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// longestCommonRun is the length of the longest common substring of a and b.
func longestCommonRun(a, b string) int {
	best := 0
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
				if cur[j] > best {
					best = cur[j]
				}
			} else {
				cur[j] = 0
			}
		}
		prev, cur = cur, prev
	}
	return best
}
