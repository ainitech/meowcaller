# Relaylatency answer (callee side)

**Status:** implemented; unit-tested; live retest pending

**Reference pinned at:**

- zapo `87dd5b0cdd5e7e0c40b209b3bc7d47b0043d2349` (`packages/voip/src/call/WaCallMediaSession.ts#L1103-L1148`)
- driving change: zapo `808f920fd1e2800a0a43d8c6ab53eed4983e02f0` (PR #288, "fix silent and one-way audio on incoming calls")
- whatsapp-rust HEAD lists `relay_latency` as an unwired builder: the reference sends no answer at all

## Observed facts (from the zapo commit message, measured live)

- The caller's `<relaylatency>` carries one `<te relay_name latency>` per relay the
  caller probed, with the caller's own RTT and the caller's view of the address.
- Echoing those `<te>` nodes back reports the peer's latencies as ours, for relays we
  were never given. The caller then elected such a relay as the best common one and
  moved its uplink there after the first few packets, leaving the call one-way.
- The relay `te2` endpoints in the offer/ack carry a server-measured `c2r_rtt`
  attribute; zapo answers with that value for the relays it dials, with its own
  endpoint bytes, and sends nothing if the peer named none of them.

## Reference source (zapo, MIT)

```ts
const ownByName = new Map<string, { latency: number; address: Uint8Array }>()
for (const ep of dialableRelayEndpoints(this.info.relayData?.endpoints ?? [])) {
    if (!ep.relayName || !ep.addressBytes || ownByName.has(ep.relayName)) continue
    ownByName.set(ep.relayName, { latency: ep.c2rRtt || 0, address: ep.addressBytes })
}
const teNodes: BinaryNode[] = []
for (const te of getNodeChildrenByTag(inner, 'te')) {
    const name = te.attrs?.relay_name
    const own = name ? ownByName.get(name) : undefined
    if (!name || !own) continue
    teNodes.push({
        tag: 'te',
        attrs: { relay_name: name, latency: String(0x2000000 + own.latency) },
        content: own.address
    })
}
if (teNodes.length === 0) return
```

## Go target

- `relayEndpoint` gains `c2rRTTMs` (`te2 c2r_rtt`) and `addrBytes` (raw 6-byte content).
- `relayLatencyAnswers(rl, rd, inbound)` returns at most one `rlProbe`: the relay
  `getMediaRelayEndpoint` dials, when a peer `<te>` names it.
- `onRelayLatency` sends one `<relaylatency>` per answer and nothing otherwise.

## Deviation from zapo

meowcaller dials exactly one relay (the inbound FNA endpoint), so the answer set is
one entry at most, where zapo answers for every relay it dials.
