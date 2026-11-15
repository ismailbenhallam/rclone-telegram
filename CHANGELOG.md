# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Versions follow the rclone release each build is made from (see
[Versions](README.md#versions)). Releases that only update rclone are not
listed here; see rclone's [changelog](https://rclone.org/changelog/).

## [Unreleased]

## [1.76.0] - 2026-11-15

First release, built on rclone v1.76.0.

### Added

- `telegram` backend: stores files in private channels of a Telegram user
  account, over MTProto. Files up to 2 GB per part (4 GB with Premium);
  larger files are split into parts automatically.
- Buckets: the first path segment is a bucket, and each bucket is its own
  private channel. A `rclone` channel lists the buckets, so every machine
  logged in to the account sees the same buckets.
- `purge` of a bucket deletes its channel at once. Renaming a bucket
  (`moveto tg:old tg:new`) changes no data.
- Server-side move, rename and copy, within a bucket and between buckets.
  Nothing is re-uploaded.
- MD5 hashes, nanosecond modification times, empty directories, and byte
  range reads, so `check`, `mount` and `serve s3` work.
- Crash-safe append-only log per bucket with periodic index snapshots.
  Several processes can use the same account at once.
- `cleanup` deletes parts left by failed or replaced uploads. The
  `backend cleanup` and `backend rebuild` commands are also available.
- Interactive login with phone number, login code and two-step
  verification password. `config reconnect` switches accounts.
- `rclone-telegram`: the official rclone with this backend compiled in.
  Prebuilt binaries for Linux, macOS, Windows and FreeBSD.
- `rclone selfupdate` installs the latest `rclone-telegram` release from
  GitHub instead of official rclone. It verifies the download against the
  release's `SHA256SUMS.txt` and installs nothing if the check fails.
  Supports `--check`, `--version` and `--output`.
- Release archives carry a signed build provenance attestation. Verify one
  with `gh attestation verify <file> -R ismailbenhallam/rclone-telegram`.

[Unreleased]: https://github.com/ismailbenhallam/rclone-telegram/compare/v1.76.0...HEAD
[1.76.0]: https://github.com/ismailbenhallam/rclone-telegram/releases/tag/v1.76.0
