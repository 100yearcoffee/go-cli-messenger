# Identity and trust

Protocol v2 uses an installation-owned Ed25519 key instead of an account. `termcall init <base-name>` creates `~/.config/termcall/identity.json` with mode `0600`; later `init` runs update the profile but refuse to replace that key.

The full fingerprint is lowercase, unpadded Base32 of `SHA-256(public_key)`. An address is `<base-name>-<first-12-fingerprint-characters>`. The suffix is computed locally and is never user-selected. Moving the same identity to another machine currently requires securely backing up both the identity and profile files; export/import tooling is not provided.

`session.hello`, `call.invite`, and `call.accept` carry a signed proof binding protocol version, message type, call ID, sender, recipient, public key, expiration, and nonce. The server binds the verified address to the WebSocket and rejects replay, key/suffix mismatch, and short-suffix collisions. Peers independently verify invitations and acceptances.

Trust is indexed by full fingerprint, not address. Unknown callers may ring, but are labeled `UNKNOWN`; accepting does not trust them. A trusted fingerprint remains trusted across servers or aliases. Alias reuse by another fingerprint produces a warning and remains unknown.

Use `termcall trust <fingerprint>`, `untrust`, `block`, or `unblock`. A full fingerprint can be pre-trusted; a prefix works only when it uniquely identifies an observed record. Blocked proofs are declined before a terminal opens.

Loss of the private key means loss of the identity. Disclosure permits impersonation and requires a new identity; there is no server-side device revocation.
