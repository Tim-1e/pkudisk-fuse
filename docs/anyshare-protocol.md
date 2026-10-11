# AnyShare protocol notes

These notes describe the protocol boundary implemented by this repository. They are based on PKU NetDisk 7.0.6-3-281, including read-only inspection of its Debian package unpacked under `/tmp`, and live requests made against the service. They contain no account data or bearer tokens.

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

The standalone `download` command uses the same `osdownload` request with bounded 512 MiB byte ranges and verifies the exact selected revision size before atomically installing a local file. Each range obtains a fresh signed URL, requests the same revision, and checks `Content-Range` and byte count. No whole-file download size cap was found in this path. The official bundle also includes size errors for preview, conversion, and batch download routes; those do not establish a general object-storage download cap.

For uploads, `POST /api/efast/v1/file/osoption` obtains `partminsize`, `partmaxsize`, and `partmaxnum`. The preferred part size is clamped to the server's size bounds and raised to at least `ceil(length / partmaxnum)`. A file beyond `partmaxsize * partmaxnum` fails before a remote upload is created. Files larger than the negotiated part size use `osinitmultiupload`, one signed `osuploadpart` request per part, `oscompleteupload`, and the signed completion request returned by that endpoint. Small and empty files use `osbeginupload`. Both flows publish at `osendupload`. A file overwrite includes its current `rev`; the driver obtains a fresh revision before upload and reports a conflict if it changed.

PKU's `osbeginupload` rejects lengths beyond 5 GiB with HTTP 400/code `400000000`; multipart avoids that endpoint limit. Administrator file-size policies can still return `403002070` and `detail.file_limit_size` from either upload flow or download authorization. Such errors retain the byte limit in their message and map to `EFBIG` in a mount. See [the investigation](transfer-limits.md) for server-reported capacity and validation.

These measures only remove client/request-size limits. Account quota, administrator file policies, forbidden types, storage-service limits, and server-side object-storage limits remain authoritative and are reported as errors.

## Recovery boundary

The local cache is created below `--cache-dir` with mode `0700`. A dirty inode has an adjacent JSON journal containing only the remote ID, parent ID, revision, cache basename, and update time. It does not contain credentials or signed URLs. A failed upload returns an error to the caller and leaves the cache and journal in place for `recover` inspection.
