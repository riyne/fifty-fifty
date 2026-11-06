package receipts

import (
	"regexp"
	"strings"
)

// Placeholders that replace what Redact finds.
const (
	CardPlaceholder  = "[CARD]"
	PhonePlaceholder = "[PHONE]"
	EmailPlaceholder = "[EMAIL]"
)

// Redactions counts what Redact replaced. Counts only: the values never leave Redact.
type Redactions struct{ Cards, Phones, Emails int }

// Total is the number of replacements.
func (r Redactions) Total() int { return r.Cards + r.Phones + r.Emails }

var (
	// 13 to 19 digits, in groups separated by single spaces or dashes: payment card
	// numbers as printed. No checksum test on purpose: a receipt's own long digit strings
	// (order and ticket numbers) are of no use to the model, and a missed card number is
	// the worse mistake.
	cardRE = regexp.MustCompile(`[0-9](?:[ -]?[0-9]){12,18}`)
	// local@domain.tld
	emailRE = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}`)
	// North American style (optional +country, area code with or without parentheses, 3 + 4
	// digits) and the common international "+cc" forms with 8 to 12 more digits.
	phoneRE = regexp.MustCompile(`(?:\+?[0-9]{1,3}[ .\-]?)?(?:\([0-9]{3}\)|[0-9]{3})[ .\-]?[0-9]{3}[ .\-]?[0-9]{4}|\+[0-9]{1,3}(?:[ .\-]?[0-9]){8,12}`)
)

// Redact replaces card numbers, email addresses and phone numbers in text with
// placeholders and counts them. It matches patterns; it does not understand text. It
// misses what is written out (an address spelled "name at example dot com", digits spelled
// as words), what OCR garbled so that it no longer matches, and everything that is not one
// of these three kinds, names and addresses included. It leans towards redacting too
// much: a long order number, or a phone number glued to letters, is replaced too, because
// the model has no use for them and a missed phone number is the worse mistake.
func Redact(text string) (string, Redactions) {
	var n Redactions
	text = replaceAll(text, cardRE, CardPlaceholder, &n.Cards)
	text = replaceAll(text, emailRE, EmailPlaceholder, &n.Emails)
	text = replaceAll(text, phoneRE, PhonePlaceholder, &n.Phones)
	return text, n
}

// replaceAll replaces every match of re.
func replaceAll(s string, re *regexp.Regexp, with string, count *int) string {
	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringIndex(s, -1) {
		b.WriteString(s[last:m[0]])
		b.WriteString(with)
		last = m[1]
		*count++
	}
	b.WriteString(s[last:])
	return b.String()
}
