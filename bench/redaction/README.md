# Redaction recall

How much planted personal data `receipts.Redact` removes before text goes to the model, on 1,000 synthetic receipts. It touches no database. Results: [claim 7](../../docs/EVIDENCE.md).

    caffeinate -ims make bench-redaction   # about 4 minutes; needs Tesseract (brew install tesseract)

Flags: `--receipts`, `--variants` shrink it for a smoke run (needs `--allow-dirty`, never committed); `--from` selects the receipt references (1-1000 by default; 1001-2000 was held out from the email-pattern change).

## What it measures

Receipts come from `internal/receipts/synth`, the same ones in every variant. Each carries a random subset of a merchant phone number (60%), a card number (50%), an email address (40%) and a customer phone number (30%), in 6 phone layouts and 4 card layouts, all fictional. Two readings:

1. **Ground-truth text** (`ground-truth-text`): what the pipeline would see if OCR were perfect.
2. **What Tesseract reads from the rendered receipt** (`ocr-clean`, `ocr-mild`, `ocr-moderate`, `ocr-harsh`): the receipt drawn as an image (Go Mono, 28 px), clean or with skew of 1.5 / 3 / 5 degrees and Gaussian pixel noise of sigma 10 / 25 / 40 gray levels (seeded by the receipt's reference), read by `ocr.Tesseract` exactly as the worker runs it (page segmentation mode 6). The images go straight to Tesseract, not through `HardenImage`.

## Definitions (stated in every result)

- A value **leaks** if at least 6 consecutive digits of a card or phone number (separators ignored), or at least 5 consecutive characters of an email's local part (lower-cased), are still readable in the redacted text. Stricter than "the exact string survives", weaker than "no trace": an email's domain, shorter digit runs and anything around the values do not count.
- **Which text a value is judged on.** The synthetic phone numbers share most of their digits (all from the fictional 555-01xx block), so judging against the whole text counted one number's leftover as a leak of the other. Instead, the lines of the OCR text are aligned with the lines of the ground truth, each value is judged on the OCR line aligned with the line it was printed on, and only that line after redaction is examined. Redaction patterns never cross a line break, so redacting line by line equals redacting the whole (tested on 400 receipts).
- A value whose line has no counterpart in the OCR text, or whose counterpart holds fewer than 4 of its digits or characters, is **lost by OCR**: not in the redacted text, so it cannot leak, though the redaction did not remove it either. It counts as not leaked in the recall and is reported in its own column.
- **Recall** = (planted - leaked) / planted, with a 95% Wilson interval, per kind and overall. Also reported: values OCR read exactly as printed, leaks of values that reached the redaction intact (a miss by the pattern) against values OCR had garbled, values lost by OCR, and placeholders inserted. The first ten leaked values per variant are listed.
- The result is **deterministic** (receipts, noise and single-threaded Tesseract give the same output every run), so there is one run per recording and the only uncertainty is the sampling interval over receipts.

## Limits (also printed in every result)

- Synthetic data written by the same author as the patterns. The planted layouts are the ones the patterns were written against; spelled-out digits and other layouts are known gaps (pinned in `internal/receipts/redact_test.go`) and are not in the sample. The numbers flatter the system and say nothing about real receipts or photographs.
- Names and street addresses are not redacted by design and are not planted. "PII removal" is not a claim.
- The images are one monospace font on a white page: far easier than a photograph. The font's slashed zero is often read by Tesseract as `@`, `Q` or `G`, which makes digits harder to read than in a typical receipt font.
- The model sees the redacted text, and a value that was only partly redacted can still reach it.
- The benchmark cannot tell a wrongly placed placeholder from a right one on OCR text, so it does not show an absence of false positives there. `TestRedact_SpaceToleranceDoesNotTouchOrdinaryText` covers ordinary text.
- The email pattern was changed after reading the first ten leaks per variant of the 1-1000 recording, so a repeat on 1-1000 is partly in-sample. References 1001-2000, which nothing was tuned on, were run with both the old and the new pattern; that comparison is the fair one.
