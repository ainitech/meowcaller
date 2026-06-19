# AGENTS.md — how meowcaller gets built

This library is built **module by module, under human audit, in real time**. The
human reviewer directs the engineering; agents prepare and explain. This file is
the working protocol. It is binding.

> If you are an agent reading this: you are not autonomous here. You scaffold and
> explain; the human decides how logic flows and what trade-offs are made. When in
> doubt, stop and ask. A wrong guess committed quietly is the worst outcome.

## The prime directives

1. **Do not decide logic. Scaffold it.** When you reach a function whose behavior
   involves a real engineering choice (an algorithm, a data layout, an
   error-handling strategy, a buffering decision), write the **signature, the doc
   comment, and the datasheet reference**, leave the body as a `// TODO` with the
   open question stated, and **stop for the human**. Do not fill it in to "keep
   moving."
2. **One module at a time. Take the break.** Finish a single, reviewable unit —
   often just a scaffold, or one function the human approved — then pause for
   review and approval before continuing. No multi-module sprints. The human is
   watching this get built and will say when to proceed.
3. **Always be able to explain.** At any moment you must be able to state, for the
   thing you are touching: *what it is, where it came from (Rust file + wacrg
   doc), what its inputs/outputs are, what constants it uses, and what it is
   validated against.* That information lives in the module's datasheet
   (`spec/<module>.md`). Read it before touching the module; keep it open.
4. **Verify against vectors, never vibes.** A behavior is correct only when its
   KAT passes. Reverse-engineered names and analysis notes are frequently wrong;
   the vector is the proof. If a module has no vector, that is a decision point
   for the human, not a license to guess.

## The build loop (per module)

```
1. READ      spec/<module>.md + the Rust source it cites + the wacrg doc.
2. SCAFFOLD  create the package file: types, exported signatures, doc comments,
             the KAT test wired to the (copied) vector — but function bodies are
             `// TODO` stubs that state the open question.  → COMMIT, PAUSE.
3. DIRECT    the human reviews the scaffold and decides how each body should work
             (or approves porting it 1:1 from the Rust). One function at a time.
4. IMPLEMENT the approved function(s) only. Keep the KAT test running.
             → COMMIT per function or small group, PAUSE.
5. VERIFY    when the module's body is complete, its KAT must pass.
             Update the datasheet status and CHANGELOG.  → COMMIT, PAUSE.
```

Each arrow `→ PAUSE` is a real stop: hand control back to the human. Do not chain
steps without approval.

## Scaffolding standard

A scaffold is a complete, compiling skeleton with no logic:

```go
// SealFrame encrypts one media frame with the participant SFrame key.
// Spec: spec/srtp-sframe.md · Ref: whatsapp-rust sframe.rs:193 · KAT: sframe #test
func (s *Session) SealFrame(plaintext []byte) ([]byte, error) {
	// TODO(human): GCM nonce construction — confirm the 16-byte LE-counter layout
	// and whether the varint header is appended (not AAD). See datasheet §Cipher.
	return nil, errNotImplemented
}
```

It must `go build` and `go vet` cleanly. The KAT test exists and **fails** (or
skips with a clear reason) until the body lands — never a fake pass.

## Comment policy

Comments earn their place or they do not exist:

- **`// TODO(human): ...`** — an open decision or unfinished body.
- **`// ASSUMPTION: ...`** — a choice made without full confirmation, stating what
  would invalidate it.
- **A short context note** — only when a future reader would otherwise lose
  non-obvious context (a magic constant's origin, a byte-order quirk, a deviation
  from the reference and why).
- **Doc comments** on exported identifiers, per Go convention, including the
  `Spec:`/`Ref:`/`KAT:` provenance line.

Do **not** narrate what the code plainly does. Clean code is the default;
comments are the exception.

## Commits and changelog

- One module change per commit. Subject: `(<module>: <imperative change>)`.
  Examples: `(mlow/toc: scaffold smpl TOC parser)`,
  `(srtp/e2e: implement RFC3711 AES-CM PRF)`,
  `(mlow/pitch: KAT-verify against pitch_vectors.json)`.
- Every commit updates [`CHANGELOG.md`](CHANGELOG.md) under the module with the
  new state (`scaffolded` / `implemented` / `KAT-verified`).
- Commit messages state **what was validated** when relevant (which vector,
  pass/fail). No attribution lines. No pushing unless the human asks.

## Explaining on demand

If the human asks "what is this / is this right / what did we validate?", answer
from the datasheet and the vector, concretely: the Rust source location, the
constants, the KAT file and what it covers, and any open assumptions. If you
cannot answer from the datasheet, the datasheet is incomplete — say so and fix it
before proceeding.

## What never happens here

- No autonomous multi-module runs.
- No filling a function body with a guess to avoid stopping.
- No "it compiles, ship it" — green KAT or it is not done.
- No silently copying logic whose meaning you cannot explain.
- No reuse of the old dublin/meowmeow calling code as a source.
