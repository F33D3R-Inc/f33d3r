/// NEXUS AML (Anti-Money Laundering) aggregation module.
///
/// Checks whether a withdrawal pushes the NEXUS aggregate above $10,000 (or
/// equivalent in AET) within any 30-day rolling window. If so, the transaction
/// is flagged for Enhanced Due Diligence (EDD) rather than blocked outright.
///
/// Uses Verity's POST /v1/nexus/aml/{shard} which atomically records the
/// withdrawal and returns the updated aggregate + flag in a single call.
/// Verity owns the nexus_aml_aggregates table — Ain Soph never cross-reads it.
use reqwest::Client;
use tracing::warn;

/// AML threshold in micro-AET (1 AET = 100 micro-AET).
/// $10,000 equivalent: adjust AET price factor as needed.
const NEXUS_AML_THRESHOLD_UNITS: i64 = 1_000_000;

/// Result of an AML aggregate check + record operation.
#[derive(Debug)]
pub struct AmlCheckResult {
    /// Running total for this nexus in the current 30-day window (after this withdrawal).
    pub period_total_units: i64,
    /// Whether this withdrawal pushes the aggregate above the EDD threshold.
    pub flag_for_edd: bool,
}

/// Records a withdrawal for the PIAL's NEXUS aggregate AND checks the AML threshold.
/// Returns `flag_for_edd: false` on network errors (fail-open) to avoid blocking
/// legitimate withdrawals when Verity is temporarily unreachable.
///
/// This is a POST — it mutates Verity's nexus_aml_aggregates table.
/// Call this only after the withdrawal has been approved and is being settled.
pub async fn record_and_check(
    http: &Client,
    verity_url: &str,
    internal_key: &str,
    pial_shard_id: &str,
    withdrawal_units: i64,
) -> AmlCheckResult {
    let url = format!("{}/v1/nexus/aml/{}", verity_url, pial_shard_id);
    let body = serde_json::json!({ "withdrawal_uaet": withdrawal_units });

    // Verity's AML aggregate is a compliance write — service-to-service only.
    // Ain Soph is the settling peer, so it carries the shared secret.
    let resp = match http
        .post(&url)
        .header("X-Internal-Key", internal_key)
        .json(&body)
        .send()
        .await
    {
        Ok(r) => r,
        Err(e) => {
            warn!(error = %e, pial_shard_id, "nexus_aml: verity unreachable; fail-open");
            return AmlCheckResult {
                period_total_units: 0,
                flag_for_edd: false,
            };
        }
    };

    if resp.status() == reqwest::StatusCode::NOT_FOUND {
        // PIAL not in any NEXUS — single-account user, no aggregate needed.
        return AmlCheckResult {
            period_total_units: 0,
            flag_for_edd: false,
        };
    }

    if !resp.status().is_success() {
        warn!(status = %resp.status(), pial_shard_id, "nexus_aml: unexpected status; fail-open");
        return AmlCheckResult {
            period_total_units: 0,
            flag_for_edd: false,
        };
    }

    let body: serde_json::Value = match resp.json().await {
        Ok(v) => v,
        Err(e) => {
            warn!(error = %e, "nexus_aml: response parse error; fail-open");
            return AmlCheckResult {
                period_total_units: 0,
                flag_for_edd: false,
            };
        }
    };

    let period_total = body["total_withdrawn_uaet"]
        .as_i64()
        .unwrap_or(withdrawal_units);
    let flag = body["flag_for_edd"]
        .as_bool()
        .unwrap_or(period_total >= NEXUS_AML_THRESHOLD_UNITS);

    AmlCheckResult {
        period_total_units: period_total,
        flag_for_edd: flag,
    }
}
