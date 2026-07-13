# Reliability behavior

Termcall keeps the peer connection independent from signaling after the
reliable control data channel opens. If the signaling service restarts during
an established call, audio, ASCII video, and chat continue peer-to-peer. The
call can still end through the control channel; a signaling `call.end` is sent
only when signaling remains available.

The incoming-call daemon reconnects with bounded exponential backoff. A short
connection failure does not create a tight reconnect loop, while a connection
that remains stable resets the backoff. Pending invitations and local handoffs
are deliberately discarded when signaling is lost because the restarted
in-memory server cannot prove that their call state still exists.

Pion is allowed 20 seconds to recover a disconnected route. If ICE reaches the
failed state, the caller sends a fresh ICE-restart offer over signaling; the
callee never creates a competing restart offer. The signaling call manager
keeps the same call ID, returns the call to negotiation, resets the bounded ICE
candidate counters, and accepts the new answer. If signaling is unavailable,
the existing ICE agent can still recover automatically until the grace period
expires.

ASCII video is intentionally lossy. Both outbound and inbound queues retain at
most the newest waiting frame, frames are dropped while SCTP is congested, and
periodic keyframes recover from lost deltas. A video-channel failure degrades
the call instead of ending healthy chat or audio.

Interactive sessions use the terminal alternate screen. Cleanup restores raw
input state, colors, cursor visibility, mouse tracking modes, and the original
screen on normal exit, context cancellation, signaling failure, or peer
failure.

Automated reliability coverage includes:

- ICE restart without replacing open data channels.
- Established chat continuing across signaling-server shutdown.
- Bounded control and newest-frame video backpressure.
- Cancelable daemon reconnect delays.
- Concurrent peer shutdown and goroutine-leak checks.
- Full terminal reset sequence coverage.

Network impairment beyond process-level disconnects should be exercised on a
Linux host with `tc netem` or an equivalent network namespace setup. Test at
100–300 ms latency, 1–10% packet loss, restricted bandwidth, UDP blocked, and
TURN-only routing; sandboxed unit-test environments generally cannot apply
those network controls.
