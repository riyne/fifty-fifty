package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/riyne/fifty-fifty/internal/receipts/synth"
)

func TestRun_WritesReceiptsAndFixtures(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	if err := run([]string{"--out", dir, "--from", "10", "--count", "3", "--skew", "2", "--noise", "5"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for ref := 10; ref < 13; ref++ {
		base := filepath.Join(dir, "0000000"+string(rune('0'+ref/10))+string(rune('0'+ref%10)))
		want := synth.Generate(ref)
		if b, _ := os.ReadFile(base + ".txt"); string(b) != want.Text() {
			t.Errorf("ref %d: text differs", ref)
		}
		if b, _ := os.ReadFile(base + ".json"); string(bytes.TrimSpace(b)) != want.Answer() {
			t.Errorf("ref %d: answer differs", ref)
		}
		wantPNG, err := want.Render(synth.RenderOptions{SkewDegrees: 2, NoiseSigma: 5, Seed: uint64(ref)})
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(base + ".png"); !bytes.Equal(b, wantPNG) {
			t.Errorf("ref %d: png is not the receipt rendered with the flags' options and seed %d (%d bytes)", ref, ref, len(b))
		}
	}
	var fx map[string]string
	b, err := os.ReadFile(filepath.Join(dir, "fixtures.json"))
	if err != nil || json.Unmarshal(b, &fx) != nil || len(fx) != 3 || fx["10"] != synth.Generate(10).Answer() || fx["12"] != synth.Generate(12).Answer() {
		t.Fatalf("fixtures.json: %v %v", err, fx)
	}
}

func TestRun_RefusesBadInputAndNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	for name, args := range map[string][]string{
		"no out":         {},
		"positional":     {"--out", filepath.Join(dir, "a"), "x"},
		"zero count":     {"--out", filepath.Join(dir, "b"), "--count", "0"},
		"huge count":     {"--out", filepath.Join(dir, "c"), "--count", "100000"},
		"negative from":  {"--out", filepath.Join(dir, "d"), "--from", "-1"},
		"skew too large": {"--out", filepath.Join(dir, "e"), "--skew", "20"},
		"unknown flag":   {"--nope"},
	} {
		var stderr bytes.Buffer
		if err := run(args, &stderr); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// A directory that already holds a file is left alone.
	keep := filepath.Join(dir, "keep")
	if err := os.Mkdir(keep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keep, "mine.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--out", keep, "--count", "1"}, io.Discard); err == nil || errors.Is(err, errUsage) {
		t.Errorf("non-empty directory: err=%v, want a plain error", err)
	}
	if b, _ := os.ReadFile(filepath.Join(keep, "mine.txt")); string(b) != "mine" {
		t.Error("existing file was touched")
	}
	if entries, _ := os.ReadDir(keep); len(entries) != 1 {
		t.Errorf("%d entries after a refused run", len(entries))
	}
}
