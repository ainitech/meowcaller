package signaling

import (
	"os"
	"testing"
)

// TestParseVoipSettings is the KAT for the voip_settings parser, wired to the
// captured sample blob (use_mlow_codec_v1="true", frame_ms="60",
// target_bitrate="24000"). Skipped until ParseVoipSettings lands.
func TestParseVoipSettings(t *testing.T) {
	raw, err := os.ReadFile("testdata/voip_settings_sample.json")
	if err != nil {
		t.Fatal(err)
	}
	vs, err := ParseVoipSettings(raw)
	if err != nil {
		t.Fatalf("ParseVoipSettings: %v", err)
	}
	if !vs.Present {
		t.Error("Present = false, want true")
	}
	if !vs.UseMlowCodecV1 {
		t.Error("UseMlowCodecV1 = false, want true (sample sets it true)")
	}
	if vs.FrameMs != 60 {
		t.Errorf("FrameMs = %d, want 60", vs.FrameMs)
	}
	if vs.TargetBitrate != 24000 {
		t.Errorf("TargetBitrate = %d, want 24000", vs.TargetBitrate)
	}
}

// TestParseVoipSettingsOpus pins the codec lever: use_mlow_codec_v1="false" must
// parse to UseMlowCodecV1=false. Skipped until ParseVoipSettings lands.
func TestParseVoipSettingsOpus(t *testing.T) {
	vs, err := ParseVoipSettings([]byte(`{"encode":{"use_mlow_codec_v1":"false","frame_ms":"60"}}`))
	if err != nil {
		t.Fatalf("ParseVoipSettings: %v", err)
	}
	if vs.UseMlowCodecV1 {
		t.Error("UseMlowCodecV1 = true, want false")
	}
}

// TestParseVoipSettingsRtcpKeys pins the server RTCP cadence from the captured sample and
// the video REMB gate from an inline vid_rc section.
func TestParseVoipSettingsRtcpKeys(t *testing.T) {
	raw, err := os.ReadFile("testdata/voip_settings_sample.json")
	if err != nil {
		t.Fatal(err)
	}
	vs, err := ParseVoipSettings(raw)
	if err != nil {
		t.Fatalf("ParseVoipSettings: %v", err)
	}
	if vs.RtcpIntervalMs != 1500 {
		t.Errorf("RtcpIntervalMs = %d, want 1500 (sample rc.rtcp_interval_ms)", vs.RtcpIntervalMs)
	}
	if vs.DisableRtcpRemb {
		t.Error("DisableRtcpRemb = true, want false (sample has no vid_rc)")
	}
	vs, err = ParseVoipSettings([]byte(`{"vid_rc":{"disable_rtcp_remb":"1"},"rc":{"rtcp_interval_ms":"1000"}}`))
	if err != nil {
		t.Fatalf("ParseVoipSettings: %v", err)
	}
	if !vs.DisableRtcpRemb || vs.RtcpIntervalMs != 1000 {
		t.Errorf("got disable_rtcp_remb=%v interval=%d, want true/1000", vs.DisableRtcpRemb, vs.RtcpIntervalMs)
	}
}
