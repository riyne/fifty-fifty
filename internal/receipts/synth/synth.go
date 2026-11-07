// Package synth makes synthetic restaurant receipts from known ground truth: the items,
// quantities, tax, tip and total are chosen first, then printed as text and rendered as
// an image. Everything is a function of the reference number, so a run can be repeated
// and a result can name the receipts it used.
//
// Three things use it: the redaction recall benchmark (the planted card numbers, email
// addresses and phone numbers are known), the mock LLM (Answer is the ground truth in
// the wire format the extractor expects, served under the receipt's "REF n" line), and
// the demo (print or display a receipt and photograph it).
//
// What it is not: real receipts. Layout, vocabulary, number formats and noise are all
// chosen by the person who also wrote the redaction patterns and the validator, so any
// result measured on it flatters the system, and the README must say so. The planted
// values are fictional (phone numbers in the 555-01xx block, email addresses at
// example.com and example.org, random digits for cards).
package synth

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
)

// Kind is a category of planted personal data.
type Kind string

const (
	Card  Kind = "card"
	Email Kind = "email"
	Phone Kind = "phone"
)

// Planted is a value printed on the receipt that redaction is expected to remove.
type Planted struct {
	Kind  Kind
	Value string // exactly as printed
}

// Line is one printed item line. TotalMinor is for the whole line, as receipts print it.
type Line struct {
	Description string
	Quantity    int
	TotalMinor  int64
}

// Receipt is the ground truth for one synthetic receipt.
type Receipt struct {
	Ref        int
	Merchant   string
	Lines      []Line
	TaxMinor   int64
	TipMinor   int64
	TotalMinor int64
	Planted    []Planted

	// printed is the text, one string per line, built once by Generate.
	printed []string
}

// Width is the number of characters in a printed line.
const Width = 36

var dishes = []string{
	"Pad Thai", "Green Curry", "Tom Yum Soup", "Spring Rolls", "Mango Salad", "Beef Rendang",
	"Chicken Satay", "Fried Rice", "Pho Dac Biet", "Banh Mi", "Bun Cha", "Goi Cuon",
	"Margherita Pizza", "Penne Arrabbiata", "Caesar Salad", "Garlic Bread", "Tiramisu",
	"Fish Tacos", "Chicken Burrito", "Guacamole", "Nachos", "Elote", "Churros",
	"Miso Soup", "Salmon Nigiri", "Dragon Roll", "Chicken Katsu", "Gyoza", "Edamame",
	"Cheeseburger", "Poutine", "Club Sandwich", "Onion Rings", "Milkshake", "Apple Pie",
	"Iced Tea", "Lemonade", "Draft Beer", "House Red Wine", "Espresso", "Flat White",
	"Butter Chicken", "Garlic Naan", "Palak Paneer", "Mango Lassi", "Samosa", "Biryani",
}

var merchants = []string{
	"THE CEDAR GRILL", "NOODLE HOUSE 88", "LITTLE ITALY TRATTORIA", "CASA VERDE",
	"HARBOUR SUSHI", "THE CORNER DINER", "SPICE ROUTE", "BLUE HERON CAFE",
}

var emailNames = []string{
	"jane.doe", "sam_lee", "alex.k", "priya.n", "chris.o", "dana.w", "m.tremblay", "r.singh",
}

// taxRatesBP are the sales-tax rates in basis points: none printed, 5%, 12%.
var taxRatesBP = []int64{0, 500, 1200}

// tipPercents are the tips printed as a share of the subtotal; 0 means no tip line.
var tipPercents = []int64{0, 0, 15, 18, 20}

