// Command receiptgen writes synthetic receipts to a directory: for each reference
// number a PNG, the text it was drawn from, and the ground-truth answer, plus one
// fixtures.json that maps every reference number to its answer, which is what
// `mockllm --fixtures` reads. Synthetic data only; see internal/receipts/synth.
//
//	receiptgen --out /some/dir --from 1 --count 20 --skew 2 --noise 12
//
// The output directory must not exist or must be empty: the tool never overwrites.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/riyne/fifty-fifty/internal/receipts/synth"
)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		if !errors.Is(err, errUsage) {
			fmt.Fprintln(os.Stderr, "receiptgen:", err)
		}
		os.Exit(2)
	}
}

var errUsage = errors.New("usage")

const maxCount = 10000

func run(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("receiptgen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "output directory (required; must not exist or be empty)")
	from := fs.Int("from", 1, "first reference number")
	count := fs.Int("count", 10, "how many receipts")
	var o synth.RenderOptions
	fs.Float64Var(&o.FontPx, "font", 0, "font size in pixels (0 is the default, 28)")
	fs.Float64Var(&o.SkewDegrees, "skew", 0, "rotation in degrees, at most 15")
	fs.Float64Var(&o.NoiseSigma, "noise", 0, "Gaussian pixel noise, in gray levels, at most 100")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	switch {
	case fs.NArg() != 0 || *out == "":
		fs.Usage()
		return errUsage
	case *from < 0 || *from > 999_999_999-maxCount:
		return fmt.Errorf("--from %d out of range", *from)
	case *count < 1 || *count > maxCount:
		return fmt.Errorf("--count %d must be 1 to %d", *count, maxCount)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	if entries, err := os.ReadDir(*out); err != nil {
		return err
	} else if len(entries) != 0 {
		return fmt.Errorf("%s is not empty", *out)
	}
	for ref := *from; ref < *from+*count; ref++ {
		r := synth.Generate(ref)
		o.Seed = uint64(ref)
		png, err := r.Render(o)
		if err != nil {
			return err
		}
		base := filepath.Join(*out, fmt.Sprintf("%09d", ref))
		for name, data := range map[string][]byte{".png": png, ".txt": []byte(r.Text()), ".json": []byte(r.Answer() + "\n")} {
			if err := os.WriteFile(base+name, data, 0o644); err != nil {
				return err
			}
		}
	}
	b, err := json.MarshalIndent(synth.Fixtures(*from, *count), "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(*out, "fixtures.json"), append(b, '\n'), 0o644)
}
