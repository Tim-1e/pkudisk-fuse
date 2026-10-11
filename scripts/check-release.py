#!/usr/bin/env python3
"""Check published archives and run the host platform's packaged executable."""

import argparse
import hashlib
import platform
from pathlib import Path
import subprocess
import tarfile
import tempfile
import zipfile


def check(directory: Path, version: str) -> None:
    root = Path(__file__).resolve().parent.parent
    required = {
        "LICENSE", "NOTICE", "README.md", "docs/platform-support.md",
        "docs/transfer-limits.md", "licenses/Go-LICENSE",
        *(f"completions/pkudisk-fuse.{shell}" for shell in ("bash", "zsh", "fish", "powershell")),
    }
    hashes = {}
    for line in (directory / "checksums.txt").read_text().splitlines():
        digest, name = line.split(maxsplit=1)
        name = name.lstrip("*")
        if name != Path(name).name or name in hashes:
            raise ValueError(f"invalid checksum entry: {name}")
        hashes[name] = digest

    host_os = {"Linux": "linux", "Darwin": "darwin", "Windows": "windows"}[platform.system()]
    host_arch = {"x86_64": "amd64", "AMD64": "amd64", "arm64": "arm64", "ARM64": "arm64", "aarch64": "arm64"}[platform.machine()]
    expected = {
        f"pkudisk-fuse_{version.removeprefix('v')}_{os_name}_{arch}.{('zip' if os_name == 'windows' else 'tar.gz')}"
        for os_name in ("linux", "darwin", "windows") for arch in ("amd64", "arm64")
    }
    if set(hashes) != expected:
        raise ValueError(f"archive set differs: missing={expected - set(hashes)}, extra={set(hashes) - expected}")

    for name, digest in hashes.items():
        path = directory / name
        if hashlib.sha256(path.read_bytes()).hexdigest() != digest:
            raise ValueError(f"checksum mismatch: {name}")
        is_windows = name.endswith(".zip")
        executable = "pkudisk-fuse.exe" if is_windows else "pkudisk-fuse"
        if is_windows:
            with zipfile.ZipFile(path) as archive:
                names = set(archive.namelist())
                license_text = archive.read("LICENSE")
                binary = archive.read(executable)
        else:
            with tarfile.open(path, "r:gz") as archive:
                names = set(archive.getnames())
                license_text = archive.extractfile("LICENSE").read()
                binary = archive.extractfile(executable).read()
                if archive.getmember(executable).mode & 0o111 == 0:
                    raise ValueError(f"binary is not executable: {name}")
        if not required.union({executable}) <= names:
            raise ValueError(f"incomplete archive {name}: {required.union({executable}) - names}")
        for dependency in ("github.com/hanwen/go-fuse", "github.com/winfsp/cgofuse", "github.com/spf13/cobra"):
            if not any(entry.startswith(f"licenses/{dependency}/") for entry in names):
                raise ValueError(f"missing dependency license: {dependency} in {name}")
        if license_text != (root / "LICENSE").read_bytes():
            raise ValueError(f"Apache license text differs: {name}")
        print(f"Verified {name}")

        if f"_{host_os}_{host_arch}." in name:
            with tempfile.TemporaryDirectory(prefix="pkudisk-release-") as temp:
                program = Path(temp) / executable
                program.write_bytes(binary)
                program.chmod(0o755)
                result = subprocess.run([str(program), "version"], text=True, capture_output=True, check=True)
                if result.stdout != f"pkudisk-fuse {version}\n" or result.stderr:
                    raise ValueError(f"incorrect version output: {result.stdout!r} {result.stderr!r}")
                for args in (("--help",), ("ls", "--help"), ("mount", "--help"), ("recover", "--help")):
                    result = subprocess.run([str(program), *args], text=True, capture_output=True, check=True)
                    if "Usage:" not in result.stdout or result.stderr:
                        raise ValueError(f"invalid help output: {args}")
                for shell in ("bash", "zsh", "fish", "powershell"):
                    result = subprocess.run([str(program), "completion", shell], capture_output=True, check=True)
                    if not result.stdout or result.stderr:
                        raise ValueError(f"invalid completion output: {shell}")
                result = subprocess.run([str(program), "root"], capture_output=True)
                if result.returncode != 2 or result.stdout:
                    raise ValueError("removed root command is still accepted")
                print(f"Packaged executable passed on {host_os}/{host_arch}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    parser.add_argument("version", help="expected version output, including the v prefix")
    args = parser.parse_args()
    check(args.directory, args.version)
