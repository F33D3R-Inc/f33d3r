use dashmap::DashMap;
use nalgebra::{DMatrix, DVector};

use crate::bandit::linucb::LinUcbModel;

/// Every learned LinUCB model, keyed `"{viewer_or_nexus_shard}:{surface}"`.
///
/// One writer: the ranking.events consumer (learning::consumer), which builds
/// each batch's changes on clones and installs them here only after the batch
/// has committed to f33d3r_rank. /rank only reads. A model that does not exist
/// has learned nothing, and reading never creates one.
pub struct ModelStore {
    models: DashMap<String, LinUcbModel>,
    dim: usize,
}

impl ModelStore {
    pub fn new(dim: usize) -> Self {
        Self {
            models: DashMap::new(),
            dim,
        }
    }

    pub fn dim(&self) -> usize {
        self.dim
    }

    /// θᵀx for this model, or None when the model has never been trained.
    pub fn expected(&self, model_key: &str, x: &DVector<f64>) -> Option<anyhow::Result<f64>> {
        self.models.get(model_key).map(|m| m.expected(x))
    }

    /// A copy of the model to change, or a fresh one when it does not exist yet.
    pub fn working_copy(&self, model_key: &str) -> LinUcbModel {
        match self.models.get(model_key) {
            Some(m) => LinUcbModel {
                surface: m.surface.clone(),
                dim: m.dim,
                a_matrix: m.a_matrix.clone(),
                b_vector: m.b_vector.clone(),
            },
            None => LinUcbModel::new(model_key, self.dim),
        }
    }

    /// Install a model whose state is already durable in f33d3r_rank.
    pub fn install(&self, model_key: &str, a_matrix: DMatrix<f64>, b_vector: DVector<f64>) {
        let dim = b_vector.len();
        self.models.insert(
            model_key.to_string(),
            LinUcbModel {
                surface: model_key.to_string(),
                dim,
                a_matrix,
                b_vector,
            },
        );
    }

    pub fn len(&self) -> usize {
        self.models.len()
    }
}
