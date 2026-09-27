#!/usr/bin/env python3
"""Inject per-release pins into the one-click installers.

Reads the signed manifest produced by `mesh release sign` and rewrites the block
between the `# >>> PINNED` and `# <<< PINNED` markers in install.sh / install.ps1
with the version, the release public key and the per-platform SHA-256 values.

The repository copies keep those markers empty, so a copy that was not produced
by a release cannot silently fall back to an unverified install.

Usage:
  pin-installers.py <manifest.json> <version> <public-key.pem> <install.sh> <install.ps1>
"""

import json
import re
import sys
from pathlib import Path

START = "# >>> PINNED"
END = "# <<< PINNED"

UNIX_TARGETS = ("darwin-amd64", "darwin-arm64", "linux-amd64", "linux-arm64")
WINDOWS_TARGETS = ("windows-amd64", "windows-arm64")


def fail(message: str) -> "None":
    raise SystemExit(f"pin-installers: {message}")


def load_hashes(manifest_path: Path) -> dict:
    try:
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as err:
        fail(f"cannot read manifest {manifest_path}: {err}")
    files = manifest.get("files")
    if not isinstance(files, dict) or not files:
        fail("manifest has no files")
    hashes = {}
    for target, entry in files.items():
        digest = entry.get("sha256") if isinstance(entry, dict) else None
        if not isinstance(target, str) or not re.fullmatch(r"[0-9a-f]{64}", digest or ""):
            fail(f"manifest entry {target!r} has no valid sha256")
        hashes[target] = digest
    return hashes


def replace_block(text: str, block: str, path: Path) -> str:
    lines = text.splitlines(keepends=True)
    try:
        start = next(i for i, line in enumerate(lines) if line.startswith(START))
        end = next(i for i, line in enumerate(lines) if line.startswith(END))
    except StopIteration:
        fail(f"{path} has no {START} / {END} markers")
    if end <= start:
        fail(f"{path} has inverted markers")
    return "".join(lines[: start + 1]) + block + "".join(lines[end:])


def shell_block(version: str, public_key: str, hashes: dict) -> str:
    if not public_key.endswith("\n"):
        public_key += "\n"
    entries = [f'PINNED_VERSION="{version}"']
    entries.append("PINNED_PUBLIC_KEY='" + public_key.rstrip("\n") + "'")
    for target in UNIX_TARGETS:
        digest = hashes.get(target)
        if not digest:
            fail(f"manifest is missing {target}")
        entries.append(f'PINNED_{target.replace("-", "_")}="{digest}"')
    return "\n".join(entries) + "\n"


def powershell_block(version: str, public_key: str, hashes: dict) -> str:
    if not public_key.endswith("\n"):
        public_key += "\n"
    rows = []
    for target in WINDOWS_TARGETS:
        digest = hashes.get(target)
        if not digest:
            fail(f"manifest is missing {target}")
        rows.append(f"    '{target}' = '{digest}'")
    return (
        f"$PinnedVersion = '{version}'\n"
        "$PinnedPublicKey = @'\n"
        f"{public_key.rstrip(chr(10))}\n"
        "'@\n"
        "$PinnedHashes = @{\n" + "\n".join(rows) + "\n}\n"
    )


def main(argv: list) -> int:
    if len(argv) != 6:
        fail(__doc__.strip().splitlines()[-1])
    manifest_path = Path(argv[1])
    version = argv[2]
    public_key_path = Path(argv[3])
    shell_path = Path(argv[4])
    powershell_path = Path(argv[5])

    if not re.fullmatch(r"[0-9A-Za-z._-]+", version):
        fail(f"invalid version {version!r}")

    hashes = load_hashes(manifest_path)
    try:
        public_key = public_key_path.read_text(encoding="utf-8")
    except OSError as err:
        fail(f"cannot read public key {public_key_path}: {err}")
    if "BEGIN PUBLIC KEY" not in public_key:
        fail("public key is not a PEM encoded public key")

    for path, block in (
        (shell_path, shell_block(version, public_key, hashes)),
        (powershell_path, powershell_block(version, public_key, hashes)),
    ):
        try:
            original = path.read_text(encoding="utf-8")
        except OSError as err:
            fail(f"cannot read {path}: {err}")
        path.write_text(replace_block(original, block, path), encoding="utf-8")
        print(f"pinned {path} for {version}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
