//! Canonical brain identifiers for the entire F33D3R system.
//!
//! Every brain region registers with one of these IDs.
//! Every BrainSignal carries one of these IDs as its source.
//! Adding a new brain = add it here + add its port + update .gitmodules.

use serde::{Deserialize, Serialize};
use std::fmt;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum BrainId {
    /// Core ranking engine — always online
    AethyrRank,
    /// Schema registry — always online
    SchemaRegistry,
    /// Music cortex — Zior
    Zior,
    /// Feed brain — Nantar
    Nantar,
    /// Shop brain — Thessalon
    Thessalon,
    /// Messaging brain — Vovin
    Vovin,
    /// Streaming brain — Caeor
    Caeor,
    /// Voice Rooms brain — Loxion
    Loxion,
    /// Video brain — Astraon
    Astraon,
    /// Wallet brain — Ain Soph
    AinSoph,
    /// Git/Code brain — Zodacare
    Zodacare,
    /// Security/SOC brain — Elohim Veni
    ElohimVeni,
}

impl BrainId {
    /// Canonical service name used in docker-compose, logs, and registry.
    pub fn service_name(&self) -> &'static str {
        match self {
            BrainId::AethyrRank => "aethyrrank-engine",
            BrainId::SchemaRegistry => "aethyr-schema-registry",
            BrainId::Zior => "zior-engine",
            BrainId::Nantar => "nantar-engine",
            BrainId::Thessalon => "thessalon-engine",
            BrainId::Vovin => "vovin-engine",
            BrainId::Caeor => "caeor-engine",
            BrainId::Loxion => "loxion-engine",
            BrainId::Astraon => "astraon-engine",
            BrainId::AinSoph => "ain-soph-engine",
            BrainId::Zodacare => "zodacare-engine",
            BrainId::ElohimVeni => "elohim-veni-engine",
        }
    }

    /// Default port for each brain (matches docker-compose and nginx).
    pub fn default_port(&self) -> u16 {
        match self {
            BrainId::SchemaRegistry => 8079,
            BrainId::AethyrRank => 8080,
            BrainId::Zior => 8082,
            BrainId::Nantar => 8083,
            BrainId::Thessalon => 8084,
            BrainId::Vovin => 8085,
            BrainId::Caeor => 8086,
            BrainId::Loxion => 8087,
            BrainId::Astraon => 8088,
            BrainId::AinSoph => 8089,
            BrainId::Zodacare => 8090,
            BrainId::ElohimVeni => 8091,
        }
    }

    /// Human-readable description of what this brain does.
    pub fn description(&self) -> &'static str {
        match self {
            BrainId::AethyrRank => "Core ranking engine — the prefrontal cortex",
            BrainId::SchemaRegistry => "Schema versioning for all brain signals",
            BrainId::Zior => "Music cortex — audio intelligence",
            BrainId::Nantar => "Feed brain — social posts and timeline",
            BrainId::Thessalon => "Shop brain — commerce and creator stores",
            BrainId::Vovin => "Messaging brain — private encrypted comms",
            BrainId::Caeor => "Streaming brain — live video",
            BrainId::Loxion => "Voice Rooms brain — audio spaces",
            BrainId::Astraon => "Video brain — short and long form video",
            BrainId::AinSoph => "Wallet brain — Uphold crypto and fiat",
            BrainId::Zodacare => "Git/Code brain — repository hosting",
            BrainId::ElohimVeni => "Security brain — SOC and cyber ops",
        }
    }
}

impl fmt::Display for BrainId {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}", self.service_name())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn schema_registry_has_lowest_port() {
        assert_eq!(BrainId::SchemaRegistry.default_port(), 8079);
    }

    #[test]
    fn aethyrrank_is_8080() {
        assert_eq!(BrainId::AethyrRank.default_port(), 8080);
    }

    #[test]
    fn all_ports_are_unique() {
        let ids = [
            BrainId::SchemaRegistry,
            BrainId::AethyrRank,
            BrainId::Zior,
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
        let mut ports: Vec<u16> = ids.iter().map(|b| b.default_port()).collect();
        ports.sort_unstable();
        ports.dedup();
        assert_eq!(ports.len(), ids.len(), "duplicate port detected");
    }

    #[test]
    fn serialises_to_snake_case() {
        let json = serde_json::to_string(&BrainId::AinSoph).unwrap();
        assert_eq!(json, r#""ain_soph""#);
    }
}
