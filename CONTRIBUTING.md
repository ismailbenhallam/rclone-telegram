# Contributing

Issues and pull requests are welcome.

## Layout

- `backend/telegram/` is the backend. It is written exactly like an in-tree
  rclone backend, so it can be proposed upstream later without changes.
- `main.go` is the standalone binary: all of rclone plus this backend.
- `plugin/` is the optional Go plugin build (experimental).

## Before sending a PR

```sh
make vet test build
```

- `store.go` holds the only interfaces to Telegram: `account` (the login,
  creating and deleting channels) and `store` (one channel). They are
  implemented in `telegram_store.go`. Keep everything else talking to
  these, so tests can use the in-memory fake in `fake_test.go`.
- Keep `index.go` free of I/O.
- Never commit an rclone config, a session string, or an API ID/hash.
- Test responsibly and within the [Telegram API Terms of Service](https://core.telegram.org/api/terms).

## Tests

`go test ./...` runs rclone's generic backend suite (`fstests`) and the
backend's own tests against an in-memory Telegram account. No Telegram account is
needed, and CI runs them on every push.

To run the suite against a real account, configure a remote and run:

```sh
go test ./backend/telegram -remote TestTelegram: -run TestIntegration -v
```

It creates and deletes a few bucket channels named `rclone/rclone-test-…`.
Use an account you can spare. The other tests only run on the in-memory
account.

## Releasing

Versions follow rclone's (see the README). To release a backend fix:

1. Move the `[Unreleased]` entries of `CHANGELOG.md` under a heading for
   the next version (`./scripts/next-version.sh` prints it) and commit.
2. Run `make release` on a clean, up-to-date `main`. It tags that version
   and the release workflow publishes the binaries.

New rclone releases are picked up automatically by the daily
`Track rclone releases` workflow.
