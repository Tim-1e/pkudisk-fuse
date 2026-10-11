# Releasing pkudisk-fuse

The project uses one Go module at the repository root. Versions use semantic
Git tags such as `v0.1.0`. Follow [Go's publishing guidance](https://go.dev/doc/modules/publishing):
tidy dependencies, test, commit, tag, push the tag, and request the module from a
proxy. Never move or reuse a published tag; publish a new version for fixes.

## Prepare

1. Run `go mod tidy`, `make test` and `make check`. CI must pass on the final
   release commit. Linux FUSE and Windows WinFsp kernel tests are required in CI.
2. Add `docs/releases/vX.Y.Z.md` with installation instructions, changes and
   material limitations. A release without this file fails before packaging.
3. With GoReleaser v2.18.3 installed, run `goreleaser check` and
   `goreleaser release --snapshot --clean`. On Linux or macOS, check the snapshot
   with `python3 scripts/check-release.py dist v0.0.0-dev` before the first tag;
   for subsequent snapshots, use the version in `dist/metadata.json` with a `v`
   prefix. Snapshots are local and do not publish.
4. Review and commit the changes, then push the release commit to `main` and
   wait for CI. Dispatch the Release workflow on `main` to build snapshot
   archives and run native packaged-binary checks; a branch dispatch uploads
   workflow artifacts and does not publish a GitHub release:

   ```sh
   gh workflow run release.yml --ref main
   ```

5. Once that rehearsal passes, create an annotated tag on that exact commit:

   ```sh
   git tag -a vX.Y.Z -m 'Release vX.Y.Z'
   git push origin vX.Y.Z
   ```

Actions use major-version tags. The GoReleaser version is in `.github/workflows/release.yml`.
Builds use the latest Go 1.26 patch release, `CGO_ENABLED=0` and `-trimpath`.
Source installations recover their version from Go build metadata; archive
binaries receive it through linker flags.

## Automated publication

The tag runs the reusable CI workflow, builds six archives with GoReleaser,
and uploads them as a workflow artifact. A separate matrix verifies
every checksum and archive, then executes the native packaged binary on Linux,
macOS and Windows. Only after all checks pass does `gh release create` publish
those same archives and `checksums.txt`; the publish job alone has write access.
After publication, a read-only job requests the version from Go's official
proxy and verifies `go install` and version reporting outside the checkout.

The archives include the official project license, NOTICE, README, platform and
transfer documentation, shell completions, and the Go/dependency license files.
`scripts/release-files.sh` collects dependency licenses for all three operating
systems using pinned `go-licenses`; the generated `licenses/` and `completions/`
directories are ignored by Git. WinGet publication is deferred.

After publication, test a fresh installation outside the checkout:

```sh
GOPROXY=proxy.golang.org go list -m github.com/thezzisu/pkudisk-fuse@vX.Y.Z
go install github.com/thezzisu/pkudisk-fuse/cmd/pkudisk-fuse@vX.Y.Z
pkudisk-fuse version
```

Ensure `GOBIN`, or its default `~/go/bin`, is on `PATH`. Also download the
published archives, verify their checksums and run the packaged-binary check.
For a failed run before publication, rerun the failed GitHub Actions jobs.
The workflow can also be dispatched against the existing tag. It never replaces
an existing published release or its assets.

## License provenance

`LICENSE` was copied verbatim from
<https://www.apache.org/licenses/LICENSE-2.0.txt>, including the unmodified
appendix. Keep project attribution in `NOTICE` and retain upstream license and
notice text unchanged. Do not generate or rewrite the license text.
