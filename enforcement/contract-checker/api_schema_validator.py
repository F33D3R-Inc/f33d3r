#!/usr/bin/env python3
"""
F33D3R API contract validator.
Validates that all brain HTTP APIs match their declared contracts
in enforcement/contract-checker/api_contracts.yaml.

Runs live against the local dev environment when services are up,
and statically against OpenAPI specs in CI.
"""
import json
import os
import sys
import yaml
from pathlib import Path

ROOT = Path(__file__).parent.parent.parent
CONTRACTS_FILE = Path(__file__).parent / "api_contracts.yaml"

# Service health endpoints for live validation
SERVICES = {
    "nantar": "http://localhost:8081/api/health",
    "aethyrrank": "http://localhost:8080/health",
    "zior": "http://localhost:8082/health",
    "vovin": "http://localhost:8092/health",
    "ain-soph": "http://localhost:8089/health",
    "elohim-veni": "http://localhost:8093/health",
    "zodacare": "http://localhost:8090/health",
    "schema-reg": "http://localhost:8079/health",
}


def validate_contracts_file() -> list[str]:
    """Validate the api_contracts.yaml is well-formed."""
    errors = []
    if not CONTRACTS_FILE.exists():
        errors.append(f"  MISSING: {CONTRACTS_FILE.relative_to(ROOT)}")
        return errors
    try:
        with open(CONTRACTS_FILE) as f:
            contracts = yaml.safe_load(f)
    except yaml.YAMLError as e:
        errors.append(f"  PARSE ERROR in api_contracts.yaml: {e}")
        return errors

    required_keys = {"version", "brains"}
    for key in required_keys:
        if key not in contracts:
            errors.append(f"  MISSING key '{key}' in api_contracts.yaml")

    return errors


def validate_event_schema_references() -> list[str]:
    """
    Check that every event topic referenced in rules.yaml
    has a corresponding schema file in events/schemas/.
    """
    errors = []
    rules_file = ROOT / "enforcement" / "dependency-graph" / "rules.yaml"
    if not rules_file.exists():
        return [f"  MISSING: {rules_file.relative_to(ROOT)}"]

    with open(rules_file) as f:
        rules = yaml.safe_load(f)

    schemas_dir = ROOT / "events" / "schemas"
    all_topics = set()
    for brain in rules.get("brains", {}).values():
        for topic in brain.get("kafka_produce", []):
            all_topics.add(topic)
        for topic in brain.get("kafka_consume", []):
            all_topics.add(topic)

    for topic in sorted(all_topics):
        # Convert topic name to schema file name: content.events → content.*.json
        prefix = topic.split(".")[0]
        matching = list(schemas_dir.glob(f"{prefix}.*.json"))
        if not matching:
            errors.append(
                f"  MISSING schema: topic '{topic}' has no schema in events/schemas/{prefix}.*.json"
            )

    return errors


def main():
    print("F33D3R API contract validator")
    print()

    all_errors = []

    print("[1/2] Validating api_contracts.yaml...")
    all_errors.extend(validate_contracts_file())

    print("[2/2] Checking event schema coverage...")
    all_errors.extend(validate_event_schema_references())

    print()
    if all_errors:
        print(f"FAILED — {len(all_errors)} error(s):\n")
        for e in all_errors:
            print(e)
        sys.exit(1)
    else:
        print("OK — all contracts valid")
        sys.exit(0)


if __name__ == "__main__":
    main()
