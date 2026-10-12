# Relaylatency (callee side)

**Status:** implemented; unit-tested; live retest pending

**Reference pinned at:**

- zapo `3c594911144e09951bd58f084f40162145bde9cc`
  (`packages/voip/src/call/WaCallMediaSession.ts#L719-L780` own report, `#L1150-L1172`
  received report)
- driving changes: zapo `fa528f1df427e60c3cf820a44522207dcdf4e968` (PR #305, "never answer
  relaylatency and announce each relay once") and `04073f8834ffee819078fec8bd2dc315f8f374bc`
  (PR #309, "announce only the relays that answered")
- whatsapp-rust HEAD lists `relay_latency` as an unwired builder: the reference sends nothing

## Observed facts (zapo, measured live)

- WhatsApp only reads a received `<relaylatency>` to feed its relay election and never
  answers one. Two clients that answered each other looped for the whole call: 549 stanzas
  in 140 s.
- Every relay that passed the dial rule used to be named, including one never reached. The
  Android client chose that relay, the lowest latency from its own network, sent all its
  media there, and nothing of it reached the other side. The official clients only report
  relays they measured.
- The report names each relay once per call with the server-measured `c2r_rtt` of the
  `te2` endpoint and that endpoint's own address bytes.

## Reference source (zapo, MIT)

```ts
// a received report is stored, nothing is sent back
handleCallRelaylatency(node, peerJid) {
    for (const te of getNodeChildrenByTag(inner, 'te')) { ...this.peerRelayLatencies.set(name, latency) }
}
// our own report: only relays we dial and that answered, once per name
private dialableRelayLatencies() {
    for (const ep of dialableRelayEndpoints(endpoints)) {
        if (!this.answeredRelays.has(ep.relayName)) continue
        relays.push({ relayName: ep.relayName, latency: ep.c2rRtt ?? ..., addressBytes: ep.addressBytes })
    }
}
```

## Go target

- `onRelayLatency`: records the peer's relay names at debug; sends nothing.
- `announceRelayLatency(callID)`: inbound calls only, once per call (`relayAnnounced`),
  called from the media loop on the first STUN allocate-success from the relay we dial;
  the `<te>` carries that relay's name, `c2r_rtt` and address bytes.
- `ownRelayLatency(rd, inbound)`: the one entry this client may report.

## Deviations

- meowcaller dials one relay, so the report has one entry; zapo names every relay that
  answered.
- The outgoing side sends neither `<relaylatency>` nor `<transport>` today, unchanged by
  this module.
