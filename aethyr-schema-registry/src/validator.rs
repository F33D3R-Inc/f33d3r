//! Schema compatibility validator.
//!
//! ## Compatibility rules (strict mode)
//!
//! ALLOWED (backward compatible):
//!   ✅ Adding an optional field (no `required` entry + has default)
//!   ✅ Adding a field with a default value
//!   ✅ Widening a type (e.g. int → number)
//!   ✅ Adding new enum values
//!
//! REJECTED (breaking change):
//!   ❌ Removing a field that existed in the previous version
//!   ❌ Changing a field's type to an incompatible type
//!   ❌ Adding a required field without a default
//!   ❌ Removing an enum value
//!   ❌ Making an optional field required
//!
//! This ensures consumers can continue processing signals after a producer
//! updates its schema. The discipline is the same as Confluent Schema Registry.

use serde_json::Value;

#[derive(Debug, Clone)]
pub struct CompatibilityResult {
    pub is_compatible: bool,
    pub violations: Vec<String>,
}

impl CompatibilityResult {
    pub fn compatible() -> Self {
        Self {
            is_compatible: true,
            violations: vec![],
        }
    }

    pub fn incompatible(violations: Vec<String>) -> Self {
        Self {
            is_compatible: false,
            violations,
        }
    }
}

/// Check whether `new_schema` is backward-compatible with `old_schema`.
/// Returns a `CompatibilityResult` with all violations listed.
pub fn check_compatibility(old_schema: &Value, new_schema: &Value) -> CompatibilityResult {
    let mut violations = Vec::new();

    let old_props = get_properties(old_schema);
    let new_props = get_properties(new_schema);
    let old_required = get_required(old_schema);
    let new_required = get_required(new_schema);

    // ── Rule 1: No field removals ─────────────────────────────────────────
    for (field, _) in &old_props {
        if !new_props.contains_key(field.as_str()) {
            violations.push(format!(
                "FIELD_REMOVED: field '{}' existed in v{} but is missing in new schema",
                field, "prev"
            ));
        }
    }

    // ── Rule 2: No type changes to incompatible types ─────────────────────
    for (field, old_def) in &old_props {
        if let Some(new_def) = new_props.get(field.as_str()) {
            let old_type = type_of(old_def);
            let new_type = type_of(new_def);
            if !is_compatible_type_change(&old_type, &new_type) {
                violations.push(format!(
                    "TYPE_CHANGED: field '{}' type changed from '{}' to '{}' (incompatible)",
                    field, old_type, new_type
                ));
            }
        }
    }

    // ── Rule 3: No making optional fields required ────────────────────────
    for field in &new_required {
        if !old_required.contains(field) {
            if old_props.contains_key(field.as_str()) {
                // Field existed but was optional — now required: breaking
                violations.push(format!(
                    "FIELD_NOW_REQUIRED: field '{}' was optional but is now required",
                    field
                ));
            }
            // New required field that didn't exist before — also breaking
            // unless it has a default
            if !old_props.contains_key(field.as_str()) {
                let has_default = new_props
                    .get(field.as_str())
                    .and_then(|d| d.get("default"))
                    .is_some();
                if !has_default {
                    violations.push(format!(
                        "NEW_REQUIRED_FIELD: field '{}' is required in new schema but has no default",
                        field
                    ));
                }
            }
        }
    }

    // ── Rule 4: No enum value removals ────────────────────────────────────
    for (field, old_def) in &old_props {
        if let Some(old_enum) = old_def.get("enum") {
            if let Some(new_def) = new_props.get(field.as_str()) {
                if let Some(new_enum) = new_def.get("enum") {
                    let old_vals: Vec<&Value> = old_enum
                        .as_array()
                        .map(|a| a.iter().collect())
                        .unwrap_or_default();
                    let new_vals: Vec<&Value> = new_enum
                        .as_array()
                        .map(|a| a.iter().collect())
                        .unwrap_or_default();
                    for val in &old_vals {
                        if !new_vals.contains(val) {
                            violations.push(format!(
                                "ENUM_VALUE_REMOVED: enum value {:?} removed from field '{}'",
                                val, field
                            ));
                        }
                    }
                }
            }
        }
    }

    if violations.is_empty() {
        CompatibilityResult::compatible()
    } else {
        CompatibilityResult::incompatible(violations)
    }
}

/// Validate a payload against a JSON Schema using the jsonschema crate.
pub fn validate_payload(schema: &Value, payload: &Value) -> CompatibilityResult {
    match jsonschema::JSONSchema::compile(schema) {
        Err(e) => CompatibilityResult::incompatible(vec![format!("schema compile error: {}", e)]),
        Ok(compiled) => match compiled.validate(payload) {
            Ok(()) => CompatibilityResult::compatible(),
            Err(errors) => {
                CompatibilityResult::incompatible(errors.map(|e| e.to_string()).collect())
            }
        },
    }
}

