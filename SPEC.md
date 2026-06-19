# SPEC.md — module registry and datasheet index

The spine of the project. Every module is a discrete, human-approved unit of work
with a **datasheet** under [`spec/`](spec/) that is its single reference: what it
is, where it comes from, its API, its constants, and what it is validated against.

Build in the order below (dependency order). Status legend:
`planned` → `scaffolded` → `implemented` → `verified` (KAT passes).

## Registry

| # | Module | Package | Datasheet | Rust source | KAT | Status |
| --- | --- | --- | --- | --- | --- | --- |
| 01 | rangecoder | `mlow` | [spec/mlow-rangecoder.md](spec/mlow-rangecoder.md) | `mlow/rangecoder.rs` | `rc_vectors.json` | planned |
| 02 | toc | `mlow` | [spec/mlow-toc.md](spec/mlow-toc.md) | `mlow/toc.rs` | `toc_vectors.json` | planned |
| 03 | mem (heap ROM) | `mlow` | [spec/mlow-mem.md](spec/mlow-mem.md) | `mlow/smpl_mem.rs` | `smpl_tables.json` | planned |
| 04 | lpc | `mlow` | [spec/mlow-lpc.md](spec/mlow-lpc.md) | `mlow/smpl_lpc.rs` | `lsf_vectors.json` | planned |
| 05 | lsf | `mlow` | [spec/mlow-lsf.md](spec/mlow-lsf.md) | `mlow/smpl_decode.rs` | `lsf_vectors.json` | planned |
| 06 | lsf_quant | `mlow` | [spec/mlow-lsf_quant.md](spec/mlow-lsf_quant.md) | `mlow/smpl_lsf_quant.rs` | `lsf_quant_io.json` | planned |
| 07 | pitch | `mlow` | [spec/mlow-pitch.md](spec/mlow-pitch.md) | `mlow/smpl_pitch.rs` | `pitch_vectors.json` | planned |
| 08 | pulse | `mlow` | [spec/mlow-pulse.md](spec/mlow-pulse.md) | `mlow/smpl_pulse.rs` | `pulse_vectors.json` | planned |
| 09 | gains | `mlow` | [spec/mlow-gains.md](spec/mlow-gains.md) | `mlow/smpl_gains.rs` | `gains_vectors.json` | planned |
| 10 | synth | `mlow` | [spec/mlow-synth.md](spec/mlow-synth.md) | `mlow/smpl_synth.rs` | (e2e) | planned |
| 11 | postfilter | `mlow` | [spec/mlow-postfilter.md](spec/mlow-postfilter.md) | `mlow/smpl_*postfilter.rs`, `smpl_harmcomb.rs` | (e2e) | planned |
| 12 | vad | `mlow` | [spec/mlow-vad.md](spec/mlow-vad.md) | `mlow/smpl_vad.rs` | `vad_ground_truth.json` | planned |
| 13 | noise | `mlow` | [spec/mlow-noise.md](spec/mlow-noise.md) | `mlow/smpl_gennoise.rs` | `gennoise_vectors.json` | planned |
| 14 | red | `mlow` | [spec/mlow-red.md](spec/mlow-red.md) | `mlow/red.rs` | (inline) | planned |
| 15 | decoder | `mlow` | [spec/mlow-decoder.md](spec/mlow-decoder.md) | `mlow/decoder.rs` | `e2e_vectors.json`, `inbound_capture_frames.json` | planned |
| 16 | encoder | `mlow` | [spec/mlow-encoder.md](spec/mlow-encoder.md) | `mlow/encode.rs`, `analysis.rs` | `sigmode_ground_truth.json` | planned |
| 17 | hkdf | `util` | [spec/util-hkdf.md](spec/util-hkdf.md) | (stdlib) | RFC 5869 | planned |
| 18 | e2e_srtp | `srtp` | [spec/srtp-e2e.md](spec/srtp-e2e.md) | `e2e_srtp.rs` | inline `#test` | planned |
| 19 | hbh_srtp | `srtp` | [spec/srtp-hbh.md](spec/srtp-hbh.md) | `hbh_srtp.rs` | inline `#test` | planned |
| 20 | sframe | `srtp` | [spec/srtp-sframe.md](spec/srtp-sframe.md) | `sframe.rs` | inline `#test` | planned |
| 21 | warp | `srtp` | [spec/srtp-warp.md](spec/srtp-warp.md) | `warp.rs` | inline `#test` | planned |
| 22 | stanza | `signaling` | [spec/signaling-stanza.md](spec/signaling-stanza.md) | `stanza.rs` | inline `#test` | planned |
| 23 | stun | `stun` | [spec/stun.md](spec/stun.md) | `stun.rs` | inline `#test` | planned |
| 24 | rtp | `rtp` | [spec/rtp.md](spec/rtp.md) | `rtp.rs`, `rtcp.rs` | inline `#test` | planned |
| 25 | ssrc | `rtp` | [spec/rtp-ssrc.md](spec/rtp-ssrc.md) | `ssrc.rs` | inline `#test` | planned |
| 26 | relay | `relay` | [spec/relay.md](spec/relay.md) | `src/voip/transport.rs` | — (integration) | planned |
| 27 | session | `meowcaller` | [spec/session.md](spec/session.md) | `src/voip/session.rs` | — (integration) | planned |
| 28 | call | `meowcaller` | [spec/call.md](spec/call.md) | `src/voip/*`, `stanza.rs` | — (integration) | planned |

## Datasheet contract

Every datasheet follows [`spec/_TEMPLATE.md`](spec/_TEMPLATE.md). The exemplar is
[`spec/mlow-toc.md`](spec/mlow-toc.md) — already complete, since the smpl TOC was
fully recovered and KAT-verified. Use it as the quality bar.

A datasheet exists **before** a module is scaffolded. It is the thing an agent
reads to be able to explain the module, and the thing the human audits to approve
the approach.
