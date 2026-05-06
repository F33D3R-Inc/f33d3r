//! BrainRegistry — tracks online/offline status for all 11 brain regions.
//!
//! Rules:
//!   - AethyrRank never panics if a brain is offline
//!   - Missing heartbeat >60s → Offline
//!   - Signals from offline brains are not processed (logged and dropped)
//!   - Recovery: brain re-registers → status returns to Online
//!   - Stub brains (status=Planned) are always present but never signal

use chrono::{DateTime, Duration, Utc};
use dashmap::DashMap;
use serde::{Deserialize, Serialize};
use std::sync::Arc;

use crate::contracts::brain_id::BrainId;
use crate::contracts::signal::{BrainRegistration, BrainStatus};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BrainEntry {
    pub registration: BrainRegistration,
    pub status: BrainStatus,
    pub last_seen: DateTime<Utc>,
    pub signal_count: u64,
}

pub struct BrainRegistry {
    brains: DashMap<BrainId, BrainEntry>,
}

impl BrainRegistry {
    pub fn new() -> Arc<Self> {
        let registry = Arc::new(Self {
            brains: DashMap::new(),
        });

        // Pre-populate all stub brains as Planned so they appear in status checks
        let planned = [
            BrainId::Nantar,
            BrainId::Thessalon,
            BrainId::Vovin,
            BrainId::Caeor,
            BrainId::Loxion,
            BrainId::Astraon,
            BrainId::AinSoph,
            BrainId::Zodacare,
            BrainId::ElohimVeni,
        ];
        for brain_id in planned {
            registry.brains.insert(
                brain_id,
                BrainEntry {
                    registration: BrainRegistration {
                        brain_id,
                        schema_version: 0,
                        port: brain_id.default_port(),
                        host: "localhost".to_string(),
                        signal_types: vec![],
                        description: brain_id.description().to_string(),
                        registered_at: Utc::now(),
                    },
                    status: BrainStatus::Planned,
                    last_seen: Utc::now(),
                    signal_count: 0,
                },
            );
        }

        registry
    }

    /// Register or update a brain. Called when a brain calls POST /brain/register.
    pub fn register(&self, reg: BrainRegistration) {
        let brain_id = reg.brain_id;
        self.brains.insert(
            brain_id,
            BrainEntry {
                registration: reg,
                status: BrainStatus::Online,
                last_seen: Utc::now(),
                signal_count: 0,
            },
        );
        tracing::info!(brain = %brain_id, "brain registered");
    }

    /// Record a heartbeat. Called every 30s by each brain.
    pub fn heartbeat(&self, brain_id: BrainId) {
        if let Some(mut entry) = self.brains.get_mut(&brain_id) {
            entry.last_seen = Utc::now();
            if entry.status != BrainStatus::Online {
                entry.status = BrainStatus::Online;
                tracing::info!(brain = %brain_id, "brain recovered → Online");
            }
        }
    }

    /// Record a signal receipt. Returns false if brain is not Online (signal dropped).
    pub fn record_signal(&self, brain_id: BrainId) -> bool {
        if let Some(mut entry) = self.brains.get_mut(&brain_id) {
            match entry.status {
                BrainStatus::Online => {
                    entry.signal_count += 1;
                    entry.last_seen = Utc::now();
                    return true;
                }
                BrainStatus::Planned => {
                    tracing::warn!(brain = %brain_id, "signal from planned brain — ignoring");
                    return false;
                }
                _ => {
                    tracing::warn!(brain = %brain_id, status = ?entry.status,
                        "signal from non-online brain — dropping");
                    return false;
                }
            }
        }
        tracing::warn!(brain = %brain_id, "signal from unknown brain — ignoring");
        false
    }

    /// Run the staleness checker. Call this on a background timer every 30s.
    /// Brains that miss heartbeats transition: Online → Degraded → Offline.
    pub fn check_staleness(&self) {
        let now = Utc::now();
        let degraded_after = Duration::seconds(45);
        let offline_after = Duration::seconds(90);

        for mut entry in self.brains.iter_mut() {
            if entry.status == BrainStatus::Planned {
                continue;
            }

            let age = now - entry.last_seen;
            let new_status = if age > offline_after {
                BrainStatus::Offline
            } else if age > degraded_after {
                BrainStatus::Degraded
            } else {
                continue;
            };

            if new_status != entry.status {
                tracing::warn!(
                    brain  = %entry.registration.brain_id,
                    status = ?new_status,
                    age_s  = age.num_seconds(),
                    "brain status changed"
                );
                entry.status = new_status;
            }
        }
    }

    /// Returns true if the given brain is online and ready to receive work.
    pub fn is_online(&self, brain_id: BrainId) -> bool {
        self.brains
            .get(&brain_id)
            .map(|e| e.status == BrainStatus::Online)
            .unwrap_or(false)
    }

    /// Snapshot of all brain statuses — used by GET /brain/status.
    pub fn status_snapshot(&self) -> Vec<BrainStatusEntry> {
        let mut entries: Vec<BrainStatusEntry> = self
            .brains
            .iter()
            .map(|e| BrainStatusEntry {
                brain_id: e.registration.brain_id,
                status: e.status.clone(),
                port: e.registration.port,
                last_seen: e.last_seen,
                signal_count: e.signal_count,
                description: e.registration.description.clone(),
            })
            .collect();
        entries.sort_by_key(|e| e.port);
        entries
    }
}

impl Default for BrainRegistry {
    fn default() -> Self {
        Self {
            brains: DashMap::new(),
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BrainStatusEntry {
    pub brain_id: BrainId,
    pub status: BrainStatus,
    pub port: u16,
    pub last_seen: DateTime<Utc>,
    pub signal_count: u64,
    pub description: String,
}

#[cfg(test)]
mod tests {
    use super::*;

    fn mock_reg(brain_id: BrainId) -> BrainRegistration {
        BrainRegistration {
            brain_id,
            schema_version: 1,
            port: brain_id.default_port(),
            host: "localhost".to_string(),
            signal_types: vec![],
            description: "test".to_string(),
            registered_at: Utc::now(),
        }
    }

    #[test]
    fn register_sets_online() {
        let r = BrainRegistry::new();
        r.register(mock_reg(BrainId::Zior));
        assert!(r.is_online(BrainId::Zior));
    }

    #[test]
    fn planned_brains_are_prepopulated() {
        let r = BrainRegistry::new();
        let snap = r.status_snapshot();
        let nantar = snap.iter().find(|e| e.brain_id == BrainId::Nantar);
        assert!(
            nantar.is_some(),
            "Nantar should be pre-populated as Planned"
        );
        assert_eq!(nantar.unwrap().status, BrainStatus::Planned);
    }

    #[test]
    fn signal_from_unregistered_brain_returns_false() {
        let r = BrainRegistry::new();
        // Zior not registered — signal should be dropped
        assert!(!r.record_signal(BrainId::Zior));
    }

    #[test]
    fn signal_from_registered_brain_returns_true() {
        let r = BrainRegistry::new();
        r.register(mock_reg(BrainId::Zior));
        assert!(r.record_signal(BrainId::Zior));
    }
}
