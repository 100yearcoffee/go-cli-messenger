# Termcall

Termcall is a peer-to-peer terminal calling application for Linux. It uses a small signaling service to introduce callers, then carries text chat, audio, and ASCII video directly between peers over WebRTC.

Identity is based on a locally generated Ed25519 key—not an account or password. Each installation gets a canonical address such as `alice-abc234def567`, and callers can verify, trust, or block the full key fingerprint.

## Build

Termcall requires Go 1.26 or newer. Audio and camera features also require the relevant Linux GStreamer plugins and devices.

```sh
make build
```

This creates `bin/termcall` and `bin/termcall-signald`.

## Run locally

Start the signaling service:

```sh
./bin/termcall-signald
```

Initialize two clients in separate user profiles. A blank access key is allowed for the local open server:

```sh
./bin/termcall init alice --server ws://127.0.0.1:8080/v1/ws
./bin/termcall identity
```

On the receiving machine or profile:

```sh
./bin/termcall listen
```

Call the canonical address printed by the other client:

```sh
./bin/termcall chat bob-xxxxxxxxxxxx
```

Use `--video=false --audio=false` for text-only testing. Run `termcall daemon` instead of `listen` for reconnecting background signaling and incoming-call handoff.

## Deploy to Railway

The MVP needs only one Railway service running `termcall-signald`. The included [`railway.json`](railway.json) builds the signaling binary, binds it to Railway's `$PORT`, and uses `/healthz` for health checks.

Configure these Railway variables:

```text
TERMCALL_ACCESS_KEY=<a random value of at least 24 bytes>
TERMCALL_STUN_URLS=stun:stun.l.google.com:19302
```

Deploy one replica because signaling state is currently held in memory. Railway provides public TLS; initialize clients with its WebSocket URL and the same access key:

```sh
TERMCALL_ACCESS_KEY='<shared-key>' \
  ./bin/termcall init alice --server wss://<railway-domain>/v1/ws
```

The shared key controls admission to the server. Peer identity is still proven independently with Ed25519 signatures, so knowing the shared key does not let someone impersonate another canonical address.

## Deferred for the MVP

### TURN and coturn deployment

STUN helps peers discover a direct route through common home NATs. TURN relays media when no direct route works; coturn is the server normally used to provide that relay.

TURN hosting is deferred so the MVP remains one inexpensive Railway service with no separate media relay. The TURN client and credential paths remain implemented for later use.

The sacrifice is connection reliability: calls can fail on restrictive corporate, hotel, mobile, symmetric-NAT, or UDP-blocked networks. TURN should be deployed before broad production use, and it adds operational complexity and media-bandwidth cost.

### Durable and horizontally scaled signaling

Presence, active calls, replay protection, and rate-limit state are currently in memory. This keeps MVP deployment simple, but requires one signaling replica and loses active signaling state on restart. Established peer sessions can continue after signaling loss.

Durable/distributed state is deferred until usage justifies the database or coordination infrastructure.

### Identity export and recovery

The private identity key is installation-based. Export/import and recovery tooling is deferred, so users must securely back up the local identity file themselves or receive a new address after losing it.

See [`docs/identity-security.md`](docs/identity-security.md) and [`docs/public-signaling.md`](docs/public-signaling.md) for the detailed security and deployment model.
