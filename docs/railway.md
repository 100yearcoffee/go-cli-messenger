# Railway signaling deployment

`railway.json` builds only `cmd/termcall-signald`, starts it on `0.0.0.0:$PORT`, and checks `/healthz`. Set `TERMCALL_ACCESS_KEY` and optional comma-separated `TERMCALL_STUN_URLS`. Railway terminates public TLS, so clients use its `wss://.../v1/ws` URL. Run one replica because signaling state is in memory.
