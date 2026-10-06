package meowcaller

import (
	"bytes"
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"
)

func relayNodeForLatencyTest() *waBinary.Node {
	return &waBinary.Node{Tag: "relay", Content: []waBinary.Node{
		{Tag: "key", Content: []byte("k")},
		{Tag: "token", Attrs: waBinary.Attrs{"id": "0"}, Content: []byte("t")},
		{Tag: "te2", Attrs: waBinary.Attrs{
			"relay_id": "7", "relay_name": "gru1c02", "token_id": "0", "auth_token_id": "0",
			"is_fna": "1", "c2r_rtt": "23",
		}, Content: []byte{10, 0, 0, 1, 0x0d, 0x98}},
		{Tag: "te2", Attrs: waBinary.Attrs{
			"relay_id": "9", "relay_name": "sao2c01", "token_id": "0", "auth_token_id": "3",
			"c2r_rtt": "41",
		}, Content: []byte{10, 0, 0, 2, 0x0d, 0x98}},
	}}
}

func probesNode(names ...string) *waBinary.Node {
	kids := make([]waBinary.Node, 0, len(names))
	for _, name := range names {
		kids = append(kids, waBinary.Node{
			Tag:     "te",
			Attrs:   waBinary.Attrs{"relay_name": name, "latency": "33554477"},
			Content: []byte{192, 168, 0, 1, 0x0d, 0x96},
		})
	}
	return &waBinary.Node{Tag: "relaylatency", Content: kids}
}

func TestRelayLatencyAnswersOnlyTheDialedRelayWithOwnRTT(t *testing.T) {
	rd := parseRelayData(relayNodeForLatencyTest())
	answers := relayLatencyAnswers(probesNode("sao2c01", "gru1c02", "gru1c02", "ams1c05"), rd, true)
	if len(answers) != 1 {
		t.Fatalf("answers = %d, want exactly one for the dialed relay", len(answers))
	}
	got := answers[0]
	if got.relayName != "gru1c02" {
		t.Errorf("relay_name = %q, want the inbound FNA relay gru1c02", got.relayName)
	}
	if got.latency != 23 {
		t.Errorf("latency = %d, want our own c2r_rtt 23, never the peer's probe value", got.latency)
	}
	if want := []byte{10, 0, 0, 1, 0x0d, 0x98}; !bytes.Equal(got.addr, want) {
		t.Errorf("addr = %x, want our te2 endpoint bytes %x, never the peer's", got.addr, want)
	}
}

func TestRelayLatencyAnswersNothingForRelaysWeDoNotDial(t *testing.T) {
	rd := parseRelayData(relayNodeForLatencyTest())
	if answers := relayLatencyAnswers(probesNode("sao2c01", "ams1c05"), rd, true); len(answers) != 0 {
		t.Fatalf("answers = %+v, want none when the peer names only relays we are not on", answers)
	}
	if answers := relayLatencyAnswers(probesNode("gru1c02"), nil, true); len(answers) != 0 {
		t.Fatalf("answers = %+v, want none before relay data is known", answers)
	}
}
