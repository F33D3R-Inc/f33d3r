/// Elohim Veni — Deterministic PIAL Decision Engine
///
/// All decisions are pure functions of the input state. No randomness,
/// no ML inference in v1. Every outcome is reproducible from the audit log.
///
/// Decision ladder (highest precedence first):
///   1. PIAL status is not ACTIVE → DENY (immediate)
///   2. PIAL is SUSPENDED         → DENY
///   3. Explicit capability revoke for this action → DENY
///   4. Anomaly score > 0.85 (combined)   → QUARANTINE
///   5. Content risk > 0.80               → QUARANTINE
///   6. Trust score < 0.20                → DENY
///   7. Action requires capability, not granted → DENY
///   8. Content risk 0.50–0.80            → ALLOW_RESTRICTED
///   9. Anomaly score 0.50–0.85           → ALLOW_RESTRICTED
///  10. Otherwise                         → ALLOW

use chrono::Utc;
use uuid::Uuid;

// Capabilities required per action (for non-BASIC tiers the bar is lower).
const GATED_ACTIONS: &[(&str, &str)] = &[
    ("monetize",      "monetize"),
    ("adult_content", "adult_content"),
    ("node_relay",    "node_relay"),
];

pub struct EvalInput<'a> {
    pub pial_id:       Uuid,
    pub action:        &'a str,
    pub pial_status:   &'a str,
    pub tier:          &'a str,
    pub cap_grant:     Option<(bool, Option<chrono::DateTime<Utc>>)>,
    pub trust_score:   f64,
    pub stored_anomaly: f64,
    pub violations:    i32,
    pub content_risk:  f64,
    pub anomaly_score: f64,
}

pub struct EvalOutput {
    pub decision:            String,
    pub confidence:          f64,
    pub reason:              String,
    pub capability_required: Option<String>,
}

pub fn evaluate(input: EvalInput<'_>) -> EvalOutput {
    let _ = input.pial_id; // referenced for future audit use

    // Rule 1 & 2: PIAL status gate.
    if input.pial_status != "ACTIVE" {
        return EvalOutput {
            decision:            "DENY".into(),
            confidence:          1.0,
            reason:              format!("PIAL status is {}", input.pial_status),
            capability_required: None,
        };
    }

    // Rule 3: explicit capability revoke.
    if let Some((granted, expires_at)) = &input.cap_grant {
        let expired = expires_at
            .map(|exp| exp < Utc::now())
            .unwrap_or(false);
        if !granted || expired {
            return EvalOutput {
                decision:            "DENY".into(),
                confidence:          1.0,
                reason:              format!("capability '{}' explicitly revoked or expired", input.action),
                capability_required: Some(input.action.to_string()),
            };
        }
    }

    // Combined anomaly: blend incoming signal with stored history.
    let combined_anomaly = (input.stored_anomaly * 0.4) + (input.anomaly_score * 0.6);

    // Rule 4: high anomaly → quarantine.
    if combined_anomaly > 0.85 {
        return EvalOutput {
            decision:            "QUARANTINE".into(),
            confidence:          0.90,
            reason:              format!(
                "combined anomaly score {:.2} exceeds quarantine threshold (0.85)",
                combined_anomaly
            ),
            capability_required: None,
        };
    }

    // Rule 5: high content risk → quarantine.
    if input.content_risk > 0.80 {
        return EvalOutput {
            decision:            "QUARANTINE".into(),
            confidence:          0.92,
            reason:              format!(
                "content risk {:.2} exceeds quarantine threshold (0.80)",
                input.content_risk
            ),
            capability_required: None,
        };
    }

    // Rule 6: critically low trust.
    if input.trust_score < 0.20 {
        return EvalOutput {
            decision:            "DENY".into(),
            confidence:          0.95,
            reason:              format!(
                "trust score {:.2} below minimum threshold (0.20); {} violations on record",
                input.trust_score, input.violations
            ),
            capability_required: None,
        };
    }

    // Rule 7: gated capability check.
    for (gated_action, cap_name) in GATED_ACTIONS {
        if input.action == *gated_action {
            // PRIVILEGED tier gets implicit access; all others need an explicit grant.
            let has_access = input.tier == "PRIVILEGED"
                || matches!(&input.cap_grant, Some((true, exp)) if
                    exp.map(|e| e > Utc::now()).unwrap_or(true));

            if !has_access {
                return EvalOutput {
                    decision:            "DENY".into(),
                    confidence:          1.0,
                    reason:              format!(
                        "action '{}' requires capability '{}' which has not been granted",
                        input.action, cap_name
                    ),
                    capability_required: Some(cap_name.to_string()),
                };
            }
        }
    }

    // Rule 8: medium content risk → restrict.
    if input.content_risk > 0.50 {
        return EvalOutput {
            decision:            "ALLOW_RESTRICTED".into(),
            confidence:          0.85,
            reason:              format!(
                "content risk {:.2} requires restricted mode (threshold 0.50)",
                input.content_risk
            ),
            capability_required: None,
        };
    }

    // Rule 9: elevated anomaly → restrict.
    if combined_anomaly > 0.50 {
        return EvalOutput {
            decision:            "ALLOW_RESTRICTED".into(),
            confidence:          0.80,
            reason:              format!(
                "combined anomaly {:.2} requires restricted mode (threshold 0.50)",
                combined_anomaly
            ),
            capability_required: None,
        };
    }

    // Rule 10: ALLOW — compute confidence from trust + inverse risk.
    let confidence = (input.trust_score * (1.0 - input.content_risk)).clamp(0.50, 1.0);

    EvalOutput {
        decision:            "ALLOW".into(),
        confidence,
        reason:              "all checks passed".into(),
        capability_required: None,
    }
}
