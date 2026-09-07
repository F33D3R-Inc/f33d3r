use anyhow::Result;
use dashmap::DashMap;
use nalgebra::{DMatrix, DVector};

use crate::bandit::linucb::LinUcbModel;

/// Thread-safe store of per-surface LinUCB models.
///
/// New surfaces are auto-initialised on first access.
/// The `DashMap` provides lock-per-shard concurrency — safe for the
/// read-heavy, write-light workload typical of ranking + feedback.
pub struct ModelStore {
    models: DashMap<String, LinUcbModel>,
    dim: usize,
}

impl ModelStore {
    /// Create a new store, pre-populating known surfaces.
    pub fn new(surfaces: &[&str], dim: usize) -> Self {
        let map = DashMap::new();
        for s in surfaces {
            map.insert(s.to_string(), LinUcbModel::new(s, dim));
        }
        Self { models: map, dim }
    }

    /// Score a feature vector against the model for `surface`.
    /// Auto-creates the model if the surface is new.
    pub fn score(&self, surface: &str, x: &DVector<f64>, alpha: f64) -> Result<(f64, f64, f64)> {
        let model = self
            .models
            .entry(surface.to_string())
            .or_insert_with(|| LinUcbModel::new(surface, self.dim));
        model.score(x, alpha)
    }

    /// Apply an online update for `surface` given feature vector `x` and reward `r`.
    pub fn update(&self, surface: &str, x: &DVector<f64>, reward: f64) {
        let mut model = self
            .models
            .entry(surface.to_string())
            .or_insert_with(|| LinUcbModel::new(surface, self.dim));
        model.update(x, reward);
    }

    /// List all surfaces currently tracked.
    pub fn surfaces(&self) -> Vec<String> {
        self.models.iter().map(|e| e.key().clone()).collect()
    }

    /// Overwrite a surface's model with a restored checkpoint from Postgres.
    pub fn load_checkpoint(&self, surface: &str, a_matrix: DMatrix<f64>, b_vector: DVector<f64>) {
        let mut model = self
            .models
            .entry(surface.to_string())
            .or_insert_with(|| LinUcbModel::new(surface, self.dim));
        model.a_matrix = a_matrix;
        model.b_vector = b_vector;
    }

    /// Return a snapshot of every surface's matrices for persistence.
    /// Clones are cheap relative to the I/O cost of writing to Postgres.
    pub fn snapshot_all(&self) -> Vec<(String, DMatrix<f64>, DVector<f64>)> {
        self.models
            .iter()
            .map(|e| (e.key().clone(), e.a_matrix.clone(), e.b_vector.clone()))
            .collect()
    }
}
