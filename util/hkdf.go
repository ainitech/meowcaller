package util

import (
	"crypto/hkdf"
	"crypto/sha256"
)

// HKDFSHA256 derives length bytes of key material from ikm using HKDF-SHA256
// (RFC 5869): an HMAC-SHA256 extract keyed by salt, then expand with info. Every
// VoIP key schedule (SRTP session keys, SFrame keys, the WARP auth key) reduces
// to this one shape.
func HKDFSHA256(salt, ikm, info []byte, length int) []byte {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/mod.rs#L32-L39
	okm, err := hkdf.Key(sha256.New, ikm, salt, string(info), length)
	if err != nil {
		panic("HKDF length within bounds")
	}
	return okm
}
