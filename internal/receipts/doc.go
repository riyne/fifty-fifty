// Package receipts holds the pure parts of the receipt pipeline (docs/ARCHITECTURE.md section 6): no
// database, no network, no clock. Everything here treats the text of a receipt, and
// whatever a language model says about it, as untrusted input.
//
//   - Redact removes card numbers, phone numbers and email addresses from OCR text before
//     it is sent to a model (names are NOT redacted; this is pattern matching on three
//     kinds of string, not removal of personal data).
//   - ParseExtraction checks a model's answer against a strict schema; the answer is
//     never trusted for anything it did not have to say (no confidence field, no
//     instructions, no extra fields).
//   - Interpret turns a valid extraction into per-unit items and decides, from the
//     numbers alone, whether a person has to look at them (the review flag).
//   - ExpandQuantity splits "3 x beer" into units that can be claimed one by one.
//
// Money is int64 minor units. There are no floats.
package receipts
