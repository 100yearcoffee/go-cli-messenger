# Incoming calls

After initialization, run `termcall listen` or the reconnecting `termcall daemon`. The daemon can print calls, open a terminal, ignore them, or decline in do-not-disturb mode.

Every invitation is verified before display. Unknown identities show `UNKNOWN` and the full fingerprint; accepting does not add trust. Blocked fingerprints are declined before terminal launch. Use `termcall answer <call-id>` or `decline <call-id>` with the daemon.

The user explicitly accepts before camera or microphone capture starts. If signaling disappears after WebRTC is established, the peer session continues.
