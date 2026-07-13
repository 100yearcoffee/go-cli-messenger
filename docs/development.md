# Development

Build and verify with `make build`, `go test ./...`, `go test -race ./...`, and `go vet ./...`.

Start a local open server with `./bin/termcall-signald`. Initialize separate configuration homes, then use the address printed by `termcall identity`:

```sh
XDG_CONFIG_HOME=/tmp/alice/config ./bin/termcall init alice --server ws://127.0.0.1:8080/v1/ws
XDG_CONFIG_HOME=/tmp/bob/config ./bin/termcall init bob --server ws://127.0.0.1:8080/v1/ws
XDG_CONFIG_HOME=/tmp/bob/config ./bin/termcall listen
XDG_CONFIG_HOME=/tmp/alice/config ./bin/termcall chat <bob-canonical-address>
```

For automation set `TERMCALL_ACCESS_KEY`, or `TERMCALL_ACCESS_KEY_FILE` during client initialization. Protocol v2 is intentionally incompatible with v1 messages and legacy identity, session, and trust files; there is no migration.
