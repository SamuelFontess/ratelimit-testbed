import json
import os
import shlex
import subprocess
from pathlib import Path

import pytest

LIB = Path(__file__).resolve().parent / "lib.sh"
SETUP = LIB.parent / "setup.sh"
ARM64_SHA = "a" * 64
AMD64_SHA = "b" * 64
DARWIN_SHA = "c" * 64
MSI_SHA = "d" * 64
GO_JSON = [
    {
        "version": "go1.25.0",
        "files": [
            {
                "kind": "archive",
                "os": "linux",
                "arch": "arm64",
                "filename": "go1.25.0.linux-arm64.tar.gz",
                "sha256": "0" * 64,
            }
        ],
    },
    {
        "version": "go1.26.7",
        "files": [
            {
                "kind": "archive",
                "os": "linux",
                "arch": "arm64",
                "filename": "go1.26.7.linux-arm64.tar.gz",
                "sha256": ARM64_SHA,
            },
            {
                "kind": "archive",
                "os": "linux",
                "arch": "amd64",
                "filename": "go1.26.7.linux-amd64.tar.gz",
                "sha256": AMD64_SHA,
            },
            {
                "kind": "archive",
                "os": "darwin",
                "arch": "arm64",
                "filename": "go1.26.7.darwin-arm64.tar.gz",
                "sha256": DARWIN_SHA,
            },
            {
                "kind": "installer",
                "os": "linux",
                "arch": "arm64",
                "filename": "go1.26.7.linux-arm64.msi",
                "sha256": MSI_SHA,
            },
        ],
    },
]

NO_FINGERPRINT = "pub   rsa4096 2024-05-29 [SC]\n      8540A6F18833A80E9C1653A42FD21310B49F6B46\n"
WITH_FINGERPRINT = "pub   rsa2048 2011-08-19 [SC]\n      573BFD6B3D8FBC641079A6ABABF5BD827BD9BF62\n"


def run(expr: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["bash", "-c", f"source {shlex.quote(str(LIB))}\n{expr}"],
        capture_output=True,
        text=True,
    )


@pytest.fixture
def go_json(tmp_path: Path) -> Path:
    path = tmp_path / "go.json"
    path.write_text(json.dumps(GO_JSON))
    return path


@pytest.mark.parametrize(
    ("have", "want", "expected"),
    [
        ("1.28.3", "1.28", True),
        ("1.28", "1.28", True),
        ("1.28.10", "1.28.3", True),
        ("1.27.9", "1.28", False),
        ("1.24.0", "1.28", False),
        ("1.26.7", "1.22", True),
        ("", "1.28", False),
    ],
)
def test_version_at_least(have: str, want: str, expected: bool) -> None:
    result = run(f"version_at_least {shlex.quote(have)} {shlex.quote(want)}")
    assert (result.returncode == 0) is expected


@pytest.mark.parametrize(("machine", "arch"), [("x86_64", "amd64"), ("aarch64", "arm64")])
def test_go_arch_maps(machine: str, arch: str) -> None:
    result = run(f"go_arch {shlex.quote(machine)}")
    assert result.returncode == 0
    assert result.stdout.strip() == arch


def test_go_arch_rejects_unknown() -> None:
    result = run("go_arch riscv64")
    assert result.returncode != 0
    assert "riscv64" in result.stderr


@pytest.mark.parametrize(
    ("arch", "filename", "sha256"),
    [
        ("arm64", "go1.26.7.linux-arm64.tar.gz", ARM64_SHA),
        ("amd64", "go1.26.7.linux-amd64.tar.gz", AMD64_SHA),
    ],
)
def test_go_archive_field(go_json: Path, arch: str, filename: str, sha256: str) -> None:
    args = f"{shlex.quote(str(go_json))} go1.26.7 {arch}"
    name = run(f"go_archive_field {args} filename")
    digest = run(f"go_archive_field {args} sha256")
    assert name.stdout.strip() == filename
    assert digest.stdout.strip() == sha256


def test_go_archive_field_unknown_version(go_json: Path) -> None:
    result = run(f"go_archive_field {shlex.quote(str(go_json))} go9.9.9 arm64 filename")
    assert result.returncode == 0
    assert result.stdout.strip() == ""


def test_key_fingerprint_present() -> None:
    result = run(f"key_fingerprint_present {shlex.quote(WITH_FINGERPRINT)}")
    assert result.returncode == 0


