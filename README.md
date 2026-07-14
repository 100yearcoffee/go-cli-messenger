# Why?

I bet you've seen those hacker movies/animes where someone is using a laptop and a terminal screen pops up with a message in it, and the main character starts chatting with someone. THIS is the reason for this project.

This command is exactly for it:

```sh
./bin/termcall daemon --detached --incoming open-terminal
```

Just for fun, for friends and maybe your coworkers (if you're not in big tech that will probably yell at ya)

TBH I wanted P2P as I was thinking about no servers, etc, but if you're not in the same network it's hard to achieve it. You still need something that will help you get around NAT, but since you need a server (like here) maybe it makes no sense for P2P here, idk.

There is a video stream that gets morphed into ASCII in terminal but, meh.
I will probably remove it, too much hustle and you need drivers to support that.

For it to work follow the intructions below, the go server is easy to deploy with Railway. 2-3 ENVs and that's it.

Boring text below.

# Termcall

Termcall is a peer-to-peer terminal calling application for Linux and macOS. It uses a small signaling service to introduce callers, then carries text chat, audio, and ASCII video directly between peers over WebRTC.

Identity is based on a locally generated Ed25519 key—not an account or password. Each installation gets a canonical address such as `alice-abc234def567`, and callers can verify, trust, or block the full key fingerprint.

## Platform support

Termcall currently supports Linux and macOS. Windows is not supported.

## Build

Termcall requires Go 1.26 or newer. Audio and camera features also require GStreamer and the relevant platform plugins. Linux camera capture uses V4L2; macOS uses AVFoundation. On macOS, grant camera and microphone access to your terminal when prompted. Text-only calls have no native media dependency.

```sh
make build
```

The build detects the host operating system and architecture and creates `bin/termcall` and `bin/termcall-signald`. To build explicitly for another supported platform, use `--platform` (`macos` is accepted as an alias for `darwin`):

```sh
./scripts/build --platform darwin
./scripts/build --platform linux
```

Explicit platform builds include the target in their filenames so they do not overwrite native binaries, for example `bin/termcall-darwin-arm64`.

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
