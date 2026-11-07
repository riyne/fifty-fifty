package synth_test

import (
	"bytes"
	"image"
	"image/png"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/riyne/fifty-fifty/internal/receipts"
	"github.com/riyne/fifty-fifty/internal/receipts/synth"
)

const sample = 3000

func TestGenerate_IsAFunctionOfTheRef(t *testing.T) {
	for _, ref := range []int{0, 1, 2, 77, 999_999} {
		a, b := synth.Generate(ref), synth.Generate(ref)
		if !reflect.DeepEqual(a, b) || a.Text() != b.Text() || a.Answer() != b.Answer() {
			t.Fatalf("ref %d: two calls differ", ref)
		}
	}
	if synth.Generate(1).Text() == synth.Generate(2).Text() {
		t.Fatal("different refs gave the same receipt")
	}
}

// Every ref has to give its own receipt: a generator that ignored part of the ref would
// quietly serve one fixture for two receipts.
func TestGenerate_EveryRefIsDistinct(t *testing.T) {
	answers := map[string]int{}
	for ref := 0; ref < 1000; ref++ {
		a := synth.Generate(ref).Answer()
		if prev, dup := answers[a]; dup {
			t.Fatalf("refs %d and %d have the same answer", prev, ref)
		}
		answers[a] = ref
	}
}

func TestGenerate_TheGroundTruthAddsUp(t *testing.T) {
	for ref := 0; ref < sample; ref++ {
		r := synth.Generate(ref)
		if len(r.Lines) < 1 || len(r.Lines) > 12 {
			t.Fatalf("ref %d: %d lines", ref, len(r.Lines))
		}
		sum := r.TaxMinor + r.TipMinor
		seen := map[string]bool{}
		for _, l := range r.Lines {
			if l.Quantity < 1 || l.Quantity > 5 || l.TotalMinor <= 0 || l.TotalMinor%int64(l.Quantity) != 0 {
				t.Fatalf("ref %d: bad line %+v", ref, l)
			}
			if seen[l.Description] {
				t.Fatalf("ref %d: %q appears twice", ref, l.Description)
			}
			seen[l.Description] = true
			sum += l.TotalMinor
		}
		if sum != r.TotalMinor || r.TaxMinor < 0 || r.TipMinor < 0 {
			t.Fatalf("ref %d: lines+tax+tip = %d, total %d", ref, sum, r.TotalMinor)
		}
	}
}

func TestText_ShowsTheGroundTruthAndNothingElseSurprising(t *testing.T) {
	for ref := 0; ref < sample; ref++ {
		r := synth.Generate(ref)
		text := r.Text()
		for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
			if len(line) > synth.Width {
				t.Fatalf("ref %d: line %q is wider than %d", ref, line, synth.Width)
			}
			for _, c := range line {
				if c < ' ' || c > '~' {
					t.Fatalf("ref %d: non-ASCII or control character %q", ref, c)
				}
			}
		}
		// Exactly one REF line, and it is the token the mock LLM looks for.
		if n := strings.Count(text, "REF "); n != 1 || !strings.Contains(text, "REF "+strconv.Itoa(ref)+"\n") {
			t.Fatalf("ref %d: REF token appears %d times", ref, n)
		}
		for _, p := range r.Planted {
			if strings.Count(text, p.Value) != 1 {
				t.Fatalf("ref %d: planted %s %q appears %d times", ref, p.Kind, p.Value, strings.Count(text, p.Value))
			}
		}
		for _, l := range r.Lines {
			if !strings.Contains(text, l.Description) {
				t.Fatalf("ref %d: %q missing from the text", ref, l.Description)
			}
		}
		if !strings.Contains(text, "TOTAL") {
			t.Fatalf("ref %d: no total line", ref)
		}
	}
}

// The answer is built with its own struct, so this test is what shows the generator and
// the validator agree on the wire format: every ground-truth answer must pass
// ParseExtraction and come out claiming, with per-unit items that sum to the lines.
func TestAnswer_PassesTheValidator(t *testing.T) {
	for ref := 0; ref < sample; ref++ {
		r := synth.Generate(ref)
		out := receipts.Interpret([]byte(r.Answer()))
		if out.Status != receipts.StatusClaiming || len(out.Reasons) != 0 || out.DiffMinor != 0 {
			t.Fatalf("ref %d: %+v\nanswer %s", ref, out, r.Answer())
		}
		units, want := 0, 0
		var sum int64
		for _, l := range r.Lines {
			want += l.Quantity
		}
		for _, it := range out.Items {
			units++
			sum += it.PriceMinor
		}
		if units != want || sum+out.TaxMinor+out.TipMinor != r.TotalMinor || out.TaxMinor != r.TaxMinor || out.TipMinor != r.TipMinor || out.PrintedTotalMinor != r.TotalMinor {
			t.Fatalf("ref %d: %d units (want %d), sum %d, outcome %+v", ref, units, want, sum, out)
		}
	}
}

