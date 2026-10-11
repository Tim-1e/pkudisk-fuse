# Releasing pkudisk-fuse

Follow [Go's publishing guidance](https://go.dev/doc/modules/publishing):
tidy dependencies, test, commit, tag, push the tag, and request the module from a
proxy. Never move or reuse a published tag; fixes get a new version.

## Prepare

1. Add `docs/releases/vX.Y.Z.md` with changes, installation instructions and
   material limitations. Run `make test` and `make check` locally.
2. With GoReleaser v2.18.3 installed, optionally rehearse packaging using
   `goreleaser release --snapshot --clean`. This builds all six archives locally
   without publishing. Check the native binary's `version` output.
3. Commit and push to `main`. Wait for CI to pass on that exact commit. CI uses
   three native runners; the Linux runner also cross-builds all six targets.
4. Create and push an annotated semantic version tag:

   ```sh
   git tag -a vX.Y.Z -m 'Release vX.Y.Z'
   git push origin vX.Y.Z
   ```

## Automated publication

The release workflow uses one Linux job. It checks that CI passed on `main` for
the tagged commit, rather than running the test matrix again. An untested or
failed commit cannot be released; wait for CI before pushing the tag.

GoReleaser builds Linux, macOS and Windows amd64/arm64 archives using the latest
Go 1.26 patch release, `CGO_ENABLED=0` and `-trimpath`. The job checks the native
binary's version, publishes the archives and checksums with `gh release create`,
and requests the tagged module from Go's official proxy. Rerun a failed workflow
before publication; it does not replace an existing release or its assets.

Archives include LICENSE, NOTICE, README, platform and transfer documentation,
shell completions and dependency license files. `scripts/release-files.sh`
collects licenses for all three operating systems using pinned `go-licenses`.
Generated `licenses/` and `completions/` directories are ignored by Git. Actions
use major-version tags. WinGet publication is deferred.

To verify a source installation manually, run outside the checkout:

```sh
go install github.com/thezzisu/pkudisk-fuse/cmd/pkudisk-fuse@vX.Y.Z
pkudisk-fuse version
```

Ensure `GOBIN`, or its default `~/go/bin`, is on `PATH`.

## License provenance

`LICENSE` is the verbatim text from
<https://www.apache.org/licenses/LICENSE-2.0.txt>, including the unmodified
appendix. Keep project attribution in NOTICE and retain upstream license text
unchanged. Do not generate or rewrite the license.
