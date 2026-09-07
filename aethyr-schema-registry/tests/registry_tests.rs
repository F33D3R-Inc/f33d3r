//! Integration tests for the schema registry validator.
//! These tests run without a database (validator is pure logic).

use aethyr_schema_registry::validator::{check_compatibility, validate_payload};
use serde_json::json;

fn obj_schema(props: serde_json::Value, required: Vec<&str>) -> serde_json::Value {
    json!({
        "type":       "object",
        "properties": props,
        "required":   required,
    })
}

// ── Compatibility rules ───────────────────────────────────────────────────────

#[test]
fn adding_optional_field_is_compatible() {
    let old = obj_schema(json!({"track_id": {"type": "string"}}), vec!["track_id"]);
    let new = obj_schema(
        json!({"track_id": {"type": "string"}, "mood": {"type": "string"}}),
        vec!["track_id"],
    );
    let r = check_compatibility(&old, &new);
    assert!(
        r.is_compatible,
        "adding optional field must be compatible: {:?}",
        r.violations
    );
}

#[test]
fn removing_field_is_rejected() {
    let old = obj_schema(
        json!({"track_id": {"type":"string"}, "bpm": {"type":"number"}}),
        vec!["track_id"],
    );
    let new = obj_schema(json!({"track_id": {"type":"string"}}), vec!["track_id"]);
    let r = check_compatibility(&old, &new);
    assert!(!r.is_compatible, "removing field must be rejected");
    assert!(r.violations.iter().any(|v| v.contains("FIELD_REMOVED")));
}

#[test]
fn type_change_string_to_int_rejected() {
    let old = obj_schema(json!({"id": {"type":"string"}}), vec![]);
    let new = obj_schema(json!({"id": {"type":"integer"}}), vec![]);
    let r = check_compatibility(&old, &new);
    assert!(!r.is_compatible);
    assert!(r.violations.iter().any(|v| v.contains("TYPE_CHANGED")));
}

#[test]
fn integer_to_number_widening_is_compatible() {
    let old = obj_schema(json!({"count": {"type":"integer"}}), vec![]);
    let new = obj_schema(json!({"count": {"type":"number"}}), vec![]);
    let r = check_compatibility(&old, &new);
    assert!(
        r.is_compatible,
        "integer→number is a compatible widening: {:?}",
        r.violations
    );
}

#[test]
fn making_optional_field_required_rejected() {
    let old = obj_schema(
        json!({"id": {"type":"string"}, "tag": {"type":"string"}}),
        vec!["id"],
    );
    let new = obj_schema(
        json!({"id": {"type":"string"}, "tag": {"type":"string"}}),
        vec!["id", "tag"],
    );
    let r = check_compatibility(&old, &new);
    assert!(!r.is_compatible);
    assert!(r
        .violations
        .iter()
        .any(|v| v.contains("FIELD_NOW_REQUIRED")));
}

#[test]
fn new_required_field_with_default_is_compatible() {
    let old = obj_schema(json!({"id": {"type":"string"}}), vec!["id"]);
    let new = obj_schema(
        json!({"id": {"type":"string"}, "version": {"type":"integer","default":1}}),
        vec!["id", "version"],
    );
    let r = check_compatibility(&old, &new);
    assert!(
        r.is_compatible,
        "new required field with default must be compatible: {:?}",
        r.violations
    );
}

#[test]
fn removing_enum_value_rejected() {
    let old = obj_schema(
        json!({"status": {"type":"string","enum":["planned","online","offline"]}}),
        vec![],
    );
    let new = obj_schema(
        json!({"status": {"type":"string","enum":["planned","online"]}}),
        vec![],
    );
    let r = check_compatibility(&old, &new);
    assert!(!r.is_compatible);
    assert!(r
        .violations
        .iter()
        .any(|v| v.contains("ENUM_VALUE_REMOVED")));
}

#[test]
fn adding_enum_value_compatible() {
    let old = obj_schema(
        json!({"mood": {"type":"string","enum":["dark","bright"]}}),
        vec![],
    );
    let new = obj_schema(
        json!({"mood": {"type":"string","enum":["dark","bright","dreamy"]}}),
        vec![],
    );
    let r = check_compatibility(&old, &new);
    assert!(r.is_compatible, "{:?}", r.violations);
}

// ── Multiple violations reported ──────────────────────────────────────────────

#[test]
fn multiple_violations_all_reported() {
    let old = obj_schema(
        json!({"id":{"type":"string"},"score":{"type":"integer"},"tag":{"type":"string"}}),
        vec!["id"],
    );
    let new = obj_schema(
        json!({"id":{"type":"integer"},"score":{"type":"integer"}}), // removed tag, changed id type
        vec!["id"],
    );
    let r = check_compatibility(&old, &new);
    assert!(!r.is_compatible);
    assert!(
        r.violations.len() >= 2,
        "expected >= 2 violations, got: {:?}",
        r.violations
    );
}

// ── Payload validation ────────────────────────────────────────────────────────

#[test]
fn valid_payload_passes() {
    let schema = obj_schema(
        json!({"track_id":{"type":"string"},"bpm":{"type":"number"}}),
        vec!["track_id", "bpm"],
    );
    let payload = json!({"track_id":"t1","bpm":128.0});
    let r = validate_payload(&schema, &payload);
    assert!(
        r.is_compatible,
        "valid payload should pass: {:?}",
        r.violations
    );
}

#[test]
fn missing_required_field_fails_validation() {
    let schema = obj_schema(
        json!({"track_id":{"type":"string"},"bpm":{"type":"number"}}),
        vec!["track_id", "bpm"],
    );
    let payload = json!({"track_id":"t1"}); // missing bpm
    let r = validate_payload(&schema, &payload);
    assert!(
        !r.is_compatible,
        "missing required field should fail validation"
    );
}

// ── Zior audio_analysis signal compatibility ──────────────────────────────────

#[test]
fn zior_audio_analysis_v1_to_v2_compatible() {
    let v1 = json!({
        "type": "object",
        "properties": {
            "track_id":      {"type":"string"},
            "topic_vector":  {"type":"array","items":{"type":"number"}},
            "bpm":           {"type":"number"},
            "key":           {"type":"integer"},
            "mood":          {"type":"string"}
        },
        "required": ["track_id","topic_vector","bpm","key","mood"]
    });
    // v2 adds optional velocity_score
    let v2 = json!({
        "type": "object",
        "properties": {
            "track_id":      {"type":"string"},
            "topic_vector":  {"type":"array","items":{"type":"number"}},
            "bpm":           {"type":"number"},
            "key":           {"type":"integer"},
            "mood":          {"type":"string"},
            "velocity_score":{"type":"number"}   // new optional field
        },
        "required": ["track_id","topic_vector","bpm","key","mood"]
    });
    let r = check_compatibility(&v1, &v2);
    assert!(
        r.is_compatible,
        "adding optional velocity_score must be compatible: {:?}",
        r.violations
    );
}

#[test]
fn zior_removing_bpm_rejected() {
    let v1 = json!({
        "type": "object",
        "properties": {
            "track_id": {"type":"string"},
            "bpm":      {"type":"number"}
        },
        "required": ["track_id","bpm"]
    });
    let v2 = json!({
        "type": "object",
        "properties": {
            "track_id": {"type":"string"}
        },
        "required": ["track_id"]
    });
    let r = check_compatibility(&v1, &v2);
    assert!(!r.is_compatible, "removing bpm must be rejected");
}
