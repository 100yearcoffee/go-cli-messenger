# Security model

Termcall separates admission from identity. The shared key admits a client to signaling; Ed25519 proofs authenticate canonical peer identities. TLS protects the access key and signaling metadata. WebRTC DTLS protects peer media and control channels.

Retained defenses include strict validation, bounded queues, duplicate suppression, nonce replay guards, participant/state authorization, invitation rate limits, terminal sanitization, and explicit acceptance before media starts. Unknown callers can ring but are fingerprinted; blocks are evaluated before terminal launch.

The signaling operator can observe metadata, deny service, and control routing, but cannot forge a peer proof without its private key. Shared-key holders are not individually revocable. A restart clears in-memory presence, calls, collision history, rate windows, and replay history.

Back up the private identity key securely if continuity matters. Rotate the shared key everywhere if disclosed. Use trusted TLS and keep secret files at mode `0600`.