def test_key_without_fingerprint_rejected() -> None:
    result = run(f"key_fingerprint_present {shlex.quote(NO_FINGERPRINT)}")
    assert result.returncode != 0


FAKES = {
    "curl": r"""#!/usr/bin/env bash
printf 'curl %s\n' "$*" >> "$FAKE_LOG"
out=
while [[ $# -gt 0 ]]; do
  case $1 in
    -o) out=$2; shift 2 ;;
    *) shift ;;
  esac
done
printf 'chave\n' > "$out"
""",
    "gpg": r"""#!/usr/bin/env bash
printf 'gpg %s gnupghome=%s\n' "$*" "${GNUPGHOME:-}" >> "$FAKE_LOG"
case " $* " in
  *" --dearmor "*) cat ;;
  *" import-show "*) cat "$FAKE_LISTING" ;;
  *) exit 0 ;;
esac
""",
    "sudo": r"""#!/usr/bin/env bash
printf 'sudo %s\n' "$*" >> "$FAKE_LOG"
exec "$@"
""",
    "lsb_release": r"""#!/usr/bin/env bash
printf 'noble\n'
""",
    "apt-get": r"""#!/usr/bin/env bash
printf 'apt-get %s\n' "$*" >> "$FAKE_LOG"
""",
    "apt-mark": r"""#!/usr/bin/env bash
printf 'apt-mark %s\n' "$*" >> "$FAKE_LOG"
""",
    "systemctl": r"""#!/usr/bin/env bash
printf 'systemctl %s\n' "$*" >> "$FAKE_LOG"
""",
    "apt-cache": r"""#!/usr/bin/env bash
printf 'apt-cache %s\n' "$*" >> "$FAKE_LOG"
printf 'nginx-module-njs | 1.28.3+0.9.6-1~noble | http://nginx.org/packages/ubuntu noble nginx\n'
""",
}


def run_nginx_org(tmp_path: Path, listing: str):
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    for name, body in FAKES.items():
        path = bin_dir / name
        path.write_text(body)
        path.chmod(0o755)
    log = tmp_path / "calls.log"
    listing_file = tmp_path / "listing"
    listing_file.write_text(listing)
    tmp_dir = tmp_path / "tmp"
    tmp_dir.mkdir()
    env = os.environ.copy()
    env["PATH"] = f"{bin_dir}{os.pathsep}{env['PATH']}"
    env["FAKE_LOG"] = str(log)
    env["FAKE_LISTING"] = str(listing_file)
    keyring = tmp_path / "keyring.gpg"
    source = tmp_path / "nginx.list"
    script = "\n".join(
        [
            f"source {shlex.quote(str(SETUP))}",
            f"TMP_DIR={shlex.quote(str(tmp_dir))}",
            f"NGINX_KEYRING={shlex.quote(str(keyring))}",
            f"NGINX_ORG_SOURCE={shlex.quote(str(source))}",
            f"NGINX_ORG_PREFS={shlex.quote(str(tmp_path / '99nginx'))}",
            "APT_UPDATED=0",
            "install_nginx_from_nginx_org",
        ]
    )
    result = subprocess.run(["bash", "-c", script], capture_output=True, text=True, env=env)
    calls = log.read_text().splitlines() if log.exists() else []
    return result, calls, source, keyring, tmp_dir / "gnupg"


def test_nginx_org_key_without_fingerprint_aborts(tmp_path: Path) -> None:
    result, calls, source, keyring, _ = run_nginx_org(tmp_path, NO_FINGERPRINT)
    assert result.returncode != 0
    assert "fingerprint" in result.stderr
    assert not any("apt-get install" in call for call in calls)
    assert not any("sudo tee" in call for call in calls)
    assert not source.exists()
    assert not keyring.exists()


def test_nginx_org_key_with_fingerprint_adds_source(tmp_path: Path) -> None:
    result, calls, source, keyring, gnupg_home = run_nginx_org(tmp_path, WITH_FINGERPRINT)
    assert result.returncode == 0, result.stderr
    check = next(call for call in calls if "import-show" in call)
    assert check.endswith(f"gnupghome={gnupg_home}")
    assert gnupg_home.is_dir()
    assert keyring.exists()
    assert "deb [signed-by=" in source.read_text()
    checked = calls.index(check)
    assert checked < next(i for i, call in enumerate(calls) if "sudo tee" in call)
