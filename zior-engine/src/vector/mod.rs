//! Vector store and cluster detection.
//!
//! Stores track PsychVectors and identifies micro-communities
//! (clusters of listeners and tracks with similar psychological signatures).
//!
//! ## Cluster-first thinking
//!
//! Music grows in micro-communities before reaching global audiences.
//! A Phoenix underground electronic artist will first find traction with
//! listeners who score high on Shadow + Agency axes — before any genre
//! label is applied. Zior detects this cluster formation early.
//!
//! ## Algorithm
//!
//! Uses cosine similarity for nearest-neighbor search (same metric AethyrRank
//! uses for user-content alignment). Cluster IDs are assigned by simple
//! greedy grouping: tracks with cosine similarity > 0.75 belong to the
//! same cluster.

use std::collections::HashMap;

use crate::jung::axes::PsychVector;

pub struct VectorStore {
    /// track_id → normalised PsychVector
    tracks: HashMap<String, [f32; 8]>,
}

impl VectorStore {
    pub fn new() -> Self {
        Self {
            tracks: HashMap::new(),
        }
    }

    pub fn upsert(&mut self, track_id: String, mut vec: PsychVector) {
        vec.normalise();
        self.tracks.insert(track_id, vec.0);
    }

    /// Find the k most similar tracks by cosine similarity.
    pub fn nearest(&self, query: &PsychVector, k: usize, exclude: &str) -> Vec<(String, f32)> {
        let mut sims: Vec<(String, f32)> = self
            .tracks
            .iter()
            .filter(|(id, _)| id.as_str() != exclude)
            .map(|(id, vec)| {
                let sim = cosine_sim(&query.0, vec);
                (id.clone(), sim)
            })
            .collect();

        sims.sort_by(|a, b| b.1.partial_cmp(&a.1).unwrap_or(std::cmp::Ordering::Equal));
        sims.truncate(k);
        sims
    }

    /// Assign cluster IDs using greedy cosine grouping.
    /// Returns map: track_id → cluster_id
    pub fn cluster(&self, similarity_threshold: f32) -> HashMap<String, u32> {
        let mut assignments: HashMap<String, u32> = HashMap::new();
        let mut next_cluster = 0_u32;

        let ids: Vec<&String> = self.tracks.keys().collect();

        for id in &ids {
            if assignments.contains_key(*id) {
                continue;
            }

            let cluster_id = next_cluster;
            next_cluster += 1;
            assignments.insert((*id).clone(), cluster_id);

            // Find all tracks similar enough to join this cluster
            let vec = &self.tracks[*id];
            for other_id in &ids {
                if assignments.contains_key(*other_id) {
                    continue;
                }
                let other_vec = &self.tracks[*other_id];
                if cosine_sim(vec, other_vec) >= similarity_threshold {
                    assignments.insert((*other_id).clone(), cluster_id);
                }
            }
        }

        assignments
    }

    pub fn len(&self) -> usize {
        self.tracks.len()
    }
}

fn cosine_sim(a: &[f32; 8], b: &[f32; 8]) -> f32 {
    let dot: f32 = a.iter().zip(b).map(|(x, y)| x * y).sum();
    let na: f32 = a.iter().map(|x| x * x).sum::<f32>().sqrt();
    let nb: f32 = b.iter().map(|x| x * x).sum::<f32>().sqrt();
    if na < 1e-8 || nb < 1e-8 {
        return 0.0;
    }
    (dot / (na * nb)).clamp(-1.0, 1.0)
}

impl Default for VectorStore {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn make_vec(vals: [f32; 8]) -> PsychVector {
        PsychVector(vals)
    }

    #[test]
    fn identical_vectors_have_similarity_one() {
        let v = [0.5_f32; 8];
        assert!((cosine_sim(&v, &v) - 1.0).abs() < 1e-5);
    }

    #[test]
    fn orthogonal_vectors_have_zero_similarity() {
        let a = [1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0_f32];
        let b = [0.0, 1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0_f32];
        assert!(cosine_sim(&a, &b).abs() < 1e-5);
    }

    #[test]
    fn nearest_returns_most_similar() {
        let mut store = VectorStore::new();
        store.upsert(
            "t1".into(),
            make_vec([1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0]),
        );
        store.upsert(
            "t2".into(),
            make_vec([0.9, 0.1, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0]),
        );
        store.upsert(
            "t3".into(),
            make_vec([0.0, 0.0, 1.0, 0.0, 0.0, 0.0, 0.0, 0.0]),
        );

        let query = make_vec([1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0]);
        let results = store.nearest(&query, 1, "");
        assert_eq!(results[0].0, "t1");
    }

    #[test]
    fn cluster_groups_similar_tracks() {
        let mut store = VectorStore::new();
        store.upsert(
            "dark1".into(),
            make_vec([0.1, 0.9, 0.8, 0.1, 0.1, 0.2, 0.7, 0.1]),
        );
        store.upsert(
            "dark2".into(),
            make_vec([0.1, 0.8, 0.9, 0.1, 0.1, 0.3, 0.8, 0.1]),
        );
        store.upsert(
            "chill".into(),
            make_vec([0.3, 0.1, 0.1, 0.9, 0.8, 0.1, 0.1, 0.7]),
        );

        let clusters = store.cluster(0.90);
        assert_eq!(
            clusters["dark1"], clusters["dark2"],
            "dark1 and dark2 should be in the same cluster"
        );
        assert_ne!(
            clusters["dark1"], clusters["chill"],
            "dark and chill tracks should be in different clusters"
        );
    }
}
