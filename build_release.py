#!/usr/bin/env python3
"""Build and package RomCompressor.go release binaries."""

from __future__ import annotations

import argparse
import gzip
import hashlib
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import zipfile


TARGETS = {
    "windows/amd64": ("windows", "amd64", None, "amd64", "zip", "romcompressor.exe"),
    "windows/386": ("windows", "386", None, "386", "zip", "romcompressor.exe"),
    "windows/arm64": ("windows", "arm64", None, "arm64", "zip", "romcompressor.exe"),
    "linux/amd64": ("linux", "amd64", None, "amd64", "tar.gz", "romcompressor"),
    "linux/386": ("linux", "386", None, "386", "tar.gz", "romcompressor"),
    "linux/arm64": ("linux", "arm64", None, "arm64", "tar.gz", "romcompressor"),
    "linux/armv7": ("linux", "arm", "7", "armv7", "tar.gz", "romcompressor"),
    "darwin/amd64": ("darwin", "amd64", None, "amd64", "tar.gz", "romcompressor"),
    "darwin/arm64": ("darwin", "arm64", None, "arm64", "tar.gz", "romcompressor"),
}
VERSION_RE = re.compile(r"^v?(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)$")
COPY_BUFFER = 1 << 20


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Cross-compile and package RomCompressor.go releases."
    )
    parser.add_argument("version", nargs="?", help="release version, such as 0.1.0 or v0.1.0")
    parser.add_argument(
        "-t", "--target", action="append", default=[], metavar="OS/ARCH",
        help="target to build; repeat or comma-separate (default: all)",
    )
    parser.add_argument(
        "-o", "--output", type=Path, metavar="DIR",
        help="output directory (default: dist/vVERSION)",
    )
    parser.add_argument("--list-targets", action="store_true", help="print supported targets and exit")
    return parser.parse_args()


def selected_targets(values: list[str]) -> list[str]:
    if not values:
        return list(TARGETS)
    selected: list[str] = []
    for value in values:
        for target in value.split(","):
            target = target.strip().lower()
            if not target:
                continue
            if target not in TARGETS:
                raise ValueError(f"unknown target {target!r}; use --list-targets to see valid targets")
            if target not in selected:
                selected.append(target)
    if not selected:
        raise ValueError("at least one non-empty target is required")
    return selected


def package_archive(path: Path, files: list[Path]) -> None:
    if path.suffix == ".zip":
        with zipfile.ZipFile(path, "x") as archive:
            for index, source in enumerate(files):
                info = zipfile.ZipInfo(source.name, date_time=(1980, 1, 1, 0, 0, 0))
                info.create_system = 3
                info.compress_type = zipfile.ZIP_DEFLATED
                info.file_size = source.stat().st_size
                info.external_attr = (stat.S_IFREG | (0o755 if index == 0 else 0o644)) << 16
                with source.open("rb") as reader, archive.open(info, "w") as writer:
                    shutil.copyfileobj(reader, writer, COPY_BUFFER)
        return
    with path.open("xb") as compressed:
        with gzip.GzipFile(filename="", mode="wb", fileobj=compressed, mtime=0) as stream:
            with tarfile.open(fileobj=stream, mode="w|", format=tarfile.USTAR_FORMAT) as archive:
                for index, source in enumerate(files):
                    info = tarfile.TarInfo(source.name)
                    info.size = source.stat().st_size
                    info.mode = 0o755 if index == 0 else 0o644
                    info.mtime = info.uid = info.gid = 0
                    info.uname = info.gname = ""
                    with source.open("rb") as reader:
                        archive.addfile(info, reader)


def checksum(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(COPY_BUFFER), b""):
            digest.update(chunk)
    return digest.hexdigest()


def matches_identity(path: Path, identity: os.stat_result) -> bool:
    try:
        return os.path.samestat(path.lstat(), identity)
    except FileNotFoundError:
        return False


