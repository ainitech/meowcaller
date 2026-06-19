# meowcaller — engineering plan

A **production-grade, pure-Go** library for WhatsApp 1:1 calls: signaling, keying,
media transport, and the MLow audio codec. Held to the same quality bar as
[whatsmeow](https://github.com/tulir/whatsmeow), whose structure and idioms it
mostly follows. This is not a proof of concept; it is a keystone library that
other software will depend on.

This file is the **map**. The companion documents are:

- [`AGENTS.md`](AGENTS.md) — how implementation proceeds (human-audited,
  module-by-module, agents scaffold then pause). **Read before writing any code.**
- [`SPEC.md`](SPEC.md) — the module registry and the index of per-module
  **datasheets** under [`spec/`](spec/). Each datasheet is the single reference
  for what a module is, where it comes from, and what it is validated against.
- [`CHANGELOG.md`](CHANGELOG.md) — every merged change, tracked.

---

## Mission and standard

Build a calling library that an enterprise can depend on:

- **Correct by construction.** Every byte-level behavior is verified against a
  known-answer vector (KAT) shared with the reference implementation. A module is
  not "done" until its vector passes. A passing round-trip is not enough; it must
  match real, captured data.
- **Auditable.** Every module has a datasheet stating its purpose, its source of
  truth, its inputs/outputs, its constants, and exactly what it was validated
  against. Any contributor — human or agent — can answer "what is this and how do
  we know it's right?" from the datasheet alone.
- **Human-directed.** The flow of logic and the engineering decisions are made by
  a human reviewer, in real time, one module at a time. Agents scaffold and
  explain; they do not decide. See [`AGENTS.md`](AGENTS.md).
- **Clean.** Idiomatic Go, whatsmeow-style. Comments only where they earn their
  place (a `TODO`, a stated assumption, or context that would otherwise be lost).

---

## Source of truth

The reference is the validated Rust library **`whatsapp-rust`**
(`/Users/purpshell/Documents/Programming/whatsapp-rust-voip`), which is KAT-pinned
and known to work. Its `wacore/src/voip/` is the porting target; its
`testdata/*.json` and inline `#[test]` cases are the vectors.

The **protocol rationale** lives in the wacrg spec
(`/Users/purpshell/Documents/Programming/wacrg/docs/`): `codec/mlow/`,
`keying/`, `signaling/`, `transport/`. A datasheet links to the relevant page.

Lineage (for provenance): **WhatsApp WASM → byte-exact Go reference → zapo-caller
(TS) → whatsapp-rust (Rust)**. The Rust is the most complete and the only one with
checked-in vectors.

Do **not** port from the earlier `dublin`/`meowmeow` calling code. It is the
unvalidated prior attempt and is out of scope as a source.

---

## Repository structure (mirrors whatsmeow)

```
meowcaller/
  client.go  call.go  offer.go  accept.go  ...   package meowcaller — call control
  mlow/         the MLow/SMPL CELP audio codec (own package)
  srtp/         E2E + HBH SRTP, SFrame, WARP (media keying + protection)
  rtp/          RTP / RTCP / WARP framing
  stun/         relay STUN dialect
  relay/        DTLS / SCTP media loop (pion, pure Go)
  signaling/    the <call> stanza builders/parsers
  types/        shared types and types/events/
  util/         small primitives (hkdfutil, ...) — whatsmeow-style
  internal/kat/ shared test-vector loader
  spec/         per-module datasheets
```

Pure-Go dependencies only. The codec package has **no** third-party dependencies.
Every `.go` file carries the project license header.

---

## Module registry (the spine)

Work proceeds **module by module**, in dependency order — not in phases. Each
module is a discrete, human-approved unit with its own datasheet, scaffold, and
verification. The registry and live status live in [`SPEC.md`](SPEC.md); the order
of attack:

```
codec foundation:   rangecoder → toc → mem(heap ROM)
codec receive DSP:   lpc → lsf → lsf_quant → pitch → pulse → gains → synth
                     → postfilter → vad → noise → red → decoder(e2e)
codec send DSP:      encoder(+analysis, signal-mode)
keying:              util/hkdf → srtp/e2e → srtp/hbh → srtp/sframe → srtp/warp
signaling:           signaling/stanza
transport:           stun → rtp → ssrc → relay
orchestration:       session/pipeline → client/call (+ types/events)
```

The first audible milestone is `decoder` decoding the real
`inbound_capture_frames.json` to PCM that matches `e2e_vectors.json`. Everything
before it is in service of that.

---

## Conventions (enforced)

- **Commits:** one module change per commit, subject `(<module>: <change>)` —
  e.g. `(mlow/toc: scaffold smpl TOC parser)`, `(srtp/sframe: implement GCM
  seal)`. Each commit updates [`CHANGELOG.md`](CHANGELOG.md).
- **Changelog:** every merged change recorded under the module, with its
  validation state (`scaffolded` / `implemented` / `KAT-verified`).
- **Comments:** only `// TODO(...)`, `// ASSUMPTION: ...`, or a short note that
  preserves context a future reader would otherwise lose. No narration of what the
  code plainly does. Doc comments on exported identifiers per Go convention.
- **Tests:** every module ships a KAT test loading the reference vector. `go test
  ./...` is green at every committed step.

The detailed working protocol — how an agent scaffolds, where it stops, and how
the human directs — is [`AGENTS.md`](AGENTS.md).
