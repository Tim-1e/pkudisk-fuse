# pkudisk-fuse

`pkudisk-fuse` mounts the files that the current user can access in PKU NetDisk (AnyShare) as a Linux FUSE filesystem. It is written in Go and uses the same HTTPS API and object-storage upload/download flow as the installed AnyShare 7.0.6 client.

The mount login uses **PKU unified authentication (IAAA)**. The password is RSA encrypted with the public key served by the login page, and is discarded after login. The session and refresh token remain in memory for the lifetime of the mount. NetDisk local accounts are intentionally not supported.

## Build

Go 1.24 or newer is required and must be available as `go` on `PATH`.

```sh
make build
make test
make check
```

The binary is `bin/pkudisk-fuse`. No part of the repository contains an account, password, token, cookie, or private key.

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

`--password-stdin` is also available. A JSON credential file can be supplied with `--credentials-file`; it must be an owner-only `0600` file.

If IAAA requires a CAPTCHA or OTP/SMS verification, the driver prints a prompt. CAPTCHA images are written to a temporary file for the user to inspect and are removed after the answer is entered.

## Choosing the mounted root

By default the mount root contains every accessible document library. Use `roots` to inspect the libraries and `ls` to inspect a folder:

```sh
bin/pkudisk-fuse roots --username YOUR-CAMPUS-ID --password-file ~/.config/pkudisk-fuse/password
bin/pkudisk-fuse ls --username YOUR-CAMPUS-ID --password-file ~/.config/pkudisk-fuse/password '/My library/Reports'
```

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

Set `--read-only` when a mount must not issue any mutating API request. The default cache limit is 10 GiB and can be changed with `--cache-limit`; `--part-size` controls multipart upload chunks (minimum 5 MiB).

Unmount with `fusermount3 -u MOUNTPOINT` (or `fusermount -u MOUNTPOINT`). The process also handles Ctrl-C and SIGTERM.

## Implementation notes

The installed desktop client was inspected from `/opt/pku-netdisk/current/resources/app.asar`. The relevant API calls are documented in [docs/anyshare-protocol.md](docs/anyshare-protocol.md). Authentication, remote object-storage URLs, path selectors, and recovery metadata are validated before use; signed object-storage URLs are never included in errors or logs.