def build_release(root: Path, output: Path, version: str, targets: list[str]) -> None:
    output.parent.mkdir(parents=True, exist_ok=True)
    # Reserving the destination prevents replacement even when another build
    # creates an empty output directory concurrently.
    output.mkdir(mode=0o700)
    output_identity = output.lstat()
    created: list[tuple[Path, os.stat_result]] = []
    complete = False
    staging: Path | None = None
    try:
        staging = Path(tempfile.mkdtemp(prefix=".romcompressor-release-", dir=output.parent)).resolve()
        if staging.parent != output.parent.resolve():
            raise OSError("release staging escaped its output parent")
        work = staging / ".work"
        work.mkdir()
        environment = os.environ.copy()
        for name in ("GOOS", "GOARCH", "GOARM"):
            environment.pop(name, None)
        environment["CGO_ENABLED"] = "0"
        artifacts: list[Path] = []
        for target in targets:
            goos, goarch, goarm, label, extension, binary_name = TARGETS[target]
            print(f"Building {target}...", flush=True)
            target_env = environment.copy()
            target_env.update({"GOOS": goos, "GOARCH": goarch})
            if goarm:
                target_env["GOARM"] = goarm
            target_work = work / target.replace("/", "-")
            target_work.mkdir()
            binary = target_work / binary_name
            subprocess.run(
                ["go", "build", "-trimpath", "-buildvcs=false", "-ldflags",
                 f"-s -w -X main.version=v{version}", "-o", str(binary), "./cmd/romcompressor"],
                cwd=root, env=target_env, check=True,
            )
            archive = staging / f"romcompressor_{version}_{goos}_{label}.{extension}"
            package_archive(archive, [binary, root / "LICENSE", root / "THIRD_PARTY_NOTICES.md"])
            artifacts.append(archive)
        checksums = staging / "checksums.txt"
        checksums.write_text(
            "".join(f"{checksum(path)}  {path.name}\n" for path in sorted(artifacts)),
            encoding="ascii",
        )
        for source in [*artifacts, checksums]:
            if not matches_identity(output, output_identity):
                raise OSError("release output directory changed during build")
            destination = output / source.name
            with destination.open("xb") as writer:
                created.append((destination, os.fstat(writer.fileno())))
                with source.open("rb") as reader:
                    shutil.copyfileobj(reader, writer, COPY_BUFFER)
        complete = True
    finally:
        try:
            if staging is not None and staging.parent == output.parent.resolve():
                shutil.rmtree(staging)
        finally:
            if not complete and matches_identity(output, output_identity):
                for path, identity in reversed(created):
                    if matches_identity(path, identity):
                        path.unlink()
                try:
                    output.rmdir()
                except OSError:
                    pass


def main() -> int:
    args = parse_args()
    if args.list_targets:
        print("\n".join(TARGETS))
        return 0
    if not args.version:
        print("error: VERSION is required unless --list-targets is used", file=sys.stderr)
        return 2
    match = VERSION_RE.fullmatch(args.version)
    if not match:
        print("error: VERSION must look like 0.1.0 or v0.1.0-beta.1", file=sys.stderr)
        return 2
    try:
        targets = selected_targets(args.target)
    except ValueError as error:
        print(f"error: {error}", file=sys.stderr)
        return 2
    if shutil.which("go") is None:
        print("error: Go is not installed or is not on PATH", file=sys.stderr)
        return 1
    root = Path(__file__).resolve().parent
    version = match.group(1)
    # Do not resolve the final component: even a dangling output symlink must
    # count as an existing output and be rejected by mkdir.
    output = Path(os.path.abspath(args.output.expanduser())) if args.output else root / "dist" / f"v{version}"
    try:
        build_release(root, output, version, targets)
    except KeyboardInterrupt:
        print("error: release build interrupted", file=sys.stderr)
        return 130
    except (OSError, subprocess.CalledProcessError) as error:
        print(f"error: release build failed: {error}", file=sys.stderr)
        return 1
    print(f"Release files created in {output}")
    for path in sorted(output.iterdir()):
        print(f"  {path.name} ({path.stat().st_size} bytes)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