// ── Helpers ───────────────────────────────────────────────────────────────────

fn get_properties(schema: &Value) -> std::collections::HashMap<String, Value> {
    schema
        .get("properties")
        .and_then(|p| p.as_object())
        .map(|o| o.iter().map(|(k, v)| (k.clone(), v.clone())).collect())
        .unwrap_or_default()
}

fn get_required(schema: &Value) -> Vec<String> {
    schema
        .get("required")
        .and_then(|r| r.as_array())
        .map(|a| {
            a.iter()
                .filter_map(|v| v.as_str().map(String::from))
                .collect()
        })
        .unwrap_or_default()
}

fn type_of(def: &Value) -> String {
    def.get("type")
        .and_then(|t| t.as_str())
        .unwrap_or("unknown")
        .to_string()
}

fn is_compatible_type_change(old: &str, new: &str) -> bool {
    if old == new {
        return true;
    }
    // Widening: integer → number is allowed
    if old == "integer" && new == "number" {
        return true;
    }
    // Everything else is incompatible
    false
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn schema(props: serde_json::Value, required: Vec<&str>) -> Value {
        json!({
            "type": "object",
            "properties": props,
            "required": required
        })
    }

    #[test]
    fn identical_schemas_are_compatible() {
        let s = schema(json!({"id": {"type": "string"}}), vec!["id"]);
        let r = check_compatibility(&s, &s);
        assert!(r.is_compatible, "{:?}", r.violations);
    }

    #[test]
    fn adding_optional_field_is_compatible() {
        let old = schema(json!({"id": {"type": "string"}}), vec!["id"]);
        let new = schema(
            json!({"id": {"type": "string"}, "tag": {"type": "string"}}),
            vec!["id"],
        );
        let r = check_compatibility(&old, &new);
        assert!(r.is_compatible, "{:?}", r.violations);
    }

    #[test]
    fn removing_field_is_incompatible() {
        let old = schema(
            json!({"id": {"type": "string"}, "tag": {"type": "string"}}),
            vec!["id"],
        );
        let new = schema(json!({"id": {"type": "string"}}), vec!["id"]);
        let r = check_compatibility(&old, &new);
        assert!(!r.is_compatible);
        assert!(r.violations.iter().any(|v| v.contains("FIELD_REMOVED")));
    }

    #[test]
    fn type_change_is_incompatible() {
        let old = schema(json!({"count": {"type": "integer"}}), vec![]);
        let new = schema(json!({"count": {"type": "string"}}), vec![]);
        let r = check_compatibility(&old, &new);
        assert!(!r.is_compatible);
        assert!(r.violations.iter().any(|v| v.contains("TYPE_CHANGED")));
    }

    #[test]
    fn widening_integer_to_number_is_compatible() {
        let old = schema(json!({"val": {"type": "integer"}}), vec![]);
        let new = schema(json!({"val": {"type": "number"}}), vec![]);
        let r = check_compatibility(&old, &new);
        assert!(r.is_compatible, "{:?}", r.violations);
    }

    #[test]
    fn adding_required_field_without_default_is_incompatible() {
        let old = schema(json!({"id": {"type": "string"}}), vec!["id"]);
        let new = schema(
            json!({"id": {"type": "string"}, "name": {"type": "string"}}),
            vec!["id", "name"],
        );
        let r = check_compatibility(&old, &new);
        assert!(!r.is_compatible);
        assert!(r
            .violations
            .iter()
            .any(|v| v.contains("NEW_REQUIRED_FIELD")));
    }

    #[test]
    fn adding_required_field_with_default_is_compatible() {
        let old = schema(json!({"id": {"type": "string"}}), vec!["id"]);
        let new = schema(
            json!({"id": {"type": "string"}, "name": {"type": "string", "default": ""}}),
            vec!["id", "name"],
        );
        let r = check_compatibility(&old, &new);
        assert!(r.is_compatible, "{:?}", r.violations);
    }

    #[test]
    fn removing_enum_value_is_incompatible() {
        let old = schema(
            json!({"mood": {"type": "string", "enum": ["dark", "bright", "chill"]}}),
            vec![],
        );
        let new = schema(
            json!({"mood": {"type": "string", "enum": ["dark", "bright"]}}),
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
    fn adding_enum_value_is_compatible() {
        let old = schema(
            json!({"mood": {"type": "string", "enum": ["dark", "bright"]}}),
            vec![],
        );
        let new = schema(
            json!({"mood": {"type": "string", "enum": ["dark", "bright", "chill"]}}),
            vec![],
        );
        let r = check_compatibility(&old, &new);
        assert!(r.is_compatible, "{:?}", r.violations);
    }
}
