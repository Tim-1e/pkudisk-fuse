# pkudisk-fuse

`pkudisk-fuse` browses, uploads, downloads and mounts PKU NetDisk (AnyShare) on Linux, macOS and Windows. Linux/macOS mounts use FUSE; Windows mounts use WinFsp. The CLI and TUI work without a filesystem driver. It is written in Go and uses the same HTTPS API and object-storage flow as the official AnyShare 7.0.6 client. See [platform support](docs/platform-support.md) for driver requirements, paths and validation.

The login uses **PKU unified authentication (IAAA)**. The password is RSA encrypted with the public key served by the login page. Saved accounts are stored in a private file below the user configuration directory (0600 on Unix; a restricted ACL on Windows); the password and renewable AnyShare session are used to resume a login without prompting. NetDisk local accounts are intentionally not supported.

## Install

Download a binary archive for your platform from [GitHub Releases](https://github.com/thezzisu/pkudisk-fuse/releases/latest), verify it against the accompanying `checksums.txt`, and extract the executable to a directory on your `PATH`. `amd64` is Intel/AMD x86-64; `arm64` includes Apple Silicon and Windows on ARM. Linux/macOS archives use `.tar.gz`; Windows archives use `.zip`. These binaries do not require Go.

With Go 1.26.9 or newer, install the command with:

```sh
go install github.com/thezzisu/pkudisk-fuse/cmd/pkudisk-fuse@latest
```

Use a version such as `@v0.3.0` instead of `@latest` to select a release. Add `GOBIN` (normally `~/go/bin`, or `%USERPROFILE%\go\bin` on Windows) to your `PATH` if needed.

```sh
pkudisk-fuse version
pkudisk-fuse account add
pkudisk-fuse ls
pkudisk-fuse tui
```

Only `mount` needs a filesystem driver: FUSE on Linux, macFUSE on macOS or WinFsp on Windows. See [platform support](docs/platform-support.md) for installation and validation details. Archives also contain shell completion scripts in `completions/`.

## Build

Go 1.26.9 or newer is required and must be available as `go` on `PATH`.

```sh
make build
make test
make check
```

The binary is `bin/pkudisk-fuse`. No part of the repository contains an account, password, token, cookie, or private key.

Without Make, build directly with `go build -trimpath -o pkudisk-fuse ./cmd/pkudisk-fuse`; on Windows use `-o pkudisk-fuse.exe`. Cross-builds for Linux/macOS/Windows amd64 and arm64 support `CGO_ENABLED=0`. Native Windows mounts require WinFsp to be installed separately.

For maintainers, [releasing](docs/releasing.md) describes the tagged CI workflow and local packaging rehearsal.

## Built-in help

The CLI uses Cobra. Every command includes its purpose, argument meanings, defaults, behavior, and examples in its help. Flags can appear before or after arguments. Use `--` before a file name beginning with a dash.

```sh
bin/pkudisk-fuse --help
bin/pkudisk-fuse help recover
bin/pkudisk-fuse help account add
bin/pkudisk-fuse download --help
```

Help and shell completion work without a saved account or network connection. Completion scripts are available for Bash, Zsh, Fish, and PowerShell:

```sh
source <(bin/pkudisk-fuse completion bash)
bin/pkudisk-fuse completion zsh > ~/.zsh/completions/_pkudisk-fuse
```

Exit codes are `0` for success, `1` for an execution failure, and `2` for invalid commands, arguments, or flags. Results and requested help go to stdout; account announcements, errors, and CLI transfer progress go to stderr. `--json` provides JSON arrays for `ls`, `account ls`, and `recover`, including `[]` for empty results.

## Authentication

An interactive mount prompts for the campus username and hides the password input. On Linux/macOS:

```sh
mkdir -m 700 ~/mnt/pkudisk
bin/pkudisk-fuse mount ~/mnt/pkudisk
```

On Windows, after installing WinFsp, use an unused drive letter: `pkudisk-fuse.exe mount P:`. `pkudisk-fuse account add` and `pkudisk-fuse tui` work on each platform without a mount driver.

For a non-interactive command, use an owner-only password file. The file must be a regular file owned by the current user with mode `0600`:

```sh
umask 077
printf '%s\n' 'YOUR-PASSWORD' > ~/.config/pkudisk-fuse/password
bin/pkudisk-fuse mount --username YOUR-CAMPUS-ID \
  --password-file ~/.config/pkudisk-fuse/password ~/mnt/pkudisk
```

`--password-stdin` is also available. A JSON credential file can be supplied with `--credentials-file`; it must be an owner-only `0600` file. Run `account add --help` for its format. The first successful login is saved under the campus username and becomes the default account. Use `--no-save` for a one-off login and `--non-interactive` to fail instead of prompting when credentials or required verification are missing.

Use `account` to add a login or manage saved accounts:

```sh
bin/pkudisk-fuse account ls
bin/pkudisk-fuse account add --username YOUR-CAMPUS-ID --password-file ~/.config/pkudisk-fuse/password work
bin/pkudisk-fuse account default work
bin/pkudisk-fuse account remove work
```

`account` without a subcommand shows help. Account labels and transfer destinations use positional arguments. Commands have no compatibility aliases.

`mount`, `ls`, `tui`, `upload`, and `download` reuse the default account. Pass `--account NAME` to choose another saved account. If the saved session has expired, the stored password is used to log in again and the renewed session is saved.

`mount`, `ls`, `upload`, and `download` print the selected account and server on standard error before restoring the session or logging in. JSON results remain on standard output. `account` and `tui` omit this announcement.

If IAAA requires a CAPTCHA or OTP/SMS verification, the driver prints a prompt. CAPTCHA images are written to a temporary file for the user to inspect and are removed after the answer is entered.

HTTP clients use Go's default TLS and HTTP protocol negotiation. Transport failures are returned immediately without application-level retries. Refresh-token rotation is serialized across processes using the same account store. Transport errors identify the server and endpoint without including query parameters or credentials.

## Choosing the mounted root

By default the mount root contains every accessible document library. Use `ls` without a path (or `ls /`) to inspect the libraries, and pass a library/folder path to `ls` for its contents:

```sh
bin/pkudisk-fuse ls --username YOUR-CAMPUS-ID --password-file ~/.config/pkudisk-fuse/password
bin/pkudisk-fuse ls --username YOUR-CAMPUS-ID --password-file ~/.config/pkudisk-fuse/password '/My library/Reports'
bin/pkudisk-fuse ls '/My library/Reports'
```

`tui` opens a Bubble Tea terminal browser showing the selected account and remote path. The remote list scrolls, paginates, and supports fuzzy filtering with `/`. Use `j`/`k` or the arrows to move, Enter to open a folder, Backspace to go up, `u` to pick a local file to upload, `d` to edit the selected file's download destination, `r` to refresh, and `?` for keyboard help. The upload picker also accepts `p` to type a local path. `~` is expanded in local path inputs.

```sh
bin/pkudisk-fuse tui
```

Directory reads and transfers run asynchronously so the screen remains responsive. The TUI adapts to terminal resizing and shows transfer progress and speed. Esc cancels an active transfer or dialog; `q` quits, cancelling an active operation first. Existing files require an explicit overwrite choice. Bubble Tea manages terminal input and restoration on exit.

Upload and download without mounting the filesystem. Remote paths are library-relative (`/Library/folder/file`) or an allowed `gns://...` ID:

```sh
bin/pkudisk-fuse upload ./report.pdf '/My library/Reports/report.pdf'
bin/pkudisk-fuse download --overwrite '/My library/Reports/report.pdf' ./report.pdf
```

Transfers handle one file at a time, not directory trees. A remote folder destination uses the local source name; a local directory destination uses the remote file name. Omitting a download destination saves that name in the current directory, including for a `gns://` selector.

Uploads refuse to replace an existing remote file unless `--overwrite` is supplied. Downloads use an atomic temporary file and refuse to replace an existing local file unless `--overwrite` is supplied. Interactive terminals show a maintained progress bar with percentage, transferred/total bytes, speed, and ETA. The bar reaches 100% after an upload is published or a download is saved, and marks failed transfers. Progress is disabled when redirected; `--io-timeout` controls each file operation. Ctrl-C cancels CLI transfers; cancelled downloads preserve an existing destination and remove the temporary file.

Uploads negotiate the server's minimum and maximum part sizes and maximum part count, then grow the part size as needed. Large files use the desktop client's multipart protocol to avoid the single-request upload limit. Small and empty files use a single request. Downloads stream 512 MiB ranges with fresh signed URLs and verify the selected revision and exact size. Server quotas, administrator policies, and storage-service limits still apply; increase `--io-timeout` for long transfers. See [the size-limit investigation](docs/transfer-limits.md) for the observed limits and verification.

The `--root` option accepts either the displayed path or an AnyShare `gns://...` object ID. A folder can therefore be mounted directly:

```sh
bin/pkudisk-fuse mount \
  --root '/My library/Reports' \
  --username YOUR-CAMPUS-ID \
  --password-file ~/.config/pkudisk-fuse/password \
  ~/mnt/pkudisk
```

The selected root is checked against the libraries returned for the authenticated account. A `gns://` ID from another library is rejected.

## Long-lived mounts

The driver renews idle sessions every hour and retains rotated refresh cookies in memory. `--refresh-interval` changes this interval; `0` disables scheduled renewal. If an API call returns HTTP 401, the driver also renews the session and retries that request once. Concurrent requests share one serialized refresh.

Temporary network failures retry after a minute. When the server rejects the refresh token, the driver unmounts and exits with an error. A supervisor can then restart it using a private credential file. Accounts requiring CAPTCHA or second-factor verification may need interactive login again.

A user service with `Restart=on-failure` can keep a mount alive across the current user session; it does not put credentials in this repository. The service used for the local read-only mount is outside the repository under `~/.config/systemd/user/`.

The optional live test uses a separate session and reads remote data only:

```sh
PKUDISK_TEST_CREDENTIALS="$HOME/.config/pkudisk-fuse/credentials.json" \
  go test -run '^TestLiveSessionRefresh$' -v ./internal/auth
```

## File behavior

Mount reads fetch files up to 16 MiB whole on first access, if the file fits in the configured disk budget. A 5 MiB photo uses one authorization call and one data GET, including when a viewer starts at its tail. Larger files start with 4 MiB ranges. Consecutive reads double the fetch size up to 16 MiB; seeking resets it to the base range. A read spanning adjacent blocks is batched into one range when the batch fits. Already cached or in-flight neighbours stop the batch, avoiding duplicate transfers. Read-ahead runs as part of the requested fetch; larger batches take longer to complete but reduce subsequent round trips.

Fetches are limited to four at a time per mount. Overlapping requests share one fetch, and queued reads allocate no fetch buffer before admission. Metadata queries and warm reads proceed independently of network I/O and its queue. Repeated reads and reopening the same revision reuse the private disk cache. Memory-mapped reads are supported on Linux, macOS and Windows.

Tune the policy with the same mount flags on all platforms:

| Flag | Default | Effect |
| --- | --- | --- |
| `--read-whole-file` | `16MiB` | Fetch files up to this size whole; `0` disables this threshold. |
| `--read-block-size` | `4MiB` | Base range for large files and random seeks. |
| `--read-ahead` | `16MiB` | Maximum fetch size for sequential reads and contiguous demand; `0` disables growth and batching beyond the base range. |
| `--read-concurrency` | `4` | Maximum simultaneous remote fetches; valid range is 1–64. |

Size flags accept bytes, decimal units such as `5MB`, and binary units such as `8MiB` or `2GiB`. Each read size is capped at 512 MiB; a read-ahead value below the base range leaves the base range unchanged. The disk budget can further reduce ranges and batches. Fetch buffers use memory as well as disk: increasing the threshold, batch size or concurrency increases peak memory use. The OS page cache uses additional memory.

For fewer round trips on a fast connection, or smaller fetches on a slower connection:

```sh
pkudisk-fuse mount ~/PKUNetDisk --read-whole-file 32MiB --read-ahead 64MiB
pkudisk-fuse mount ~/PKUNetDisk --read-whole-file 0 --read-block-size 1MiB --read-ahead 0 --read-concurrency 2
```

On Windows use an unused drive letter, such as `P:`, as the mountpoint. `mount --help` explains the policy and includes these examples without requiring an account or a network connection.

Reads and writes share one per-mount `--cache-limit` budget. Clean data is evicted in order of use to make room; pending writes and recovery journals are never evicted. A complete file held in one cached range, including a whole-file fetch or batch, or a previously flushed write file, can become writable without another download or allocation. Flushed write contents remain available for subsequent reads until eviction or unmount. Reads still work when the budget is occupied by pending writes, but may not retain new ranges. Clean cache data is removed on unmount.

New files, overwrites, random writes, truncation, append, mkdir, unlink, rmdir, rename, and same-library moves are supported. Existing-file writes are compared with the remote revision before upload; a concurrent edit returns `EBUSY` and leaves the local write cache for recovery.

Writes are held in a private `0700` cache and published on `fsync`/close. A failed upload is never acknowledged as successful. The journal and cached data remain available under `--cache-dir`; inspect them with:

```sh
bin/pkudisk-fuse recover --cache-dir ~/.cache/pkudisk-fuse
```

`recover` lists retained local contents and their recorded remote names/IDs, sizes, revisions, and update times. It runs offline and does not upload, delete, or automatically restore anything. Stop the affected mount, copy needed cached files to a permanent location, then use `upload` with a chosen destination. `recover --help` includes a complete example; `recover --json` returns `[]` when no records exist.

Set `--read-only` when a mount must not issue any mutating API request. The default shared disk-cache limit is 10 GiB; `--cache-limit 0` disables it. This limit does not cap the size of files that can be read. `--part-size` sets a preferred upload chunk size (at least 5 MiB), which is adjusted to the server's limits and the file size.

Ctrl-C unmounts on all platforms. Linux also supports `fusermount3 -u MOUNTPOINT` (or `fusermount -u MOUNTPOINT`); macOS supports `umount MOUNTPOINT`.

## License

Licensed under [Apache-2.0](LICENSE). The license is the verbatim [official Apache text](https://www.apache.org/licenses/LICENSE-2.0.txt); project attribution is in [NOTICE](NOTICE). Binary archives include third-party licenses in `licenses/`.

## Implementation notes

The installed desktop client was inspected from `/opt/pku-netdisk/current/resources/app.asar`. The relevant API calls are documented in [docs/anyshare-protocol.md](docs/anyshare-protocol.md). Cobra handles command parsing and help; Bubble Tea, Bubbles, and Lip Gloss provide the terminal UI; progressbar renders CLI transfer progress. Both interfaces use the same transfer service for destination selection, streaming, revision checks, and atomic downloads. Authentication, remote object-storage URLs, path selectors, and recovery metadata are validated before use; signed object-storage URLs are never included in errors or logs.
