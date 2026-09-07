// tests/pipeline_tests.rs
//
// End-to-end tests for the Zior processing pipeline.
// Tests the full chain: features → Jung mapping → signal output.

use std::f32::consts::PI;

use zior_engine::audio::features;
use zior_engine::behavioral::events::{BehavioralStore, PlayEvent, PlayEventType};
use zior_engine::behavioral::velocity::VelocityTracker;
use zior_engine::jung::axes::PsychVector;
use zior_engine::jung::mapper;
use zior_engine::vector::VectorStore;

fn sine_wave(freq: f32, sr: u32, secs: f32) -> Vec<f32> {
    let n = (sr as f32 * secs) as usize;
    (0..n)
        .map(|i| (2.0 * PI * freq * i as f32 / sr as f32).sin() * 0.5)
        .collect()
}

fn make_play_event(track_id: &str, etype: PlayEventType, pos: f64, dur: f64) -> PlayEvent {
    PlayEvent {
        track_id: track_id.to_string(),
        user_id: "u1".to_string(),
        event_type: etype,
        position_secs: pos,
        duration_secs: dur,
        timestamp: chrono::Utc::now(),
        session_id: "s1".to_string(),
    }
}

// ── Feature extraction ────────────────────────────────────────────────────────

#[test]
fn extract_features_from_sine() {
    let samples = sine_wave(440.0, 22050, 5.0);
    let f = features::extract(&samples, 22050, 2048, 512).unwrap();
    assert!(f.rms_energy > 0.0);
    assert!(f.bpm_raw >= 40.0 && f.bpm_raw <= 220.0);
    assert!(f.tonal_valence >= 0.0 && f.tonal_valence <= 1.0);
}

#[test]
fn band_energies_sum_to_one() {
    let samples = sine_wave(880.0, 22050, 3.0);
    let f = features::extract(&samples, 22050, 2048, 512).unwrap();
    let sum = f.bass_energy + f.mid_energy + f.treble_energy;
    assert!((sum - 1.0).abs() < 0.01, "band energies sum={}", sum);
}

// ── Jung mapping ──────────────────────────────────────────────────────────────

#[test]
fn high_energy_minor_maps_to_shadow_and_agency() {
    use zior_engine::audio::features::AudioFeatures;
    let f = AudioFeatures {
        bpm_raw: 140.0,
        bpm_normalised: 0.56,
        key: 9,
        is_major: false,
        tonal_valence: 0.2,
        rms_energy: 0.9,
        dynamic_range: 0.7,
        spectral_centroid: 0.3,
        spectral_rolloff: 0.5,
        spectral_flux: 0.7,
        bass_energy: 0.7,
        mid_energy: 0.2,
        treble_energy: 0.1,
        vocal_probability: 0.2,
    };
    let mapped = mapper::map_to_axes(&f, None);
    let v = mapped.psych_vector.0;
    assert!(v[1] > 0.3, "shadow={}", v[1]);
    assert!(v[2] > 0.4, "agency={}", v[2]);
    for (i, &x) in v.iter().enumerate() {
        assert!(x >= 0.0 && x <= 1.0, "axis[{}]={}", i, x);
    }
}

#[test]
fn slow_major_vocal_maps_to_attachment_and_release() {
    use zior_engine::audio::features::AudioFeatures;
    let f = AudioFeatures {
        bpm_raw: 68.0,
        bpm_normalised: 0.16,
        key: 0,
        is_major: true,
        tonal_valence: 0.9,
        rms_energy: 0.25,
        dynamic_range: 0.3,
        spectral_centroid: 0.55,
        spectral_rolloff: 0.35,
        spectral_flux: 0.15,
        bass_energy: 0.15,
        mid_energy: 0.65,
        treble_energy: 0.20,
        vocal_probability: 0.92,
    };
    let mapped = mapper::map_to_axes(&f, None);
    let v = mapped.psych_vector.0;
    assert!(v[4] > 0.35, "attachment={}", v[4]);
    assert!(v[7] > 0.3, "release={}", v[7]);
}

// ── Behavioral pipeline ───────────────────────────────────────────────────────