// A generator that never produced a case would make the tests above vacuous, so check
// that the sample reaches every variation the benchmark depends on.
func TestGenerate_CoversTheVariations(t *testing.T) {
	lines := map[int]bool{}
	var multi, noTax, tax, noTip, tip int
	kinds := map[synth.Kind]int{}
	cardShapes, phoneShapes := map[string]int{}, map[string]int{}
	shape := regexp.MustCompile(`[0-9]`)
	for ref := 0; ref < sample; ref++ {
		r := synth.Generate(ref)
		lines[len(r.Lines)] = true
		for _, l := range r.Lines {
			if l.Quantity > 1 {
				multi++
				break
			}
		}
		if r.TaxMinor == 0 {
			noTax++
		} else {
			tax++
		}
		if r.TipMinor == 0 {
			noTip++
		} else {
			tip++
		}
		for _, p := range r.Planted {
			kinds[p.Kind]++
			switch p.Kind {
			case synth.Card:
				cardShapes[shape.ReplaceAllString(p.Value, "9")]++
			case synth.Phone:
				phoneShapes[shape.ReplaceAllString(p.Value, "9")]++
			case synth.Email:
				if !strings.HasSuffix(p.Value, "@example.com") && !strings.HasSuffix(p.Value, "@example.org") {
					t.Fatalf("email %q is not at a reserved domain", p.Value)
				}
			}
		}
	}
	for n := 1; n <= 12; n++ {
		if !lines[n] {
			t.Errorf("no receipt with %d lines in %d", n, sample)
		}
	}
	for name, n := range map[string]int{"multi-quantity": multi, "no tax": noTax, "tax": tax, "no tip": noTip, "tip": tip,
		"card": kinds[synth.Card], "email": kinds[synth.Email], "phone": kinds[synth.Phone]} {
		if n < sample/20 {
			t.Errorf("%s appears on only %d of %d receipts", name, n, sample)
		}
	}
	if len(cardShapes) != 4 || len(phoneShapes) != 6 {
		t.Errorf("card shapes %v, phone shapes %v: want 4 and 6", cardShapes, phoneShapes)
	}
}

func TestFictionalValues(t *testing.T) {
	for ref := 0; ref < sample; ref++ {
		for _, p := range synth.Generate(ref).Planted {
			if p.Kind == synth.Phone && !regexp.MustCompile(`555[ .\-]?01[0-9]{2}$`).MatchString(p.Value) {
				t.Fatalf("phone %q is outside the fictional 555-01xx block", p.Value)
			}
		}
	}
}

func TestFixtures(t *testing.T) {
	f := synth.Fixtures(40, 25)
	if len(f) != 25 {
		t.Fatalf("%d fixtures", len(f))
	}
	for ref := 40; ref < 65; ref++ {
		if f[strconv.Itoa(ref)] != synth.Generate(ref).Answer() {
			t.Fatalf("fixture %d is not that receipt's answer", ref)
		}
	}
}

func decode(t *testing.T, b []byte) *image.Gray {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	g, ok := img.(*image.Gray)
	if !ok {
		t.Fatalf("decoded %T, want *image.Gray", img)
	}
	return g
}

func inkShare(g *image.Gray) float64 {
	dark := 0
	for _, v := range g.Pix {
		if v < 128 {
			dark++
		}
	}
	return float64(dark) / float64(len(g.Pix))
}

func TestRender(t *testing.T) {
	r := synth.Generate(5)
	clean, err := r.Render(synth.RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	g := decode(t, clean)
	if g.Bounds().Dx() < 400 || g.Bounds().Dy() < 400 {
		t.Fatalf("image is %v", g.Bounds())
	}
	// Black text on white: some ink, nowhere near a black page.
	if s := inkShare(g); s < 0.01 || s > 0.25 {
		t.Fatalf("ink share %.4f", s)
	}
	again, _ := r.Render(synth.RenderOptions{})
	if !bytes.Equal(clean, again) {
		t.Fatal("rendering is not deterministic")
	}
	if other, _ := synth.Generate(6).Render(synth.RenderOptions{}); bytes.Equal(clean, other) {
		t.Fatal("two receipts rendered the same")
	}

	skewed, err := r.Render(synth.RenderOptions{SkewDegrees: 3})
	if err != nil {
		t.Fatal(err)
	}
	sg := decode(t, skewed)
	if sg.Bounds().Dx() <= g.Bounds().Dx() || sg.Bounds().Dy() <= g.Bounds().Dy() {
		t.Fatalf("skewed %v not larger than clean %v", sg.Bounds(), g.Bounds())
	}
	if s := inkShare(sg); s < 0.01 || s > 0.25 {
		t.Fatalf("skewed ink share %.4f", s)
	}

	n1, _ := r.Render(synth.RenderOptions{NoiseSigma: 20, Seed: 1})
	n1b, _ := r.Render(synth.RenderOptions{NoiseSigma: 20, Seed: 1})
	n2, _ := r.Render(synth.RenderOptions{NoiseSigma: 20, Seed: 2})
	if !bytes.Equal(n1, n1b) || bytes.Equal(n1, n2) || bytes.Equal(n1, clean) {
		t.Fatal("noise is not a deterministic function of its seed")
	}
	if g1 := decode(t, n1); g1.Bounds() != g.Bounds() {
		t.Fatalf("noise changed the size: %v", g1.Bounds())
	}
}

func TestRender_RefusesWhatItCannotDraw(t *testing.T) {
	r := synth.Generate(1)
	for name, o := range map[string]synth.RenderOptions{
		"skew too large": {SkewDegrees: 16}, "skew too negative": {SkewDegrees: -16},
		"noise too large": {NoiseSigma: 101}, "negative noise": {NoiseSigma: -1},
		"negative font": {FontPx: -1}, "huge font": {FontPx: 101},
	} {
		if _, err := r.Render(o); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := (synth.Receipt{}).Render(synth.RenderOptions{}); err == nil {
		t.Error("an empty receipt rendered")
	}
}

func TestRender_AcceptsTheLimits(t *testing.T) {
	r := synth.Generate(1)
	for _, o := range []synth.RenderOptions{{SkewDegrees: 15}, {SkewDegrees: -15}, {NoiseSigma: 100}, {FontPx: 100}, {FontPx: 10}} {
		if _, err := r.Render(o); err != nil {
			t.Errorf("%+v: %v", o, err)
		}
	}
}
