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
	salt := binary.LittleEndian.AppendUint32(nil, slotWord)
	okm, err := util.HKDFSHA256(salt, []byte(callID), []byte(lid), 4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(okm), nil
}

// DeriveWasmRelayStreamSsrcs derives all 9 relay-stream SSRCs in slot order.
func DeriveWasmRelayStreamSsrcs(callID, lid string) ([9]uint32, error) {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/ssrc.rs#L20-L22
	var out [9]uint32
	for i, slot := range WasmRelayStreamSlotWords {
		ssrc, err := DeriveWasmParticipantSsrc(callID, lid, slot)
		if err != nil {
			return [9]uint32{}, err
		}
		out[i] = ssrc
	}
	return out, nil
}

// FormatE2ESrtpParticipantID formats the device-qualified LID for E2E-SRTP HKDF info.
func FormatE2ESrtpParticipantID(jid string) string {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/ssrc.rs#L28-L30
	return util.FormatParticipantID(jid)
}

// E2EParticipantIDVariants lists the device-qualified LID variants the recv path
// tries as HKDF info (peer sender LIDs), deduplicated in insertion order.
func E2EParticipantIDVariants(jid string) []string {
	// Source of truth: https://github.com/oxidezap/whatsapp-rust/blob/41095d4e6ba4610e054e9ede3af1d5e88a83faee/wacore/src/voip/ssrc.rs#L33-L56
	var out []string
	seen := make(map[string]bool)
	push := func(s string) {
		t := strings.TrimSpace(s)
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	bare, _, _ := strings.Cut(jid, "/")
	bare = strings.TrimSpace(bare)
	push(bare)
	push(FormatE2ESrtpParticipantID(jid))
	if at := strings.LastIndexByte(bare, '@'); at > 0 {
		user := bare[:at]
		domain := bare[at+1:]
		if domain == "lid" && strings.Contains(user, ":") {
			base, _, _ := strings.Cut(user, ":")
			push(base + ":0@" + domain)
			push(base + "@" + domain)
		}
	}
	return out
}
