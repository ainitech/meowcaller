<!-- Datasheet template. Copy to spec/<module>.md and fill every section. A section
     with no content must say why (e.g. "no constants"), never be deleted. -->

# Datasheet: `<package>/<module>`

**Status:** planned | scaffolded | implemented | verified
**Registry #:** NN  ·  **Depends on:** `<modules>`  ·  **Depended on by:** `<modules>`

## Purpose

One or two sentences: what this module does and why it exists in the call path.

## Source of truth

- **Rust reference:** `whatsapp-rust/wacore/src/voip/<file>.rs` (key fns + lines).
- **wacrg doc:** `docs/<...>.md` (the protocol rationale).
- **Provenance / confidence:** where the facts come from (WASM RE, capture,
  reconstruction) and how sure we are. Note anything `speculative`.

## Interface

The Go API this module exposes (types + exported function signatures). This is
what gets scaffolded first.

```go
// signatures only
```

## Behavior

The algorithm/format, precisely enough to implement and to explain. Bit layouts,
stage ordering, formulas. Reference the Rust for the exact logic; restate the
load-bearing parts here so the datasheet is self-contained.

## Constants and tables

Magic numbers, labels, table names/addresses, and their origin. If a table is
loaded from a vector/ROM file, name the file.

## Inputs / outputs

Concrete shapes: byte layouts, sample counts, value ranges, units.

## Validation

- **KAT:** the vector file and exactly what it covers (e.g. "256 byte values",
  "289 LPC frames", "real captured frames → PCM").
- **How to run:** the Go test name and command.
- **Done when:** the precise pass condition.

## Assumptions and open decisions

Things not fully confirmed, choices left to the human, and discrepancies between
sources. Each is a `TODO(human)` the implementer must surface, not silently
resolve.

## Notes

Anything else a future reader needs and would otherwise lose.
