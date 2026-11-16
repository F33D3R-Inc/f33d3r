// tests/pipeline_tests.rs
//
// End-to-end tests for the Zior processing pipeline.
// Tests the full chain: features → Jung mapping.

use std::f32::consts::PI;

use zior_engine::audio::features;
use zior_engine::jung::mapper;

fn sine_wave(freq: f32, sr: u32, secs: f32) -> Vec<f32> {
    let n = (sr as f32 * secs) as usize;
    (0..n)
        .map(|i| (2.0 * PI * freq * i as f32 / sr as f32).sin() * 0.5)
        .collect()
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
    let mapped = mapper::map_to_axes(&f);
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
    let mapped = mapper::map_to_axes(&f);
    let v = mapped.psych_vector.0;
    assert!(v[4] > 0.35, "attachment={}", v[4]);
    assert!(v[7] > 0.3, "release={}", v[7]);
}
