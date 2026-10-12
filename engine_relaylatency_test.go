package meowcaller

import (
	"bytes"
	"context"
	"testing"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
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

func TestOwnRelayLatencyNamesTheDialedRelayWithServerRTT(t *testing.T) {
	rd := parseRelayData(relayNodeForLatencyTest())
	own, ok := ownRelayLatency(rd, true)
	if !ok {
		t.Fatal("no own relay latency for a parsed relay block")
	}
	if own.relayName != "gru1c02" || own.latency != 23 {
		t.Fatalf("own = %+v, want the inbound FNA relay gru1c02 with c2r_rtt 23", own)
	}
	if want := []byte{10, 0, 0, 1, 0x0d, 0x98}; !bytes.Equal(own.addr, want) {
		t.Fatalf("addr = %x, want our te2 endpoint bytes %x", own.addr, want)
	}
	if _, ok := ownRelayLatency(nil, true); ok {
		t.Fatal("no relay data must yield nothing to report")
	}
}

func testEngineWithIncomingCall() (*engine, *Call) {
	c := &Client{}
	c.eng = newEngine(c)
	call := &Call{eng: c.eng, id: "CID", peer: peerJID(), phase: CallPhaseRinging}
	c.eng.calls["CID"] = &engineCall{
		call:      call,
		direction: CallDirectionIncoming,
		from:      peerJID(),
		creator:   creatorJID(),
		relay:     parseRelayData(relayNodeForLatencyTest()),
	}
	return c.eng, call
}

func TestAnnounceRelayLatencySendsOnceAfterTheRelayAnswered(t *testing.T) {
	eng, call := testEngineWithIncomingCall()
	var sent []waBinary.Node
	eng.sendCallNode = func(_ context.Context, node waBinary.Node) error {
		sent = append(sent, node)
		return nil
	}
	eng.announceRelayLatency(call.id)
	eng.announceRelayLatency(call.id)
	if len(sent) != 1 {
		t.Fatalf("sent %d relaylatency stanzas, want exactly one per call", len(sent))
	}
	action, ok := sent[0].Content.([]waBinary.Node)
	if !ok || len(action) != 1 || action[0].Tag != "relaylatency" {
		t.Fatalf("stanza content = %+v, want one <relaylatency>", sent[0].Content)
	}
	kids := action[0].GetChildren()
	if len(kids) != 1 || kids[0].Tag != "te" {
		t.Fatalf("relaylatency children = %+v, want one <te>", kids)
	}
	ag := kids[0].AttrGetter()
	if ag.String("relay_name") != "gru1c02" || ag.String("latency") != "33554455" {
		t.Fatalf("te attrs = %v, want gru1c02 with latency 0x2000000+23", kids[0].Attrs)
	}
	if to := sent[0].AttrGetter().JID("to"); to != peerJID() {
		t.Fatalf("to = %s, want the offer sender %s", to, peerJID())
	}
}

func TestAnnounceRelayLatencyStaysSilentForOutgoingAndWithoutRelay(t *testing.T) {
	eng, call := testEngineWithOutgoingCall()
	eng.sendCallNode = func(context.Context, waBinary.Node) error {
		t.Fatal("an outgoing call must not announce relay latency")
		return nil
	}
	eng.announceRelayLatency(call.id)

	eng2, call2 := testEngineWithIncomingCall()
	eng2.calls[call2.id].relay = nil
	eng2.sendCallNode = func(context.Context, waBinary.Node) error {
		t.Fatal("no relay data means nothing to report")
		return nil
	}
	eng2.announceRelayLatency(call2.id)
	if eng2.calls[call2.id].relayAnnounced {
		t.Fatal("nothing was announced, so the flag must stay clear")
	}
}

func TestInboundRelayLatencyReportIsNeverAnswered(t *testing.T) {
	eng, call := testEngineWithIncomingCall()
	eng.sendCallNode = func(context.Context, waBinary.Node) error {
		t.Fatal("a received relaylatency must never be answered")
		return nil
	}
	eng.onRelayLatency(&events.CallRelayLatency{BasicCallMeta: types.BasicCallMeta{CallID: call.id, From: peerJID(), CallCreator: creatorJID()}, Data: &waBinary.Node{Tag: "call", Content: []waBinary.Node{{
		Tag: "relaylatency", Content: []waBinary.Node{{Tag: "te", Attrs: waBinary.Attrs{"relay_name": "gru1c02", "latency": "33554477"}}},
	}}}})
}
