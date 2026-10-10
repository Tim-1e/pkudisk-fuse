# pkudisk-fuse

`pkudisk-fuse` mounts the files that the current user can access in PKU NetDisk (AnyShare) as a Linux FUSE filesystem and can upload or download files without a mount. It is written in Go and uses the same HTTPS API and object-storage flow as the installed AnyShare 7.0.6 client.

The login uses **PKU unified authentication (IAAA)**. The password is RSA encrypted with the public key served by the login page. Saved accounts are stored in an owner-only `0600` file below the user configuration directory; the password and renewable AnyShare session are used to resume a login without prompting. NetDisk local accounts are intentionally not supported.

## Build

Go 1.26 or newer is required and must be available as `go` on `PATH`.

```sh
make build
make test
make check
```

The binary is `bin/pkudisk-fuse`. No part of the repository contains an account, password, token, cookie, or private key.

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

Exit codes are `0` for success, `1` for an execution failure, and `2` for invalid commands, arguments, or flags. Results and requested help go to stdout; account announcements, errors, and CLI transfer progress go to stderr. `--json` provides JSON arrays for `root`, `ls`, `account list`, and `recover`, including `[]` for empty results.

## Authentication

The simplest interactive mount prompts for the campus username and hides the password input:

```sh
mkdir -m 700 ~/mnt/pkudisk
bin/pkudisk-fuse mount ~/mnt/pkudisk
```

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
bin/pkudisk-fuse account
bin/pkudisk-fuse account add --username YOUR-CAMPUS-ID --password-file ~/.config/pkudisk-fuse/password work
bin/pkudisk-fuse account default work
bin/pkudisk-fuse account remove work
```

`mount`, `root`, `ls`, `tui`, `upload`, and `download` reuse the default account. Pass `--account NAME` to choose another saved account. If the saved session has expired, the stored password is used to log in again and the renewed session is saved.

`mount`, `root`, `ls`, `upload`, and `download` print the selected account and server on standard error before restoring the session or logging in. JSON results remain on standard output. `account` and `tui` omit this announcement.

If IAAA requires a CAPTCHA or OTP/SMS verification, the driver prints a prompt. CAPTCHA images are written to a temporary file for the user to inspect and are removed after the answer is entered.

HTTP clients use Go's default TLS and HTTP protocol negotiation. Transport failures are returned immediately without application-level retries. Refresh-token rotation is serialized across processes using the same account store. Transport errors identify the server and endpoint without including query parameters or credentials.

## Choosing the mounted root

By default the mount root contains every accessible document library. Use `root` or `ls` without a path to inspect the libraries, and pass a library/folder path to `ls` for its contents:

```sh
bin/pkudisk-fuse root --username YOUR-CAMPUS-ID --password-file ~/.config/pkudisk-fuse/password
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

Uploads follow the desktop client's negotiated sync protocol for files that need multipart transfer. The client asks the server for minimum and maximum part sizes and the maximum part count, then grows the part size as needed so large files avoid the ordinary single-request upload limit. Downloads use bounded ranged requests so a portal response-size limit does not cap a large file. Server quotas, administrator policies, and storage-service limits still apply.

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

Reads use byte ranges and do not download a whole large file. New files, overwrites, random writes, truncation, append, mkdir, unlink, rmdir, rename, and same-library moves are supported. Existing-file writes are compared with the remote revision before upload; a concurrent edit returns `EBUSY` and leaves the local write cache for recovery.

Writes are held in a private `0700` cache and published on `fsync`/close. A failed upload is never acknowledged as successful. The journal and cached data remain available under `--cache-dir`; inspect them with:

```sh
bin/pkudisk-fuse recover --cache-dir ~/.cache/pkudisk-fuse
```

`recover` lists retained local contents and their recorded remote names/IDs, sizes, revisions, and update times. It runs offline and does not upload, delete, or automatically restore anything. Stop the affected mount, copy needed cached files to a permanent location, then use `upload` with a chosen destination. `recover --help` includes a complete example; `recover --json` returns `[]` when no records exist.

Set `--read-only` when a mount must not issue any mutating API request. The default cache limit is 10 GiB and can be changed with `--cache-limit`; `--part-size` controls multipart upload chunks (minimum 5 MiB).

Unmount with `fusermount3 -u MOUNTPOINT` (or `fusermount -u MOUNTPOINT`). The process also handles Ctrl-C and SIGTERM.

## Implementation notes

The installed desktop client was inspected from `/opt/pku-netdisk/current/resources/app.asar`. The relevant API calls are documented in [docs/anyshare-protocol.md](docs/anyshare-protocol.md). Cobra handles command parsing and help; Bubble Tea, Bubbles, and Lip Gloss provide the terminal UI; progressbar renders CLI transfer progress. Both interfaces use the same transfer service for destination selection, streaming, revision checks, and atomic downloads. Authentication, remote object-storage URLs, path selectors, and recovery metadata are validated before use; signed object-storage URLs are never included in errors or logs.
