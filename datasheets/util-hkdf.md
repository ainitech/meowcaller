<!-- Datasheet = three things only: the reference source VERBATIM, the Go envelope
     (signatures, no bodies), and implementation suggestions. No behavioral summary,
     no implementation. The verbatim source is the only authoritative content. -->

# Datasheet: `util/hkdf`

HKDF-SHA256 extract-and-expand. Keying layer: the single key-derivation
primitive that every VoIP key schedule (SRTP session keys, SFrame keys, WARP
auth key) reduces to.

**Validation vector:** RFC 5869 Appendix A — the published HKDF-SHA256
extract-and-expand test cases (Test Case 1, 2, 3). Copy the chosen vectors into
`util/testdata/` as a JSON file holding `IKM`, `salt`, `info`, `L`, and the
expected `OKM` (all hex).

## Reference source (verbatim — authoritative)


```rust
/// HKDF-SHA256 (extract with `salt`, expand with `info`) — the one KDF shape all of
/// WhatsApp's VoIP key derivations reduce to. Mirrors the hand-rolled `hkdf`/`waSfuKdf`
/// helpers in the TS source.
pub(crate) fn hkdf_sha256(salt: &[u8], ikm: &[u8], info: &[u8], len: usize) -> Vec<u8> {
    let hk = Hkdf::<Sha256>::new(Some(salt), ikm);
    let mut okm = vec![0u8; len];
    hk.expand(info, &mut okm)
        .expect("HKDF length within bounds");
    okm
}
```

## Go envelope (signatures only)

The corresponding Go declarations — exported types and function **signatures with
no bodies**. This is the surface to implement; it is not the implementation.

```go
package util

func HKDFSHA256(salt, ikm, info []byte, length int) []byte
```

## Implementation suggestions (guidance, not authoritative)

- The standard library covers this: `golang.org/x/crypto/hkdf` with `crypto/sha256`
  for the hash. Construct with `hkdf.New(sha256.New, ikm, salt, info)` then read
  `length` bytes.
- `usize` length → Go `int`. Return `[]byte` of exactly `length` bytes.
- The reference panics ("expect") when the requested length exceeds the HKDF bound
  (255 * HashLen = 8160 bytes for SHA-256). All call sites in this codebase request
  far less, so a panic on overflow is acceptable; alternatively return an error.
  `TODO(human):` decide panic vs. error for the out-of-bounds length case.
- `salt` is passed straight through. When a caller wants the RFC "no salt" behavior,
  it passes an explicit zero/empty slice; do not special-case `nil` differently from
  the reference unless a vector forces it.
- Allocate the output buffer up front at `length` bytes and fill it; do not grow.
