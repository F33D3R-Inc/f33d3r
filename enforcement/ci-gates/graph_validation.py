#!/usr/bin/env python3
"""
F33D3R dependency graph validator.
Runs in CI to block merges that violate brain boundary rules.

Usage:
  python3 enforcement/ci-gates/graph_validation.py
  python3 enforcement/ci-gates/graph_validation.py --strict
"""
import json
import os
import re
import sys
import yaml
from pathlib import Path

ROOT = Path(__file__).parent.parent.parent
RULES_FILE = ROOT / "enforcement" / "dependency-graph" / "rules.yaml"
FORBIDDEN_FILE = ROOT / "enforcement" / "dependency-graph" / "forbidden_edges.json"

STRICT = "--strict" in sys.argv


def load_rules():
    with open(RULES_FILE) as f:
        return yaml.safe_load(f)


def load_forbidden():
    with open(FORBIDDEN_FILE) as f:
        return json.load(f)


def scan_source_for_calls(brain_dir: Path, brain_name: str, rules: dict, forbidden: dict) -> list[str]:
    """
    Scan source files for HTTP calls to other brains.
    Looks for URL patterns referencing other brains' ports or service names.
    """
    violations = []
    forbidden_edges = {
        (e["from"], e["to"]): e["reason"]
        for e in forbidden["forbidden_edges"]
    }

    # Port → brain name mapping
    port_to_brain = {
        str(b.get("port", "")): name
        for name, b in rules["brains"].items()
    }

    # Service name patterns to detect
    service_patterns = {
        name: [
            name,
            data.get("service_dir", ""),
            f":{data.get('port', '')}",
        ]
        for name, data in rules["brains"].items()
    }

    source_files = list(brain_dir.rglob("*.rs")) + list(brain_dir.rglob("*.go"))
    for src in source_files:
        if "target" in src.parts or "vendor" in src.parts:
            continue
        try:
            content = src.read_text(errors="ignore")
        except Exception:
            continue

        for target_brain, patterns in service_patterns.items():
            if target_brain == brain_name:
                continue
            edge = (brain_name, target_brain)
            if edge not in forbidden_edges:
                continue
            for pattern in patterns:
                if not pattern:
                    continue
                # Look for HTTP client calls containing the target pattern
                http_patterns = [
                    rf'http://[^\s"\']*{re.escape(pattern)}',
                    rf'https://[^\s"\']*{re.escape(pattern)}',
                    rf'reqwest.*{re.escape(pattern)}',
                    rf'http\.NewRequest.*{re.escape(pattern)}',
                ]
                for hp in http_patterns:
                    matches = re.findall(hp, content, re.IGNORECASE)
                    if matches:
                        reason = forbidden_edges[edge]
                        violations.append(
                            f"  VIOLATION: {brain_name} → {target_brain}\n"
                            f"    File: {src.relative_to(ROOT)}\n"
                            f"    Match: {matches[0][:80]}\n"
                            f"    Rule: {reason}"
                        )
                        break

    return violations


def validate_version_pins(rules: dict) -> list[str]:
    """Check that all Dockerfiles use the pinned Rust/Go versions."""
    violations = []
    expected_rust = "rust:1.95.0-slim-bookworm"
    expected_go = "golang:1.26.2-alpine"

    for brain_name, brain in rules["brains"].items():
        svc_dir = brain.get("service_dir")
        if not svc_dir:
            continue
        dockerfile_paths = [
            ROOT / svc_dir / "docker" / "Dockerfile",
            ROOT / svc_dir / "Dockerfile",
        ]
        for df in dockerfile_paths:
            if not df.exists():
                continue
            content = df.read_text()
            first_from = next(
                (line for line in content.splitlines() if line.startswith("FROM")),
                ""
            )
            if "rust:" in first_from and expected_rust not in first_from:
                violations.append(
                    f"  VERSION PIN: {df.relative_to(ROOT)}\n"
                    f"    Expected: FROM {expected_rust}\n"
                    f"    Found:    {first_from}"
                )
            if "golang:" in first_from and expected_go not in first_from:
                violations.append(
                    f"  VERSION PIN: {df.relative_to(ROOT)}\n"
                    f"    Expected: FROM {expected_go}\n"
                    f"    Found:    {first_from}"
                )
    return violations


def validate_brain_map_sync(rules: dict) -> list[str]:
    """Check that BRAIN_MAP.md mentions all brains defined in rules.yaml."""
    violations = []
    brain_map = ROOT / "BRAIN_MAP.md"
    if not brain_map.exists():
        return [f"  MISSING: BRAIN_MAP.md not found"]
    content = brain_map.read_text().lower()
    for brain_name in rules["brains"]:
        if brain_name.lower() not in content:
            violations.append(
                f"  BRAIN_MAP: '{brain_name}' defined in rules.yaml but not found in BRAIN_MAP.md"
            )
    return violations


def main():
    print("F33D3R dependency graph validator")
    print(f"Root: {ROOT}")
    print(f"Strict mode: {STRICT}")
    print()

    rules = load_rules()
    forbidden = load_forbidden()

    all_violations = []

    # 1. Version pin checks
    print("[1/3] Checking version pins...")
    version_violations = validate_version_pins(rules)
    all_violations.extend(version_violations)

    # 2. Brain map sync
    print("[2/3] Checking BRAIN_MAP.md sync...")
    map_violations = validate_brain_map_sync(rules)
    all_violations.extend(map_violations)

    # 3. Dependency edge scan (only in strict mode to avoid false positives)
    if STRICT:
        print("[3/3] Scanning source for forbidden HTTP calls (strict mode)...")
        for brain_name, brain in rules["brains"].items():
            svc_dir = brain.get("service_dir")
            if not svc_dir:
                continue
            brain_path = ROOT / svc_dir
            if not brain_path.exists():
                continue
            violations = scan_source_for_calls(brain_path, brain_name, rules, forbidden)
            all_violations.extend(violations)
    else:
        print("[3/3] Source scan skipped (use --strict to enable)")

    print()
    if all_violations:
        print(f"FAILED — {len(all_violations)} violation(s) found:\n")
        for v in all_violations:
            print(v)
            print()
        sys.exit(1)
    else:
        print("OK — no violations found")
        sys.exit(0)


if __name__ == "__main__":
    main()