// Generate returns the receipt for a reference number. The same ref always gives the
// same receipt.
func Generate(ref int) Receipt {
	rng := rand.New(rand.NewPCG(uint64(ref), 0x73796e7468726563))
	r := Receipt{Ref: ref, Merchant: merchants[rng.IntN(len(merchants))]}

	n := 1 + rng.IntN(12)
	for _, i := range rng.Perm(len(dishes))[:n] {
		qty := 1
		if rng.IntN(4) == 0 {
			qty = 2 + rng.IntN(4)
		}
		unit := int64(250 + rng.IntN(4251)) // 2.50 to 45.00
		r.Lines = append(r.Lines, Line{Description: dishes[i], Quantity: qty, TotalMinor: unit * int64(qty)})
	}
	var subtotal int64
	for _, l := range r.Lines {
		subtotal += l.TotalMinor
	}
	r.TaxMinor = roundShare(subtotal, taxRatesBP[rng.IntN(len(taxRatesBP))], 10000)
	if p := tipPercents[rng.IntN(len(tipPercents))]; p > 0 {
		r.TipMinor = roundShare(subtotal, p, 100)
	}
	r.TotalMinor = subtotal + r.TaxMinor + r.TipMinor

	r.plant(rng)
	r.print(rng, subtotal)
	return r
}

// roundShare is amount * num / den rounded half up.
func roundShare(amount, num, den int64) int64 { return (amount*num + den/2) / den }

// plant chooses the personal data printed on this receipt. Each kind appears with its
// own probability, in one of several formats.
func (r *Receipt) plant(rng *rand.Rand) {
	if rng.IntN(10) < 6 {
		r.Planted = append(r.Planted, Planted{Phone, phoneNumber(rng)}) // the merchant's number
	}
	if rng.IntN(10) < 5 {
		r.Planted = append(r.Planted, Planted{Card, cardNumber(rng)})
	}
	if rng.IntN(10) < 4 {
		r.Planted = append(r.Planted, Planted{Email, fmt.Sprintf("%s%d@%s", emailNames[rng.IntN(len(emailNames))], rng.IntN(100), []string{"example.com", "example.org"}[rng.IntN(2)])})
	}
	if rng.IntN(10) < 3 {
		r.Planted = append(r.Planted, Planted{Phone, phoneNumber(rng)}) // a customer's number
	}
}

// phoneNumber returns a fictional number in one of the common North American layouts.
// The line and exchange avoid real allocations: 555-01xx is reserved for fiction.
func phoneNumber(rng *rand.Rand) string {
	area := []string{"604", "778", "250", "416", "212", "415"}[rng.IntN(6)]
	line := fmt.Sprintf("01%02d", rng.IntN(100))
	switch rng.IntN(6) {
	case 0:
		return fmt.Sprintf("(%s) 555-%s", area, line)
	case 1:
		return fmt.Sprintf("%s-555-%s", area, line)
	case 2:
		return fmt.Sprintf("%s.555.%s", area, line)
	case 3:
		return fmt.Sprintf("+1 %s 555 %s", area, line)
	case 4:
		return fmt.Sprintf("1-%s-555-%s", area, line)
	default:
		return fmt.Sprintf("%s555%s", area, line)
	}
}

// cardNumber returns 15 or 16 random digits in the usual groupings. It is not checked
// against any issuer or checksum; redaction does not check one either.
func cardNumber(rng *rand.Rand) string {
	digits := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte('0' + rng.IntN(10))
		}
		return string(b)
	}
	switch rng.IntN(5) {
	case 0: // 15 digits, 4-6-5
		return "37" + digits(2) + " " + digits(6) + " " + digits(5)
	case 1: // 16 digits, dashes
		d := "4" + digits(15)
		return d[0:4] + "-" + d[4:8] + "-" + d[8:12] + "-" + d[12:]
	case 2: // 16 digits, no separators
		return "5" + digits(15)
	default: // 16 digits, spaces
		d := "4" + digits(15)
		return d[0:4] + " " + d[4:8] + " " + d[8:12] + " " + d[12:]
	}
}

// money formats minor units as a decimal amount.
func money(minor int64) string {
	return fmt.Sprintf("%d.%02d", minor/100, minor%100)
}

// row left-aligns the label and right-aligns the amount within Width. A label too long
// to fit is cut, which the vocabulary never needs.
func row(label, amount string) string {
	room := Width - len(amount) - 1
	if len(label) > room {
		label = label[:room]
	}
	return label + strings.Repeat(" ", Width-len(label)-len(amount)) + amount
}

