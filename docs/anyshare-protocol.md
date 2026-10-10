# AnyShare protocol notes

These notes describe the protocol boundary implemented by this repository. They are based on the installed PKU NetDisk 7.0.6-3-281 client (`/opt/pku-netdisk/current`) and live requests made against the service. They contain no account data or bearer tokens.

## Session

The web client starts at `/anyshare/oauth2/login?lang=zh-cn&redirect=/anyshare/`. The server redirects to `/oauth2/signin` and sets the OAuth challenge and state cookies. The third-party link is `/web/v1/sso?ty=auth`; it posts the official IAAA form to `https://iaaa.pku.edu.cn/iaaa/oauth.jsp`. IAAA account passwords are RSA PKCS#1 v1.5 encrypted with the public key from `/iaaa/getPublicKey.do`, then sent to `/iaaa/oauthlogin.do`. The returned one-time token is sent back to `/oauth2/signin` with the challenge and the `_rand` parameter used by the web client. The resulting `client.oauth2_token` and `client.oauth2_refresh_token` cookies are kept in a private in-memory jar during a session. The command-line account store may persist these two renewable cookies and the login password in an owner-only `0600` file so a selected account can resume without prompting.

The refresh operation is a GET to `/anyshare/oauth2/login/refreshToken?isforced=true`. Scheduled renewal runs hourly, including when the filesystem is idle. A request that receives HTTP 401 also refreshes once and retries with the new token. All renewals use the same cookie jar and are serialized so parallel FUSE operations cannot rotate the same session concurrently. A rejected refresh token causes the mount to exit for supervised login; temporary network failures retain the mount and retry.

## Namespace

`GET /api/efast/v1/entry-doc-lib` returns document libraries. The driver exposes them as the synthetic mount root unless `--root` selects one library or descendant folder. Directory contents use the paginated endpoint:

```text
GET /api/efast/v1/folders/{url-escaped-gns-id}/sub_objects
    ?limit=1000&sort=name&direction=asc[&marker=...]
```

The legacy `POST /api/efast/v1/dir/list` endpoint is useful for diagnostics but is not used by the FUSE namespace. Every selected path is resolved from an allowed library; the driver never concatenates an unchecked user path into an API URL.

The `ls` command uses the same root and paginated folder endpoints. With no selector it lists accessible libraries; with a display path or allowed `gns://...` selector it lists that folder. The terminal UI keeps the resolved parent ID in memory while browsing, so upload and download actions use the same checked namespace.

## Files and mutations

The driver uses these JSON calls:

| Operation | Endpoint |
| --- | --- |
| metadata | `POST /api/efast/v1/file/attribute` or `dir/attribute` |
| permission check | `POST /api/eacp/v1/perm1/checkall` |
| create directory | `POST /api/efast/v1/dir/create` |
| delete | `POST /api/efast/v1/file/delete` or `dir/delete` |
| rename | `POST /api/efast/v1/file/rename` or `dir/rename` |
| move | `POST /api/efast/v1/file/move` or `dir/move` |

Downloads begin with `POST /api/efast/v1/file/osdownload`. The returned signed request is restricted to HTTPS on an HTTPS server and is used with a checked `Range` header for `ReadAt`.

The standalone `download` command uses the same `osdownload` request with bounded byte ranges and verifies the exact selected revision size before atomically installing a local file. The range size is kept below the desktop portal's documented 2 GiB response limit; a fresh signed URL is obtained for each range. A server policy or quota can still reject a download.

Uploads use the desktop sync flow for every non-empty file: first `POST /api/efast/v1/file/osoption` obtains `partminsize`, `partmaxsize`, and `partmaxnum`; then the client uses `osinitmultiupload`, one signed `osuploadpart` request per part, `oscompleteupload`, and the signed completion request returned by that endpoint. This avoids the ordinary `osbeginupload` path's configured per-file limit (`403002070`, returned as `detail.file_limit_size`). The client increases the negotiated part size when needed and rejects only files beyond the server-reported multipart capacity. A file overwrite includes its current `rev`; the driver obtains a fresh revision before upload and reports a conflict if it changed.

These measures only remove client/request-size limits. Account quota, administrator file policies, forbidden types, storage-service limits, and server-side object-storage limits remain authoritative and are reported as errors.

## Recovery boundary

The local cache is created below `--cache-dir` with mode `0700`. A dirty inode has an adjacent JSON journal containing only the remote ID, parent ID, revision, cache basename, and update time. It does not contain credentials or signed URLs. A failed upload returns an error to the caller and leaves the cache and journal in place for `recover` inspection.