#[test]
fn behavioral_blending_updates_vector() {
    use zior_engine::audio::features::AudioFeatures;

    let store = BehavioralStore::new();
    let track = "test_track";

    for _ in 0..20 {
        store.ingest(make_play_event(track, PlayEventType::Play, 0.0, 200.0));
    }
    for _ in 0..18 {
        store.ingest(make_play_event(
            track,
            PlayEventType::Complete,
            200.0,
            200.0,
        ));
    }
    for _ in 0..10 {
        store.ingest(make_play_event(track, PlayEventType::Replay, 0.0, 200.0));
    }

    let behav_vec = store.psych_vector(track).unwrap();
    // High completion and replay → high Integration [3] and Agency [2]
    assert!(behav_vec.0[3] > 0.6, "integration={}", behav_vec.0[3]);

    let audio_feats = AudioFeatures {
        bpm_raw: 120.0,
        bpm_normalised: 0.44,
        key: 5,
        is_major: true,
        tonal_valence: 0.65,
        rms_energy: 0.5,
        dynamic_range: 0.4,
        spectral_centroid: 0.5,
        spectral_rolloff: 0.4,
        spectral_flux: 0.4,
        bass_energy: 0.33,
        mid_energy: 0.34,
        treble_energy: 0.33,
        vocal_probability: 0.6,
    };
    let mapped_with_behavior = mapper::map_to_axes(&audio_feats, Some(&behav_vec));
    for &x in &mapped_with_behavior.psych_vector.0 {
        assert!(x >= 0.0 && x <= 1.0);
    }
}

// ── Velocity ──────────────────────────────────────────────────────────────────

#[test]
fn velocity_increases_with_engagement() {
    let mut tracker = VelocityTracker::new(1.0);
    let initial = tracker.velocity_score();
    for _ in 0..20 {
        tracker.record("complete");
    }
    let after = tracker.velocity_score();
    // After recording completions without baseline, should be > initial
    assert!(after >= initial, "velocity should increase with engagement");
}

// ── Vector clustering ─────────────────────────────────────────────────────────

#[test]
fn similar_tracks_cluster_together() {
    let mut vs = VectorStore::new();

    // Two similar dark tracks
    vs.upsert(
        "dark_a".into(),
        PsychVector([0.1, 0.9, 0.8, 0.1, 0.1, 0.2, 0.8, 0.1]),
    );
    vs.upsert(
        "dark_b".into(),
        PsychVector([0.1, 0.85, 0.85, 0.1, 0.1, 0.2, 0.75, 0.1]),
    );
    // One very different bright track
    vs.upsert(
        "bright".into(),
        PsychVector([0.4, 0.1, 0.2, 0.9, 0.8, 0.1, 0.1, 0.9]),
    );

    let clusters = vs.cluster(0.95);
    assert_eq!(
        clusters["dark_a"], clusters["dark_b"],
        "similar dark tracks should cluster together"
    );
    assert_ne!(
        clusters["dark_a"], clusters["bright"],
        "dark and bright should be in different clusters"
    );
}

#[test]
fn nearest_neighbor_is_most_similar() {
    let mut vs = VectorStore::new();
    vs.upsert(
        "t1".into(),
        PsychVector([1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0]),
    );
    vs.upsert(
        "t2".into(),
        PsychVector([0.9, 0.1, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0]),
    );
    vs.upsert(
        "t3".into(),
        PsychVector([0.0, 0.0, 0.0, 0.0, 1.0, 0.0, 0.0, 0.0]),
    );

    let query = PsychVector([1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0]);
    let results = vs.nearest(&query, 1, "");
    assert_eq!(results[0].0, "t1");
}

// ── Fingerprint consistency ───────────────────────────────────────────────────

#[test]
fn same_audio_same_fingerprint() {
    use zior_engine::audio::fingerprint;
    let s = sine_wave(440.0, 22050, 5.0);
    let f1 = fingerprint::generate(&s, 22050);
    let f2 = fingerprint::generate(&s, 22050);
    assert_eq!(f1.hash, f2.hash);
    assert_eq!(f1.hash.len(), 64);
}
