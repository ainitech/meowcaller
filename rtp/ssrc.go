package rtp

import (
	"encoding/binary"
	"strings"

	"github.com/purpshell/meowcaller/util"
)

// SSRC derivation and participant-LID helpers for E2E HKDF info.

// WasmRelayStreamSlotWords are the slot words for the 9-stream relay allocate plan.
var WasmRelayStreamSlotWords = [9]uint32{0, 1, 4, 2, 3, 5, 7, 8, 6}

// DeriveWasmParticipantSsrc derives a participant/stream SSRC:
// HKDF-SHA256(salt=slotWord LE32, ikm=callID, info=lid, 4), read back as LE u32.
func DeriveWasmParticipantSsrc(callID, lid string, slotWord uint32) (uint32, error) {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/ssrc.rs#L11-L17
	// TODO
	// agent suggestion: salt = LE32(slotWord); okm, err := util.HKDFSHA256(salt, []byte(callID),
	// []byte(lid), 4); if err return 0, err; return binary.LittleEndian.Uint32(okm), nil.
	// human input:
	return 0, nil
}

// DeriveWasmRelayStreamSsrcs derives all 9 relay-stream SSRCs in slot order.
func DeriveWasmRelayStreamSsrcs(callID, lid string) ([9]uint32, error) {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/ssrc.rs#L20-L22
	// TODO
	// agent suggestion: map each slot in WasmRelayStreamSlotWords through DeriveWasmParticipantSsrc,
	// bubbling the first error.
	// human input:
	return [9]uint32{}, nil
}

// FormatE2ESrtpParticipantID formats the device-qualified LID for E2E-SRTP HKDF info.
func FormatE2ESrtpParticipantID(jid string) string {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/ssrc.rs#L28-L30
	// TODO
	// agent suggestion: return util.FormatParticipantID(jid) (same surface as SFrame today).
	// human input:
	return ""
}

// E2EParticipantIDVariants lists the device-qualified LID variants the recv path
// tries as HKDF info (peer sender LIDs), deduplicated in insertion order.
func E2EParticipantIDVariants(jid string) []string {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/ssrc.rs#L33-L56
	// TODO
	// agent suggestion: push(trim(bare)) and push(FormatE2ESrtpParticipantID(jid)) with empty/dup
	// skipped; if bare has '@'(at>0), domain=="lid" and user has ':', base=user before ':', push
	// base+":0@lid" and base+"@lid".
	// human input:
	return nil
}

var (
	_ = binary.LittleEndian
	_ = strings.TrimSpace
	_ = util.FormatParticipantID
)
