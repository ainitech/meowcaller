# Changelog

All notable changes to meowcaller, tracked per module. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/). Each entry notes the module's
**validation state**: `scaffolded` (signatures + KAT test, bodies are TODO),
`implemented` (bodies written), or `KAT-verified` (its reference vector passes).

## [Unreleased]

### Planning
- Project framework: `PLAN.md` (engineering plan), `AGENTS.md` (human-audited
  module-by-module build protocol), `SPEC.md` (module registry + datasheet index),
  per-module datasheets under `spec/`.

<!--
Entry template (newest first), grouped by module:

### mlow/toc
- KAT-verified: smpl TOC parser matches toc_vectors.json (256/256 byte values).
- implemented: ParseSmplTOC body.
- scaffolded: SmplTOC type + ParseSmplTOC signature + KAT test.
-->
