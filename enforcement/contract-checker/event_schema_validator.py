#!/usr/bin/env python3
"""
F33D3R event schema validator.
Validates all JSON schemas in events/schemas/ are well-formed
and checks that all required top-level fields are present.

Runs in CI at the enforce stage.
"""
import json
import sys
from pathlib import Path

ROOT = Path(__file__).parent.parent.parent
SCHEMAS_DIR = ROOT / "events" / "schemas"

REQUIRED_FIELDS = {
    "event_id",
    "event_type",
    "schema_version",
    "pial_id",
    "timestamp",
}

REQUIRED_META = {
    "$schema",
    "title",
    "description",
    "type",
    "required",
    "properties",
}


def validate_schema_file(path: Path) -> list[str]:
    errors = []
    try:
        schema = json.loads(path.read_text())
    except json.JSONDecodeError as e:
        return [f"  PARSE ERROR in {path.name}: {e}"]

    # Must have required metadata fields
    for field in REQUIRED_META:
        if field not in schema:
            errors.append(f"  MISSING '{field}' in {path.name}")

    # Properties must include the platform envelope fields
    props = schema.get("properties", {})
    required_in_schema = set(schema.get("required", []))
    for field in REQUIRED_FIELDS:
        if field not in props:
            errors.append(f"  MISSING property '{field}' in {path.name}")
        if field not in required_in_schema:
            errors.append(f"  '{field}' not in required[] in {path.name}")

    return errors


def main():
    if not SCHEMAS_DIR.exists():
        print(f"ERROR: {SCHEMAS_DIR} does not exist")
        sys.exit(1)

    schema_files = sorted(SCHEMAS_DIR.glob("*.json"))
    if not schema_files:
        print("WARNING: No schema files found in events/schemas/")
        sys.exit(0)

    print(f"Validating {len(schema_files)} event schema(s)...")
    all_errors = []

    for schema_file in schema_files:
        errors = validate_schema_file(schema_file)
        if errors:
            all_errors.extend([f"[{schema_file.name}]"] + errors)

    print()
    if all_errors:
        print(f"FAILED — {len(all_errors)} error(s):\n")
        for e in all_errors:
            print(e)
        sys.exit(1)
    else:
        print(f"OK — all {len(schema_files)} schemas valid")
        sys.exit(0)


if __name__ == "__main__":
    main()
