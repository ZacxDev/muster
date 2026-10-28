# Running muster from source

This is the developer setup: a throwaway database, the server from `go run`, and
one of your own machines mirrored over plain HTTP. To install muster for real,
follow [INSTALL.md](https://github.com/ZacxDev/muster/blob/main/INSTALL.md).

You need Go (the version in `go.mod`), Docker and, to mirror a session, tmux.
The [Nix flake](../flake.nix) provides the toolchain (`nix develop`).

```sh
git clone https://github.com/ZacxDev/muster && cd muster
make test-db                       # a throwaway Postgres on 127.0.0.1:55432
docker compose -f docker-compose.test.yml exec postgres createdb -U muster muster_dev

export DATABASE_URL='postgres://muster:muster@127.0.0.1:55432/muster_dev?sslmode=disable'
export MUSTER_UI_PASSWORD='pick-a-password-of-12+-chars'
export MUSTER_STANDALONE=1         # no external permission router
export MUSTER_MIRROR=1             # turn mirrored sessions on
go run ./cmd/muster-server         # migrates, then listens on :8105 (all interfaces)
```

Open <http://127.0.0.1:8105> and sign in with the password. Then mirror your
tmux sessions. Run this in a second shell with the same `DATABASE_URL`:

```sh
go run ./cmd/muster-server mirror token create mybox > ~/.muster-mirror-token
go run ./cmd/muster-mirror -url http://127.0.0.1:8105 -allow-http \
  -host mybox -token-file ~/.muster-mirror-token -interactive
```

Your tmux windows now appear under **Agents**. Drop `-interactive` to make them
read-only. `-allow-http` is for local development only; a real deployment uses
https. When you are done, `make test-db-down` removes the database.

⚠ This setup listens on every interface and sets no `MUSTER_HOOK_TOKEN`, so
the machine API is open to anyone who can reach the port. Keep it on a trusted
machine, or set the token ([auth.md](auth.md#machine-credentials)).

The server prints one line per feature at boot, saying what is on, what is off,
and which variable changes it. Every variable is listed in
[configuration.md](configuration.md).

Next: [CONTRIBUTING.md](../CONTRIBUTING.md) for the test suite and the gates,
and [testing.md](testing.md) for the browser suite and `make screenshots`.
