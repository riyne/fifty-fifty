package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/riyne/fifty-fifty/internal/receipts"
	"github.com/riyne/fifty-fifty/internal/receipts/synth"
)

// analyze reads a recording (the .json this command wrote) and, for each variant, splits
// the wrong parses by what kind of wrong they are (classify) and by what the system did
// with them. It regenerates each receipt's ground truth from its reference number, and
// uses the answers the recording kept for every parse that was not exact, so it adds
// nothing to the recording and anyone can repeat it: go run ./bench/accuracy --analyze <file>.
func analyze(path string, w io.Writer) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var rep report
	if err := json.Unmarshal(raw, &rep); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	fmt.Fprintf(w, "Wrong parses by kind, from %s (commit %s)\n", path, rep.Env.Commit)
	fmt.Fprintf(w, "quantity_only: every line total, tax and tip right, only the number of units differs. amounts: a line total, tax or tip wrong, or a line missing or invented. unparsed: no valid answer.\n\n")
	fmt.Fprintf(w, "%-18s %6s | %-22s | %-22s | %-22s\n", "variant", "wrong", "quantity_only (acc/flag)", "amounts (acc/flag)", "unparsed (acc/flag)")
	var order []string
	byVariant := map[string][]record{}
	for _, r := range rep.Records {
		if _, ok := byVariant[r.Variant]; !ok {
			order = append(order, r.Variant)
		}
		byVariant[r.Variant] = append(byVariant[r.Variant], r)
	}
	for _, v := range order {
		counts := map[string][2]int{} // kind -> {accepted, flagged}
		wrong := 0
		for _, r := range byVariant[v] {
			if r.CallError != "" || r.Score.AmountsExact {
				continue
			}
			wrong++
			kind := classify(synth.Generate(r.Ref), []byte(r.Answer))
			c := counts[kind]
			if r.Status == receipts.StatusClaiming {
				c[0]++
			} else {
				c[1]++
			}
			counts[kind] = c
		}
		q, a, u := counts[kindQuantityOnly], counts[kindAmounts], counts[kindUnparsed]
		fmt.Fprintf(w, "%-18s %6d | %10d / %-10d | %10d / %-10d | %10d / %-10d\n", v, wrong, q[0], q[1], a[0], a[1], u[0], u[1])
	}
	var silent []string
	for _, v := range order {
		for _, r := range byVariant[v] {
			if r.CallError == "" && !r.Score.AmountsExact && r.Status == receipts.StatusClaiming {
				silent = append(silent, fmt.Sprintf("  %s ref %d: %s", v, r.Ref, classify(synth.Generate(r.Ref), []byte(r.Answer))))
			}
		}
	}
	sort.Strings(silent)
	fmt.Fprintf(w, "\nsilent errors (wrong and accepted), one per line:\n")
	for _, s := range silent {
		fmt.Fprintln(w, s)
	}
	return nil
}
