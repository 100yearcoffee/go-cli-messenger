# TURN integration status

TURN credential code remains supported, but coturn hosting is not required for the simple signaling deployment.

Configure signaling with `--turn-secret-file`, one or more `--turn` URLs, and optional `--stun`. TURN issuance requires the shared signaling key. `GET /v1/turn-credentials` uses the same bearer key and creates a random allocation subject rather than an account name. The client feeds returned credentials into ICE and rejects expired or incomplete responses.

The existing coturn compose template remains for operators needing relay fallback. Its REST secret is distinct from `TERMCALL_ACCESS_KEY`.
