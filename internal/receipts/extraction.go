package receipts

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits on what a model may say. They bound the cost of looking at an answer and keep
// every sum far from int64 overflow (an expense is at most 100 items of 100,000,000).
const (
	MaxAnswerBytes = 64 << 10
	MaxLines       = 100
	MaxQuantity    = 99
	MaxAmountMinor = 100_000_000
	maxTotalMinor  = (MaxItems + 2) * MaxAmountMinor
)

// Tolerance is how far items + tax + tip may be from the printed total (minor units)
// before a person has to look. It is exact: receipts add up to the cent.
const Tolerance int64 = 0

// Why a person has to look at an answer (Outcome.Reasons).
const (
	ReasonInvalid  = "invalid_output"             // not the expected shape, or a value out of range
	ReasonTooMany  = "too_many_units"             // the quantities add up to more items than an expense can have
	ReasonMismatch = "total_mismatch"             // items + tax + tip is not the printed total
	ReasonAllFree  = "free_items_with_tax_or_tip" // nothing to charge the tax or tip to
)

// Outcome statuses: the expense statuses the pipeline may choose between.
const (
	StatusClaiming    = "claiming"
	StatusNeedsReview = "needs_review"
)

// Line is one line of a receipt as a model reported it, after validation.
type Line struct {
	Description    string
	Quantity       int
	LineTotalMinor int64 // for all Quantity units together
}

// Extraction is a validated model answer: what the receipt says, and nothing else.
type Extraction struct {
	Lines      []Line
	TaxMinor   int64
	TipMinor   int64
	TotalMinor int64 // the total printed on the receipt
}

// The wire format the model is asked for: exactly these fields, integers in minor units.
// Anything else (an extra field, a float, a string where a number belongs) is refused.
type rawLine struct {
	Description    *string `json:"description"`
	Quantity       *int    `json:"quantity"`
	LineTotalMinor *int64  `json:"line_total_minor"`
}

type rawExtraction struct {
	Items      []rawLine `json:"items"`
	TaxMinor   *int64    `json:"tax_minor"`
	TipMinor   *int64    `json:"tip_minor"`
	TotalMinor *int64    `json:"total_minor"`
}

