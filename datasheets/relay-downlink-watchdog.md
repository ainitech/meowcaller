# Relay downlink watchdog (1:1)

**Status:** implemented; unit-tested; live retest pending

**Reference pinned at:**

- zapo `bceb268c846763a2cd0b494da830a54aacdc3818`
  (`packages/voip-media/src/relay/WaRelayDownlinkWatchdog.ts#L42-L67`,
  `packages/voip-media/src/relay/WaSctpRelay.ts#L620-L655`)
- driving change: zapo `1ebadd92b7f16457625ce50cd494f00f1c215501` (PR #311), commit "redial a WebRTC leg whose downlink stalls"

## Observed facts (zapo, measured live)

- A relay leg can keep answering pings and allocates while forwarding none of the peer's
  media; one relay answered 34 keepalives and forwarded nothing. Earlier: a leg lost on one
  side left the other side sending to the old relay, and the relay does not re-point its
  downlink to a sibling leg.
- zapo's rule: on an accepted call, a leg that hears none of the peer's authenticated RTP,
  SRTCP or app data re-allocates after 5 s and is redialled 5 s later, with a doubling wait
  (5 + 5·2ⁿ s) and at most three redials per call. Peer media clears the stall; the redial
  count carries for the call. A peer in DTX still sends RTCP, so it never trips it.

## Reference source (zapo, MIT)

```ts
onSend(): void {
    const now = this.now()
    if (this.stalledSince === null) { this.stalledSince = now; return }
    const stalledMs = Math.round(now - this.stalledSince)
    if (!this.reallocated) {
        if (stalledMs < DOWNLINK_REALLOCATE_AFTER_MS) return   // 5_000
        this.reallocated = true; this.reallocate(stalledMs); return
    }
    if (this.redials >= DOWNLINK_MAX_REDIALS) return            // 3
    const redialAfterMs = DOWNLINK_REALLOCATE_AFTER_MS + DOWNLINK_REDIAL_AFTER_MS * 2 ** this.redials
    if (stalledMs < redialAfterMs) return
    if (this.redial(stalledMs, this.redials + 1)) this.redials++
    this.stalledSince = now; this.reallocated = false
}
notePeerMedia(): void { this.stalledSince = null; this.reallocated = false }
```

## Go target

- `downlinkWatchdog.tick(now)` on every 1 Hz keepalive once `callAccepted`; `notePeerMedia`
  on each authenticated SRTCP, app-data, video and audio packet.
- `relayLink` holds the current leg; `redialRelay` dials the same endpoint, sends the
  current allocate and a ping on the new leg, then swaps it in and closes the old one.
  `Recv` blocked on the old leg continues on the new one.

## Deviations

- "Re-allocate" is a log and diag marker only: meowcaller's keepalive already re-sends the
  allocate every second on the current leg.
- One leg per call, so the redial replaces it rather than electing among siblings; the
  redial counts as attempted even when the dial fails (zapo counts only successful ones).
- Group calls are left out; their relay follows the `group_update` path.
