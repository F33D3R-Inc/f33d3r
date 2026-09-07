// tests/session_tests.rs
use aethyrrank_engine::session::cache::SessionFeatureCache;

#[test]
fn store_and_retrieve_feature_vector() {
    let cache = SessionFeatureCache::new();
    let fv = vec![0.3, 0.6, 0.1, 0.8, 0.5, 0.4, 0.9];
    cache.put("session_1", "content_42", "feed", fv.clone());
    let entry = cache
        .get("session_1", "content_42")
        .expect("entry should exist");
    assert_eq!(entry.feature_vector, fv);
    assert_eq!(entry.surface, "feed");
}

#[test]
fn missing_entry_returns_none() {
    let cache = SessionFeatureCache::new();
    assert!(cache.get("ghost_session", "ghost_item").is_none());
}

#[test]
fn overwrite_updates_entry() {
    let cache = SessionFeatureCache::new();
    cache.put("s1", "c1", "feed", vec![0.1; 7]);
    cache.put("s1", "c1", "explore", vec![0.9; 7]);
    let entry = cache.get("s1", "c1").unwrap();
    assert_eq!(entry.feature_vector, vec![0.9; 7]);
    assert_eq!(entry.surface, "explore");
}

#[test]
fn cache_length_tracks_unique_keys() {
    let cache = SessionFeatureCache::new();
    cache.put("s1", "c1", "feed", vec![0.1; 7]);
    cache.put("s1", "c2", "feed", vec![0.2; 7]);
    cache.put("s2", "c1", "feed", vec![0.3; 7]);
    assert_eq!(cache.len(), 3);
}
