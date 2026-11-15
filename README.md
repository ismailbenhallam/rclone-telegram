# rclone-telegram

[![CI](https://github.com/ismailbenhallam/rclone-telegram/actions/workflows/ci.yml/badge.svg)](https://github.com/ismailbenhallam/rclone-telegram/actions/workflows/ci.yml)

A [rclone](https://rclone.org) **backend** that stores files in private
channels of your Telegram account, one channel per bucket. Every rclone
feature works on top of it: `copy`, `sync`, `check`, `mount`,
`serve webdav`, `serve s3` and more.

It talks [MTProto](https://core.telegram.org/mtproto) as a **user** account
through [gotd/td](https://github.com/gotd/td). Each message holds up to
2 GB (4 GB with Premium), not the Bot API's 50 MB. Larger files are split
into parts automatically.

Releases follow rclone's version numbers: `rclone-telegram` v1.76.0 is
rclone v1.76.0 with the `telegram` backend added. See [Versions](#versions).

> [!WARNING]
> This is an experimental project. It is **not** a storage, backup or
> archival solution, and it is not presented or encouraged as one. You are
> solely responsible for how you use it. Before using it, read and comply
> with the [Telegram Terms of Service](https://telegram.org/tos) and the
> [Telegram API Terms of Service](https://core.telegram.org/api/terms).
> See the [Disclaimer](#disclaimer).

## How it plugs into rclone

Official rclone binaries cannot load third-party backends at runtime.
Every storage type is a Go package compiled into the binary. These are the
entries you see in `rclone config`, such as `s3`, `drive` and `dropbox`.

This repository ships **`rclone-telegram`**: the official rclone (as a Go
library dependency, not a fork) with the `telegram` backend added. It is a
drop-in rclone: same commands, same flags, same config file. You can keep
the official `rclone` installed alongside it. The only difference is that
official rclone fails on `telegram` remotes with
`didn't find backend called "telegram"`.

## Install

### Prebuilt binaries

Download the archive for your platform from
[Releases](https://github.com/ismailbenhallam/rclone-telegram/releases),
extract it and put `rclone-telegram` on your `PATH`.

| Platform | `mount` support |
| --- | --- |
| Linux amd64 / arm64 / armv7 | yes, needs `fuse3` (`apt install fuse3`) |
| macOS arm64 / amd64 | yes, needs [macFUSE](https://macfuse.github.io) or [FUSE-T](https://www.fuse-t.org) |
| Windows amd64 / arm64 | yes, needs [WinFsp](https://winfsp.dev) |
| FreeBSD amd64 | no |

macOS blocks unsigned downloads. After extracting, run:

```sh
xattr -d com.apple.quarantine ./rclone-telegram
```

To check a download before extracting it: `SHA256SUMS.txt` catches a
corrupt or incomplete file, and the attestation proves the archive was
built by this repository's release workflow (needs the
[GitHub CLI](https://cli.github.com)):

```sh
sha256sum -c --ignore-missing SHA256SUMS.txt   # macOS: shasum -a 256 -c
gh attestation verify rclone-telegram-*.tar.gz -R ismailbenhallam/rclone-telegram
```

### With Go (1.26+)

```sh
go install github.com/ismailbenhallam/rclone-telegram@latest
```

On macOS this build has no `mount`. Use a release binary, or build from
source with macFUSE installed.

### From source

```sh
git clone https://github.com/ismailbenhallam/rclone-telegram
cd rclone-telegram
make build        # produces ./rclone-telegram
```

On macOS, `make build` enables `mount` automatically when macFUSE or
FUSE-T headers are installed.

Check that it works:

```sh
rclone-telegram version            # rclone v1.76.0-telegram-v1.76.0
rclone-telegram help backend telegram
```

## Use it as your main `rclone`

`rclone-telegram` is a complete rclone, so it can replace the official
binary. Every command, flag and remote keeps working, and you gain the
`telegram` backend. It reads the same `rclone.conf`, so your existing
remotes need no changes.

### Linux and macOS

1. **Install `rclone-telegram`** using one of the methods above.

2. **Find the current `rclone`:**

   ```sh
   which -a rclone
   ```

3. **Remove it**, using the same tool that installed it:

   | Installed with | Remove with |
   | --- | --- |
   | apt (Debian/Ubuntu) | `sudo apt remove rclone` |
   | dnf (Fedora) | `sudo dnf remove rclone` |
   | snap | `sudo snap remove rclone` |
   | Homebrew | `brew uninstall rclone` |
   | rclone.org `install.sh` | `sudo rm /usr/bin/rclone /usr/local/bin/rclone` |

   Run `which -a rclone` again. It should print nothing.

4. **Point `rclone` at this build** with a symlink in `~/.local/bin`:

   ```sh
   mkdir -p ~/.local/bin
   # If you installed with `go install`:
   ln -sf "$(go env GOPATH)/bin/rclone-telegram" ~/.local/bin/rclone
   # If you downloaded a release, move the binary first:
   #   mv ./rclone-telegram ~/.local/bin/rclone-telegram
   #   ln -sf ~/.local/bin/rclone-telegram ~/.local/bin/rclone
   ```

5. **Make sure `~/.local/bin` is on your `PATH`.** Add this line to
   `~/.zshrc` or `~/.bashrc` if it is missing, then open a new terminal:

   ```sh
   export PATH="$HOME/.local/bin:$PATH"
   ```

6. **Check the result:**

   ```sh
   hash -r                      # forget the old rclone location
   which rclone                 # ~/.local/bin/rclone
   rclone version               # rclone v1.xx.x-telegram-vX.Y.Z
   rclone listremotes           # your existing remotes are still there
   rclone help backend telegram
   ```

7. **Optional: `rclone mount`.** Install FUSE: `sudo apt install fuse3`
   on Debian/Ubuntu, or macFUSE / FUSE-T on macOS.

### Windows

1. Download the Windows zip from Releases and extract it.
2. Rename `rclone-telegram.exe` to `rclone.exe`.
3. Replace your existing `rclone.exe` with it. Run `where rclone` to find
   the old one.
4. Check it with `rclone version`. The output should contain `-telegram-`.

### Updating

`rclone-telegram` contains a whole copy of rclone, so rclone's fixes and
features only reach you through a new `rclone-telegram` release. A
scheduled GitHub workflow checks rclone's releases every day. For each new
one, oldest first, it updates the rclone dependency on `main`, runs the
tests, and publishes a matching release. To update:

- **Installed with `go install`:** run
  `go install github.com/ismailbenhallam/rclone-telegram@latest`. The
  symlink keeps working.
- **Downloaded a release:** run `rclone selfupdate`. It installs the
  latest `rclone-telegram` release for your platform from this
  repository's GitHub releases, never the official rclone. It follows the
  `rclone` symlink and replaces the real binary. The download is checked
  against the release's `SHA256SUMS.txt`, and if that check fails nothing
  is installed.
  - `rclone selfupdate --check` only reports whether an update exists.
  - `rclone selfupdate --version v1.76.1` installs a given release, older
    ones included.
  - `rclone selfupdate --output ./rclone-telegram.new` writes the new
    binary elsewhere instead of replacing the running one.
  - A binary in a system directory such as `/usr/local/bin` needs `sudo`.
  - You can also download the new release by hand and replace
    `~/.local/bin/rclone-telegram` (or `rclone.exe` on Windows).
- **Built from source:** run `git pull && make build`, or `go install .`
  from the clone.

The rclone version is pinned in `go.mod`, so a source build always uses the
version that was tested with this backend. To try a newer rclone before
the workflow picks it up:

```sh
go get github.com/rclone/rclone@latest && go mod tidy && make test build
```

### Versions

Every stable rclone release gets an `rclone-telegram` release with the
same version number, with one exception: a fix to the `telegram` backend
between two rclone releases takes the next patch number, and the next
rclone patch release then skips past it:

| Change | rclone | rclone-telegram |
| --- | --- | --- |
| rclone release | `v1.76.0` | `v1.76.0` |
| Backend fix | `v1.76.0` | `v1.76.1` |
| rclone release | `v1.76.1` | `v1.76.2` |
| rclone release | `v1.76.2` | `v1.76.3` |

`MAJOR.MINOR` always matches rclone's. `rclone version` always shows the
exact rclone version first, for example `rclone v1.76.1-telegram-v1.76.2`.
Patch releases to older rclone lines (for example rclone `v1.75.3` after
`v1.76.0`) are not built.

### Going back to official rclone

```sh
rm ~/.local/bin/rclone
sudo apt install rclone    # or: brew install rclone, or the rclone.org installer
```

Remotes of type `telegram` then stop working, with
`didn't find backend called "telegram"`. All other remotes are unaffected.

## Setup

1. Log in at <https://my.telegram.org/apps> and create an application (any
   name). Copy the **api_id** and **api_hash**.
2. Run `rclone-telegram config`:
   - `n` for a new remote, and name it e.g. `tg`.
   - Storage: `telegram`.
   - Enter `api_id` and `api_hash`.
   - Advanced config: `n`.
   - Phone number in international format, e.g. `+33612345678`.
   - The login code Telegram sends **inside the Telegram app**.
   - Your two-step verification password, if you set one.
3. On first use a private channel named `rclone` is created in the account.
   It lists your buckets. Each bucket gets its own private channel, named
   `rclone/<bucket>`. Do not post in these channels manually.

The login must run in an interactive terminal. To use the remote on a
server, configure it on a machine where you can type, then copy the remote
section of `rclone.conf` across (`rclone-telegram config file` shows where
the file is).

### Switching to another Telegram account

The channels belong to the account that created them. To use another
account, choose one of these:

- **Keep both:** create a second remote with `rclone-telegram config`. Each
  remote keeps its own account and channels.
- **Replace the account:** run `rclone-telegram config reconnect tg:`,
  answer `y` to log in again, and log in with the other account. The new
  account cannot see the old channels, so its settings are cleared and a
  new `rclone` channel is created on first use. Files in the old account
  stay there.

To rename a remote, use `rclone-telegram config` and choose `r) Rename
remote`. There is no `config rename` command.

## Usage

The first path segment is the bucket, as with S3: `tg:photos/2024/a.jpg`
is `2024/a.jpg` in bucket `photos`.

```sh
rclone-telegram mkdir tg:photos                       # create a bucket
rclone-telegram lsd tg:                               # list buckets
rclone-telegram copy ./photos tg:photos -P            # creates the bucket if needed
rclone-telegram check ./photos tg:photos              # verify MD5s
rclone-telegram mount tg: ~/tg --vfs-cache-mode writes
rclone-telegram serve s3 tg: --auth-key ACCESS,SECRET  # S3 API on :8080
rclone-telegram serve webdav tg:
```

Any rclone command works. See the [rclone docs](https://rclone.org/docs/).

### Buckets

| Action | Command | Cost |
| --- | --- | --- |
| Create | `mkdir tg:b`, or upload into `tg:b/...` | Creates one channel |
| Delete everything in it | `purge tg:b` | Deletes the channel at once |
| Delete if empty | `rmdir tg:b` | Deletes the channel |
| Rename | `moveto tg:old tg:new` | One message, nothing is copied |
| Copy or move files between buckets | `copy tg:a/x tg:b/` | Server-side: messages are forwarded, nothing is re-uploaded |

Files cannot be stored at the top level, outside a bucket. A bucket name
can be up to 100 characters.

## Options

| Option | Default | Description |
| --- | --- | --- |
| `api_id`, `api_hash` | required | From my.telegram.org |
| `channel_title` | `rclone` | Title of the channel that lists the buckets. Bucket channels are named `<channel_title>/<bucket>` |
| `chunk_size` | `2000Mi` | Split size for large files (up to `4000Mi` with Premium) |
| `upload_concurrency` | `4` | Parallel part uploads per file |
| `flush_interval` | `10s` | How long changes are batched before a new index snapshot |
| `refresh_interval` | `1m` | How often to pick up changes made by other processes (`0` disables) |
| `encoding` | standard | Characters to escape in names ([rclone encoding](https://rclone.org/overview/#encoding)) |

Each can also be set as a flag (`--telegram-chunk-size 1Gi`) or an
environment variable (`RCLONE_TELEGRAM_CHUNK_SIZE`).

## Security

The config file holds the Telegram **session**, which grants full access to
the account: messages, contacts, everything. Treat it like a password:

- Encrypt the config: `rclone-telegram config encryption set`.
- Never commit or share `rclone.conf`.
- To revoke access, open Telegram, go to Settings, then Devices, and
  terminate the session.

## How data is stored

Every channel works as an append-only log. Replaying it in order rebuilds
the exact state.

- **Bucket list.** The `rclone` channel holds one small text message for
  each bucket created, renamed or deleted, with the ID of the bucket's
  channel. If two processes create the same bucket at the same moment, the
  first message wins and the other channel is deleted.

Each bucket channel holds:

- **Data messages.** Each file is one document message, or several of up to
  `chunk_size`. The caption of each part holds JSON metadata: path, part
  number, modtime, and size and MD5 on the final part. Empty files are text
  messages.
- **Operation messages.** Small text messages record every change: delete,
  move, directory move, mkdir, rmdir and modtime. Each change is written to
  the channel before it is applied, so no change is lost if the process
  stops.
- **Index snapshots.** A gzipped JSON snapshot of the state is posted as a
  document captioned `#rclone_index`, so startup does not replay the whole
  log. On startup the newest snapshot loads and only newer messages are
  replayed.
- **Several processes.** Each process replays the others' messages, every
  `refresh_interval` and before saving a snapshot, so changes merge
  instead of overwriting each other.
- **Server-side operations.** Inside a bucket, move and rename write one
  operation message. Copy, and moves between buckets, forward the
  messages. No data is re-uploaded.
- **Reads.** Byte-range reads use aligned 1 MiB blocks, so `mount` and S3
  range requests work.

## Maintenance

```sh
rclone-telegram cleanup tg:b           # delete parts left by failed or replaced uploads in bucket b
rclone-telegram cleanup tg:            # the same for every bucket
rclone-telegram backend rebuild tg:b   # rebuild bucket b's index by replaying its channel
rclone-telegram backend rebuild tg:    # the same for every bucket
```

`cleanup` keeps unfinished uploads younger than 24h, so uploads in progress
on other machines are not affected. `backend cleanup` does the same and
prints a count per bucket.

## Troubleshooting

| Problem | Fix |
| --- | --- |
| `not logged in` or `session expired or revoked` | `rclone-telegram config reconnect tg:` |
| `this account cannot access storage channel` | You logged in with another account. See [Switching to another Telegram account](#switching-to-another-telegram-account). |
| Uploads pause for a while | Telegram rate limit (`FLOOD_WAIT`); rclone waits automatically. Lower `--transfers` and `--telegram-upload-concurrency`. |
| Listing looks wrong | `rclone-telegram backend rebuild tg:` |
| `path too long to store` | Shorten the path: it must fit in a 1024-character caption |
| `PHONE_CODE_EXPIRED` / `PHONE_CODE_INVALID` | Run `config` again and enter the newest code quickly |
| `unknown command "mount"` | Your build has no mount support. See the table under [Install](#install). |
| Need details | Add `-vv` to any command |

## Limitations

- Each bucket is a channel. Telegram limits an account to about 500
  channels (1000 with Premium), and creating many channels quickly
  triggers long rate limits. Prefer a few buckets with folders inside
  over many buckets.
- Several processes can write to one bucket, but each sees the others'
  changes only after `refresh_interval`. Two processes writing the same
  path at the same moment: the later upload wins.
- Paths must fit in a caption: about 900 characters.
- Downloads fetch one block at a time per file.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## AI disclosure

This project was developed with the help of AI. Much of the code, tests
and documentation was generated with [Claude Code](https://claude.com/claude-code),
Anthropic's AI coding assistant, under the maintainer's direction. Commits
written with AI help carry a `Co-Authored-By: Claude` trailer.

## Disclaimer

- This project has no affiliation with, endorsement from or connection to
  Telegram.
- It is an experimental, educational integration between rclone and the
  Telegram API. It is not intended, designed or encouraged for use as a
  primary storage, backup or archival service.
- You alone are responsible for your use of this software, and for
  complying with the [Telegram Terms of Service](https://telegram.org/tos),
  the [Telegram API Terms of Service](https://core.telegram.org/api/terms)
  and any other applicable rules and laws. Telegram may limit, restrict or
  terminate accounts at its discretion.
- Do not rely on it for data you cannot afford to lose.
- The software is provided "as is", without warranty of any kind. The
  authors and contributors accept no liability for data loss, account
  restrictions or any other consequences of using it (see [LICENSE](LICENSE)).

## License

[MIT](LICENSE)
