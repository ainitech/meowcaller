package signaling

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/rs/zerolog"
)

// VoipSettings is the codec-relevant subset of the server's <voip_settings> JSON
// blob, delivered inline on an inbound <offer> and in the <ack> of an outbound
// offer. meowcaller reads it to choose the per-call audio codec.
//
// This is meowcaller-original glue: the whatsapp-rust reference does not parse
// voip_settings to pick a codec — it steers onto RFC Opus by advertising only
// <audio rate=8000> (see BuildAccept's audio_rates). Reading use_mlow_codec_v1 is
// a meowcaller-specific lever, so the parser carries no // Source of truth: port.
type VoipSettings struct {
	// UseMlowCodecV1 mirrors encode.use_mlow_codec_v1: true => Meta's 16 kHz MLow
	// codec, false => RFC Opus. Absent => true (MLow), matching current behavior.
	UseMlowCodecV1 bool
	// FrameMs mirrors encode.frame_ms (audio frame duration in ms); 0 if absent.
	FrameMs int
	// TargetBitrate mirrors rc.target_bitrate in bits/s; 0 if absent.
	TargetBitrate int
	// RtcpIntervalMs mirrors rc.rtcp_interval_ms, the server's RTCP report cadence; 0 if absent.
	RtcpIntervalMs int
	// DisableRtcpRemb mirrors vid_rc.disable_rtcp_remb: the server turned REMB over RTCP off
	// for video, leaving the in-stream RTP header extension as the only receiver estimate.
	DisableRtcpRemb bool
	// Present reports whether a non-empty voip_settings blob was parsed.
	Present bool
}

// ParseVoipSettings parses the <voip_settings> JSON content into the codec-relevant
// subset. An empty blob yields the zero VoipSettings (MLow default); malformed JSON
// is an error. The values are stringly-typed on the wire ("true"/"60"), so each is
// converted explicitly; use_mlow_codec_v1 defaults to true (MLow) unless the key is
// the literal "false".
func ParseVoipSettings(raw []byte, log ...zerolog.Logger) (*VoipSettings, error) {
	lg := pickLog(log)
	if len(bytes.TrimSpace(raw)) == 0 {
		return &VoipSettings{UseMlowCodecV1: true}, nil
	}
	var doc struct {
		Encode struct {
			UseMlowCodecV1 string `json:"use_mlow_codec_v1"`
			FrameMs        string `json:"frame_ms"`
		} `json:"encode"`
		RC struct {
			TargetBitrate  string `json:"target_bitrate"`
			RtcpIntervalMs string `json:"rtcp_interval_ms"`
		} `json:"rc"`
		VidRC struct {
			DisableRtcpRemb string `json:"disable_rtcp_remb"`
		} `json:"vid_rc"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		lg.Warn().Int("bytes", len(raw)).Err(err).Msg("malformed voip_settings json")
		return nil, fmt.Errorf("signaling: parse voip_settings: %w", err)
	}
	vs := &VoipSettings{
		UseMlowCodecV1: doc.Encode.UseMlowCodecV1 != "false",
		FrameMs:        atoiOrZero(doc.Encode.FrameMs),
		TargetBitrate:  atoiOrZero(doc.RC.TargetBitrate),
		RtcpIntervalMs: atoiOrZero(doc.RC.RtcpIntervalMs),
		// Source of truth: https://github.com/vinikjkkj/zapo/blob/87dd5b0cdd5e7e0c40b209b3bc7d47b0043d2349/packages/voip/src/signaling/voip-settings.ts#L40-L50
		DisableRtcpRemb: doc.VidRC.DisableRtcpRemb == "1" || strings.EqualFold(doc.VidRC.DisableRtcpRemb, "true"),
		Present:         true,
	}
	lg.Debug().
		Bool("use_mlow_codec_v1", vs.UseMlowCodecV1).
		Int("frame_ms", vs.FrameMs).
		Int("target_bitrate", vs.TargetBitrate).
		Int("rtcp_interval_ms", vs.RtcpIntervalMs).
		Bool("disable_rtcp_remb", vs.DisableRtcpRemb).
		Msg("parsed voip_settings")
	return vs, nil
}

// atoiOrZero parses a base-10 int, returning 0 for an absent or unparseable value.
func atoiOrZero(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