func center(s string) string {
	if len(s) >= Width {
		return s
	}
	return strings.Repeat(" ", (Width-len(s))/2) + s
}

// print builds the text of the receipt. The REF line carries the reference number the
// mock LLM looks for; the order of lines is fixed, the content is not.
func (r *Receipt) print(rng *rand.Rand, subtotal int64) {
	var out []string
	add := func(s string) { out = append(out, s) }

	add(center(r.Merchant))
	add(center(fmt.Sprintf("%d Main St, Vancouver BC", 100+rng.IntN(900))))
	var phones, cards, emails []string
	for _, p := range r.Planted {
		switch p.Kind {
		case Phone:
			phones = append(phones, p.Value)
		case Card:
			cards = append(cards, p.Value)
		case Email:
			emails = append(emails, p.Value)
		}
	}
	// The first phone is the merchant's; a second one is a customer's, printed below.
	if len(phones) > 0 {
		add(center("Tel " + phones[0]))
	}
	add("REF " + strconv.Itoa(r.Ref))
	add(fmt.Sprintf("Table %d   Server %s", 1+rng.IntN(30), []string{"Dana", "Marco", "Aiko", "Sam"}[rng.IntN(4)]))
	add(strings.Repeat("-", Width))
	for _, l := range r.Lines {
		label := l.Description
		if l.Quantity > 1 {
			label = strconv.Itoa(l.Quantity) + " " + label
		}
		add(row(label, money(l.TotalMinor)))
	}
	add(strings.Repeat("-", Width))
	add(row("Subtotal", money(subtotal)))
	if r.TaxMinor > 0 {
		add(row("Tax", money(r.TaxMinor)))
	}
	if r.TipMinor > 0 {
		add(row("Tip", money(r.TipMinor)))
	}
	add(row("TOTAL", money(r.TotalMinor)))
	add(strings.Repeat("-", Width))
	for _, c := range cards {
		add("CARD " + c)
	}
	for _, e := range emails {
		add("Receipt sent to")
		add(e)
	}
	if len(phones) > 1 {
		add("Customer " + phones[1])
	}
	add(center("Thank you!"))
	r.printed = out
}

// Text is what the receipt says, one line per row, as a perfect reader would see it.
func (r Receipt) Text() string { return strings.Join(r.printed, "\n") + "\n" }

// answer is the wire format the extractor is asked for. It is declared here, and not
// borrowed from the receipts package, on purpose: the tests then show that the two
// agree, instead of the generator being right by construction.
type answer struct {
	Items []answerItem `json:"items"`
	Tax   int64        `json:"tax_minor,omitempty"`
	Tip   int64        `json:"tip_minor,omitempty"`
	Total int64        `json:"total_minor"`
}

type answerItem struct {
	Description string `json:"description"`
	Quantity    int    `json:"quantity,omitempty"`
	LineTotal   int64  `json:"line_total_minor"`
}

// Answer is the ground truth as a model would ideally answer: the JSON object the
// extractor expects.
func (r Receipt) Answer() string {
	a := answer{Tax: r.TaxMinor, Tip: r.TipMinor, Total: r.TotalMinor}
	for _, l := range r.Lines {
		it := answerItem{Description: l.Description, LineTotal: l.TotalMinor}
		if l.Quantity > 1 {
			it.Quantity = l.Quantity
		}
		a.Items = append(a.Items, it)
	}
	b, err := json.Marshal(a)
	if err != nil {
		panic(err) // plain structs of strings and integers cannot fail
	}
	return string(b)
}

// Fixtures returns the mock LLM's fixture map for the receipts with refs from..from+count-1:
// each ref's decimal string to its ground-truth answer.
func Fixtures(from, count int) map[string]string {
	m := make(map[string]string, count)
	for ref := from; ref < from+count; ref++ {
		m[strconv.Itoa(ref)] = Generate(ref).Answer()
	}
	return m
}
