# voip — meowcaller calling-layer demo

A small cross-platform CLI that drives the meowcaller calling layer with a real mic
and speaker. It's a **separate Go module** so its audio/WhatsApp dependencies
(miniaudio, whatsmeow, sqlite) stay out of the library.

Audio is captured/played through [miniaudio](https://github.com/gen2brain/malgo),
which picks the OS default backend (CoreAudio / WASAPI / ALSA / PulseAudio), so it
runs on macOS, Linux and Windows with no per-OS device wiring. (cgo is required — a
C compiler must be on PATH.)

## Commands

```
voip loopback            Mic → MLow encode → E2E-SRTP protect (RTP WARP header +
                         WARP MI tag) → unprotect → MLow decode → speaker.
                         No WhatsApp connection: it exercises the whole codec +
                         keying + framing stack on live audio. Talk and you hear
                         yourself back through the full pipeline.

voip call <target>       Log in (QR on first run), resolve the peer LID, discover
                         the peer's devices, encrypt a fresh callKey per device, and
                         send a <call><offer>.
                         <target> = a phone number (+15551234567), a phone JID
                         (15551234567@s.whatsapp.net), or a LID JID (...@lid).

voip listen              Log in and print incoming call signaling.

voip autoaccept          Log in and auto-accept incoming calls: decrypt the offer's
                         callKey and reply preaccept + accept.
```

Run from this directory:

```sh
go run . loopback
go run . call +15551234567
go run . autoaccept
```

## What's wired

`loopback` is the honest end-to-end exercise of everything in the library: `mlow`
encode/decode, `MediaPipeline` (RTP WARP framing + E2E-SRTP + WARP MI tag), and the
recv-side ROC tracker — all on real audio, no network.

`call` and `autoaccept` drive the full signaling path against a real account:

1. **Login** — whatsmeow session in a local `wa-voip.db` (QR pairing on first run).
2. **LID resolution** — a phone target is mapped to the peer's `@lid` via the LID
   store, seeded by a usync query when not cached. The call's E2E keys and SSRCs
   derive from the LID, so this happens *before* the offer.
3. **Device discovery** — `GetUserDevices` lists the peer's devices.
4. **callKey encryption** — the 32-byte callKey is wrapped as the Signal message
   `Message{Call{CallKey}}` and encrypted to each device's Signal session
   (`EncryptMessageForDevice`, fetching a pre-key bundle when there's no session
   yet). `autoaccept` does the reverse: it decrypts the inbound offer's `<enc>`.
5. **Offer / answer** — `signaling.BuildOffer` (and `BuildPreaccept`/`BuildAccept`
   for answering) assemble the call-control stanzas with the load-bearing child order.

## What's left for a live media call

After the peer accepts, audio flows over the relay. That last hop —
`relay.ConnectRelayMedia` (the pion DTLS/SCTP/DataChannel transport) feeding the
same `MediaPipeline` that `loopback` proves — is the remaining wiring; the transport
itself is validated only against a real relay. The signaling, keying, LID
resolution, callKey encryption and codec are all in place.

## Notes

- Requires cgo (a C compiler) for miniaudio. The Signal/WhatsApp pieces use
  whatsmeow's low-level `DangerousInternals` (the only entry point for raw call
  nodes and per-device encryption) — expected for call signaling.
- All dependencies are public; `loopback` needs only `malgo` + the library.