// ErrInvalid is wrapped by every ParseExtraction error. Its messages name fields and
// limits only and never repeat anything the model wrote, because that is receipt text.
var ErrInvalid = errors.New("receipts: invalid extraction")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// ParseExtraction validates a model's answer: one JSON object, no unknown fields, no
// trailing data, integer amounts in range, 1 to MaxLines items each with a description
// and a line total, quantities 1 to MaxQuantity (default 1), and a printed total. The
// answer is an untrusted string; the only thing taken from it is these numbers and text.
func ParseExtraction(raw []byte) (Extraction, error) {
	var ex Extraction
	if len(raw) > MaxAnswerBytes {
		return ex, invalid("answer is longer than %d bytes", MaxAnswerBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var r rawExtraction
	if err := dec.Decode(&r); err != nil {
		// The decoder's messages can quote the input ("invalid character 'x' ..."), so
		// they are not passed on.
		return ex, invalid("not a JSON object of the expected shape")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return ex, invalid("data after the JSON object")
	}
	if len(r.Items) < 1 || len(r.Items) > MaxLines {
		return ex, invalid("items must have 1-%d entries", MaxLines)
	}
	for i, l := range r.Items {
		if l.Description == nil || l.LineTotalMinor == nil {
			return ex, invalid("items[%d] needs description and line_total_minor", i)
		}
		d, err := cleanDescription(*l.Description)
		if err != nil {
			return ex, invalid("items[%d].description: %v", i, err)
		}
		qty := 1
		if l.Quantity != nil {
			qty = *l.Quantity
		}
		if qty < 1 || qty > MaxQuantity {
			return ex, invalid("items[%d].quantity must be 1-%d", i, MaxQuantity)
		}
		if *l.LineTotalMinor < 0 || *l.LineTotalMinor > MaxAmountMinor {
			return ex, invalid("items[%d].line_total_minor must be 0-%d", i, MaxAmountMinor)
		}
		ex.Lines = append(ex.Lines, Line{Description: d, Quantity: qty, LineTotalMinor: *l.LineTotalMinor})
	}
	amount := func(v *int64, name string, required bool, max int64) (int64, error) {
		if v == nil {
			if required {
				return 0, invalid("%s is required", name)
			}
			return 0, nil
		}
		if *v < 0 || *v > max {
			return 0, invalid("%s must be 0-%d", name, max)
		}
		return *v, nil
	}
	var err error
	if ex.TaxMinor, err = amount(r.TaxMinor, "tax_minor", false, MaxAmountMinor); err != nil {
		return Extraction{}, err
	}
	if ex.TipMinor, err = amount(r.TipMinor, "tip_minor", false, MaxAmountMinor); err != nil {
		return Extraction{}, err
	}
	if ex.TotalMinor, err = amount(r.TotalMinor, "total_minor", true, maxTotalMinor); err != nil {
		return Extraction{}, err
	}
	return ex, nil
}

// cleanDescription makes a model's item description safe to store and show as plain
// text: runs of whitespace (including newlines) become one space, the ends are trimmed,
// and anything that is not printable text is refused: control characters, the replacement character (what broken escapes decode to), invisible
// formatting characters (zero-width joiners, bidirectional overrides that can make text
// read in a different order than it is stored), unassigned or private-use code points,
// invalid UTF-8, and anything over the length limit. Markup is plain text here (the API
// returns JSON; escaping for display is the client's job).
func cleanDescription(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("not valid UTF-8")
	}
	s = strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
	if s == "" {
		return "", errors.New("empty")
	}
	if utf8.RuneCountInString(s) > MaxDescriptionRunes {
		return "", fmt.Errorf("longer than %d characters", MaxDescriptionRunes)
	}
	for _, r := range s {
		if r == unicode.ReplacementChar { // what a broken escape or invalid UTF-8 decodes to
			return "", errors.New("contains a character that is not printable text")
		}
		// IsPrint is letters, marks, numbers, punctuation, symbols and the ASCII space: it
		// excludes control characters, invisible formatting characters (zero-width joiners,
		// bidirectional overrides), private-use and unassigned code points.
		if !unicode.IsPrint(r) {
			return "", errors.New("contains a character that is not printable text")
		}
	}
	return s, nil
}

// Outcome is what the pipeline does with an answer.
type Outcome struct {
	// Status is StatusClaiming when the answer is valid and adds up, otherwise
	// StatusNeedsReview (needs_review): a person looks before anyone can claim.
	Status string
	// Reasons say why a person has to look (empty when Status is claiming).
	Reasons []string
	// Items, TaxMinor and TipMinor are what was read: per-unit items, ready to store.
	// Empty when the answer was invalid or had too many units to store.
	Items    []Item
	TaxMinor int64
	TipMinor int64
	// PrintedTotalMinor is the receipt's own total, kept so the reviewer can see by how much
	// the items miss it; DiffMinor is items + tax + tip minus that total.
	PrintedTotalMinor int64
	DiffMinor         int64
}

// Interpret decides what to do with a model's raw answer. It never fails: whatever the
// answer was, the result is either items that add up to the receipt's printed total, or
// a request for a person to look, with whatever was read. The decision uses the numbers
// only; the model's own confidence, if it gave any, is not even parsed (an extra field
// makes the answer invalid).
func Interpret(raw []byte) Outcome {
	ex, err := ParseExtraction(raw)
	if err != nil {
		return Outcome{Status: StatusNeedsReview, Reasons: []string{ReasonInvalid}}
	}
	out := Outcome{TaxMinor: ex.TaxMinor, TipMinor: ex.TipMinor, PrintedTotalMinor: ex.TotalMinor}
	units := 0
	for _, l := range ex.Lines {
		units += l.Quantity
	}
	if units > MaxItems {
		return Outcome{Status: StatusNeedsReview, Reasons: []string{ReasonTooMany}, PrintedTotalMinor: ex.TotalMinor}
	}
	var sum int64
	for _, l := range ex.Lines {
		items, err := ExpandQuantity(l.Description, l.Quantity, l.LineTotalMinor)
		if err != nil { // cannot happen after validation
			return Outcome{Status: StatusNeedsReview, Reasons: []string{ReasonInvalid}}
		}
		out.Items = append(out.Items, items...)
		sum += l.LineTotalMinor
	}
	out.DiffMinor = sum + ex.TaxMinor + ex.TipMinor - ex.TotalMinor
	if out.DiffMinor > Tolerance || out.DiffMinor < -Tolerance {
		out.Reasons = append(out.Reasons, ReasonMismatch)
	}
	if sum == 0 && (ex.TaxMinor > 0 || ex.TipMinor > 0) {
		out.Reasons = append(out.Reasons, ReasonAllFree)
	}
	out.Status = StatusClaiming
	if len(out.Reasons) > 0 {
		out.Status = StatusNeedsReview
	}
	return out
}
