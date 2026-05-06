#!/usr/bin/env python3
"""
F33D3R version policy enforcer.
Validates that all services use the platform minimum language versions.

Minimum versions (use these or newer):
  Rust:       1.95.0
  Go:         1.26.2
  HTMX:       2.0.10
  PostgreSQL: 18
  Tailwind:   4.2.4
  Python:     3.14.4
"""
import re
import sys
from pathlib import Path

ROOT = Path(__file__).parent.parent.parent

PINNED = {
    "rust": "1.95.0",
    "go": "1.26.2",
    "htmx": "2.0.10",
}

violations = []


def check_dockerfiles():
    for df in ROOT.rglob("Dockerfile"):
        if "target" in df.parts or ".git" in df.parts:
            continue
        content = df.read_text(errors="ignore")
        rel = df.relative_to(ROOT)

        # Check Rust version
        rust_matches = re.findall(r"FROM rust:([0-9.]+)-", content)
        for ver in rust_matches:
            if ver != PINNED["rust"]:
                violations.append(
                    f"  RUST VERSION: {rel}\n"
                    f"    Expected rust:{PINNED['rust']}-slim-bookworm\n"
                    f"    Found:    rust:{ver}-*"
                )

        # Check Go version
        go_matches = re.findall(r"FROM golang:([0-9.]+)-", content)
        for ver in go_matches:
            if ver != PINNED["go"]:
                violations.append(
                    f"  GO VERSION: {rel}\n"
                    f"    Expected golang:{PINNED['go']}-alpine\n"
                    f"    Found:    golang:{ver}-*"
                )


def check_go_mod():
    for go_mod in ROOT.rglob("go.mod"):
        if ".git" in go_mod.parts:
            continue
        content = go_mod.read_text(errors="ignore")
        rel = go_mod.relative_to(ROOT)
        match = re.search(r"^go\s+([0-9.]+)", content, re.MULTILINE)
        if match:
            ver = match.group(1)
            # go.mod uses major.minor, not major.minor.patch
            expected_major_minor = ".".join(PINNED["go"].split(".")[:2])
            if not ver.startswith(expected_major_minor):
                violations.append(
                    f"  GO MOD: {rel}\n"
                    f"    Expected: go {PINNED['go']}\n"
                    f"    Found:    go {ver}"
                )


def check_htmx():
    for html in ROOT.rglob("*.html"):
        if ".git" in html.parts or "target" in html.parts:
            continue
        content = html.read_text(errors="ignore")
        htmx_matches = re.findall(r"htmx\.org@([0-9.]+)/", content)
        for ver in htmx_matches:
            if ver != PINNED["htmx"]:
                violations.append(
                    f"  HTMX VERSION: {html.relative_to(ROOT)}\n"
                    f"    Expected: htmx.org@{PINNED['htmx']}\n"
                    f"    Found:    htmx.org@{ver}"
                )


def main():
    print("F33D3R version policy check")
    print(f"  Rust:  {PINNED['rust']}")
    print(f"  Go:    {PINNED['go']}")
    print(f"  HTMX:  {PINNED['htmx']}")
    print()

    check_dockerfiles()
    check_go_mod()
    check_htmx()

    if violations:
        print(f"FAILED — {len(violations)} version violation(s):\n")
        for v in violations:
            print(v)
            print()
        sys.exit(1)
    else:
        print("OK — all versions pinned correctly")
        sys.exit(0)


if __name__ == "__main__":
    main()
