# Audio

Termcall carries audio as a standard WebRTC Opus RTP track. Microphone capture
and speaker playback use GStreamer processes isolated in
`internal/client/media/gstreamer`.

The initial profile is mono capture at 48 kHz, 20 ms Opus packets, and a 32
Kbit/s target bitrate. In-band forward error correction is enabled. WebRTC's
Opus RTP declaration uses two channels as required by its negotiated codec
profile; the encoded microphone signal remains mono.

GStreamer RTP is framed over local process pipes with the two-byte RFC 4571
length prefix. Audio queues hold at most 200 ms. When processing falls behind,
old packets are discarded instead of accumulating delayed audio.

## Controls

- `/mute` or `m` toggles local microphone transmission.
- `/status` reports whether local audio is on, muted, or disabled.
- `--audio=false` disables capture, playback, and the WebRTC audio track.
- `--microphone NAME` selects a PulseAudio/PipeWire source.
- `--speaker NAME` selects a PulseAudio/PipeWire sink.
- `--audio-bitrate RATE` accepts 24000–40000 bits per second.

Use `termcall devices` to show GStreamer audio devices and `termcall doctor` to
verify the required plugins, perform a synthetic Opus encode/decode self-test,
and check device discovery.
