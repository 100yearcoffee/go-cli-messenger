# Signaling deployment

The signaling service keeps presence, calls, replay guards, duplicate tracking, and rate limits in memory and must run as a single replica. It stores no accounts, passwords, sessions, devices, or revocation state.

Admission uses one shared access key. Set `TERMCALL_ACCESS_KEY` or pass `--access-key-file` pointing to a `0600` file. Keys contain 24–1024 bytes; the file flag overrides the environment. Every WebSocket upgrade and TURN credential request requires `Authorization: Bearer <key>` when configured; health endpoints stay public. Keys are compared in constant time and never logged.

The service refuses a non-loopback listener without a key unless `--open` is explicit. Loopback is open by default. Use `wss://` remotely; termcall refuses to send the key over remote plaintext `ws://`.

All key holders can enter signaling but cannot claim another address without its Ed25519 key. There is no individual revocation: compromise requires rotating the shared key on the server and every client. Direct TLS flags remain available; a reverse proxy or Railway-managed TLS is the simple public path.
