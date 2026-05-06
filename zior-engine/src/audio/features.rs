//! Audio feature extraction.
//!
//! Extracts the core measurable properties of a track:
//!   - BPM (onset-based tempo detection)
//!   - Key and mode (chroma vector analysis)
//!   - RMS loudness
//!   - Spectral centroid, rolloff, flux
//!   - Bass / mid / treble energy ratios
//!
//! All features normalised to [0, 1] before being passed to the Jung mapper.

use anyhow::Result;
use realfft::RealFftPlanner;
use std::f32::consts::PI;

/// All extracted audio features for a single track.
#[derive(Debug, Clone)]
pub struct AudioFeatures {
    // ── Rhythm ────────────────────────────────────────────────────────────────
    /// Beats per minute, normalised to [0, 1] over [40, 220] BPM range
    pub bpm_raw: f32,
    pub bpm_normalised: f32,

    // ── Tonality ──────────────────────────────────────────────────────────────
    /// Detected pitch class: 0=C, 1=C#, 2=D, ... 11=B
    pub key: u8,
    /// true = major, false = minor
    pub is_major: bool,
    /// Valence proxy from key/mode: [0, 1] (major + bright key = high)
    pub tonal_valence: f32,

    // ── Dynamics ──────────────────────────────────────────────────────────────
    /// RMS energy, normalised to [0, 1]
    pub rms_energy: f32,
    /// Dynamic range (peak/RMS ratio), normalised to [0, 1]
    pub dynamic_range: f32,

    // ── Spectral ──────────────────────────────────────────────────────────────
    /// Spectral centroid (brightness), normalised to [0, 1]
    pub spectral_centroid: f32,
    /// Spectral rolloff (edge frequency), normalised to [0, 1]
    pub spectral_rolloff: f32,
    /// Mean spectral flux (variation over time), normalised to [0, 1]
    pub spectral_flux: f32,

    // ── Band energies ─────────────────────────────────────────────────────────
    /// Sub-bass + bass energy fraction (< 300 Hz)
    pub bass_energy: f32,
    /// Mid energy fraction (300 Hz – 3 kHz)
    pub mid_energy: f32,
    /// High energy fraction (> 3 kHz)
    pub treble_energy: f32,

    // ── Vocal detection ───────────────────────────────────────────────────────
    /// Estimated probability of vocal presence [0, 1]
    pub vocal_probability: f32,
}

/// Extract features from mono PCM samples at the given sample rate.
pub fn extract(
    samples: &[f32],
    sample_rate: u32,
    fft_window: usize,
    hop: usize,
) -> Result<AudioFeatures> {
    let rms = compute_rms(samples);
    let peak = samples.iter().cloned().fold(0.0_f32, f32::max);
    let dynamic_range = if rms > 1e-6 {
        (peak / rms).min(100.0) / 100.0
    } else {
        0.0
    };

    // Compute FFT frames
    let frames = compute_fft_frames(samples, fft_window, hop);

    let spectral_centroid = mean_spectral_centroid(&frames, sample_rate, fft_window);
    let spectral_rolloff = mean_spectral_rolloff(&frames, sample_rate, fft_window);
    let spectral_flux = mean_spectral_flux(&frames);

    let (bass, mid, treble) = band_energy_ratios(&frames, sample_rate, fft_window);

    let bpm_raw = estimate_bpm(samples, sample_rate);
    let bpm_normalised = ((bpm_raw - 40.0) / 180.0).clamp(0.0, 1.0);

    let chroma = compute_chroma(&frames, sample_rate, fft_window);
    let (key, is_major, tonal_valence) = detect_key_and_mode(&chroma);

    // Vocal detection: vocals sit in 200–3400 Hz formant range with
    // moderate spectral flux. Approximate with mid-energy + flux heuristic.
    let vocal_probability = (mid * 0.6 + spectral_flux * 0.4).clamp(0.0, 1.0);

    Ok(AudioFeatures {
        bpm_raw,
        bpm_normalised,
        key,
        is_major,
        tonal_valence,
        rms_energy: rms.min(1.0),
        dynamic_range,
        spectral_centroid,
        spectral_rolloff,
        spectral_flux,
        bass_energy: bass,
        mid_energy: mid,
        treble_energy: treble,
        vocal_probability,
    })
}

// ── RMS ───────────────────────────────────────────────────────────────────────

fn compute_rms(samples: &[f32]) -> f32 {
    if samples.is_empty() {
        return 0.0;
    }
    let sq_sum: f32 = samples.iter().map(|s| s * s).sum();
    (sq_sum / samples.len() as f32).sqrt()
}

// ── FFT frames ────────────────────────────────────────────────────────────────

fn compute_fft_frames(samples: &[f32], window: usize, hop: usize) -> Vec<Vec<f32>> {
    let mut planner = RealFftPlanner::<f32>::new();
    let fft = planner.plan_fft_forward(window);
    let mut frames = Vec::new();
    let hann: Vec<f32> = (0..window)
        .map(|i| 0.5 * (1.0 - (2.0 * PI * i as f32 / (window as f32 - 1.0)).cos()))
        .collect();

    let mut pos = 0;
    while pos + window <= samples.len() {
        let mut input: Vec<f32> = samples[pos..pos + window]
            .iter()
            .zip(&hann)
            .map(|(s, w)| s * w)
            .collect();

        let mut output = fft.make_output_vec();
        if fft.process(&mut input, &mut output).is_ok() {
            let mags: Vec<f32> = output
                .iter()
                .map(|c| (c.re * c.re + c.im * c.im).sqrt())
                .collect();
            frames.push(mags);
        }
        pos += hop;
    }
    frames
}

// ── Spectral features ─────────────────────────────────────────────────────────

fn mean_spectral_centroid(frames: &[Vec<f32>], sr: u32, window: usize) -> f32 {
    if frames.is_empty() {
        return 0.0;
    }
    let centroids: Vec<f32> = frames
        .iter()
        .map(|mags| {
            let total: f32 = mags.iter().sum();
            if total < 1e-10 {
                return 0.0;
            }
            let weighted: f32 = mags
                .iter()
                .enumerate()
                .map(|(i, m)| i as f32 * m)
                .sum::<f32>();
            let bin_hz = sr as f32 / window as f32;
            (weighted / total * bin_hz) / (sr as f32 / 2.0)
        })
        .collect();
    centroids.iter().sum::<f32>() / centroids.len() as f32
}

fn mean_spectral_rolloff(frames: &[Vec<f32>], sr: u32, window: usize) -> f32 {
    if frames.is_empty() {
        return 0.0;
    }
    let rolloffs: Vec<f32> = frames
        .iter()
        .map(|mags| {
            let total: f32 = mags.iter().sum();
            let threshold = total * 0.85;
            let mut cum = 0.0_f32;
            let bin_hz = sr as f32 / window as f32;
            for (i, m) in mags.iter().enumerate() {
                cum += m;
                if cum >= threshold {
                    return (i as f32 * bin_hz) / (sr as f32 / 2.0);
                }
            }
            1.0
        })
        .collect();
    rolloffs.iter().sum::<f32>() / rolloffs.len() as f32
}

fn mean_spectral_flux(frames: &[Vec<f32>]) -> f32 {
    if frames.len() < 2 {
        return 0.0;
    }
    let fluxes: Vec<f32> = frames
        .windows(2)
        .map(|w| {
            let diff: f32 = w[0]
                .iter()
                .zip(&w[1])
                .map(|(a, b)| (b - a).max(0.0).powi(2))
                .sum();
            diff.sqrt()
        })
        .collect();
    let mean = fluxes.iter().sum::<f32>() / fluxes.len() as f32;
    // Normalise: typical flux values sit in [0, 50]; cap at 100
    (mean / 100.0).clamp(0.0, 1.0)
}

fn band_energy_ratios(frames: &[Vec<f32>], sr: u32, window: usize) -> (f32, f32, f32) {
    if frames.is_empty() {
        return (0.0, 0.0, 0.0);
    }
    let bin_hz = sr as f32 / window as f32;
    let bass_max = (300.0 / bin_hz) as usize;
    let mid_max = (3000.0 / bin_hz) as usize;

    let (mut b, mut m, mut t, mut total) = (0.0_f32, 0.0_f32, 0.0_f32, 0.0_f32);
    for frame in frames {
        for (i, mag) in frame.iter().enumerate() {
            let e = mag * mag;
            if i < bass_max {
                b += e;
            } else if i < mid_max {
                m += e;
            } else {
                t += e;
            }
            total += e;
        }
    }
    if total < 1e-10 {
        return (0.0, 0.0, 0.0);
    }
    (b / total, m / total, t / total)
}

// ── BPM estimation ────────────────────────────────────────────────────────────
//
// Onset-strength based tempo detection.
// Uses spectral flux onsets, then autocorrelation over the onset envelope.

fn estimate_bpm(samples: &[f32], sample_rate: u32) -> f32 {
    // Build onset strength envelope at ~100 fps
    let frame_size = (sample_rate as usize / 100).max(128);
    let hop = frame_size / 2;
    let mut energy_prev = 0.0_f32;
    let mut onsets: Vec<f32> = Vec::new();

    let mut pos = 0;
    while pos + frame_size <= samples.len() {
        let frame = &samples[pos..pos + frame_size];
        let energy = frame.iter().map(|s| s * s).sum::<f32>() / frame_size as f32;
        let diff = (energy - energy_prev).max(0.0);
        onsets.push(diff);
        energy_prev = energy;
        pos += hop;
    }

    if onsets.len() < 4 {
        return 120.0;
    } // fallback

    // Autocorrelation to find dominant period
    let fps = sample_rate as f32 / hop as f32;
    let min_period = (fps * 60.0 / 220.0) as usize; // 220 BPM max
    let max_period = (fps * 60.0 / 40.0) as usize; // 40 BPM min

    let max_period = max_period.min(onsets.len() / 2);
    if min_period >= max_period {
        return 120.0;
    }

    let mut best_corr = f32::NEG_INFINITY;
    let mut best_period = min_period;

    for period in min_period..max_period {
        let corr: f32 = onsets[..onsets.len() - period]
            .iter()
            .zip(&onsets[period..])
            .map(|(a, b)| a * b)
            .sum();
        if corr > best_corr {
            best_corr = corr;
            best_period = period;
        }
    }

    (fps * 60.0 / best_period as f32).clamp(40.0, 220.0)
}

// ── Chroma and key detection ──────────────────────────────────────────────────
//
// Krumhansl-Schmuckler key-finding algorithm.
// Projects spectral energy onto 12 pitch classes (chroma vector),
// then correlates with major and minor key profiles.

fn compute_chroma(frames: &[Vec<f32>], sr: u32, window: usize) -> [f32; 12] {
    let mut chroma = [0.0_f32; 12];
    let bin_hz = sr as f32 / window as f32;

    for frame in frames {
        for (bin, mag) in frame.iter().enumerate() {
            let freq = bin as f32 * bin_hz;
            if freq < 27.5 || freq > 4186.0 {
                continue;
            }
            // MIDI note from frequency
            let midi = 12.0 * (freq / 440.0).log2() + 69.0;
            let pitch = (midi.round() as i32).rem_euclid(12) as usize;
            chroma[pitch] += mag;
        }
    }

    // Normalise chroma vector
    let sum: f32 = chroma.iter().sum();
    if sum > 0.0 {
        chroma.iter_mut().for_each(|c| *c /= sum);
    }
    chroma
}

fn detect_key_and_mode(chroma: &[f32; 12]) -> (u8, bool, f32) {
    // Krumhansl-Schmuckler key profiles
    let major_profile: [f32; 12] = [
        6.35, 2.23, 3.48, 2.33, 4.38, 4.09, 2.52, 5.19, 2.39, 3.66, 2.29, 2.88,
    ];
    let minor_profile: [f32; 12] = [
        6.33, 2.68, 3.52, 5.38, 2.60, 3.53, 2.54, 4.75, 3.98, 2.69, 3.34, 3.17,
    ];

    let mut best_score = f32::NEG_INFINITY;
    let mut best_key = 0_u8;
    let mut best_major = true;

    for root in 0..12_u8 {
        // Rotate profiles to match root
        let major_score = correlate(chroma, &rotate(&major_profile, root as usize));
        let minor_score = correlate(chroma, &rotate(&minor_profile, root as usize));

        if major_score > best_score {
            best_score = major_score;
            best_key = root;
            best_major = true;
        }
        if minor_score > best_score {
            best_score = minor_score;
            best_key = root;
            best_major = false;
        }
    }

    // Tonal valence: major keys have higher base valence; bright keys (C, G, D) higher still
    let brightness = [
        1.0_f32, 0.4, 0.7, 0.5, 0.8, 0.6, 0.3, 0.9, 0.5, 0.7, 0.4, 0.6,
    ];
    let mode_boost = if best_major { 0.25_f32 } else { 0.0 };
    let tonal_valence = (brightness[best_key as usize] * 0.7 + mode_boost).clamp(0.0, 1.0);

    (best_key, best_major, tonal_valence)
}

fn rotate(profile: &[f32; 12], n: usize) -> [f32; 12] {
    let mut out = [0.0_f32; 12];
    for i in 0..12 {
        out[i] = profile[(i + n) % 12];
    }
    out
}

fn correlate(a: &[f32; 12], b: &[f32; 12]) -> f32 {
    let mean_a = a.iter().sum::<f32>() / 12.0;
    let mean_b = b.iter().sum::<f32>() / 12.0;
    let num: f32 = a
        .iter()
        .zip(b.iter())
        .map(|(x, y)| (x - mean_a) * (y - mean_b))
        .sum();
    let da: f32 = a.iter().map(|x| (x - mean_a).powi(2)).sum::<f32>().sqrt();
    let db: f32 = b.iter().map(|y| (y - mean_b).powi(2)).sum::<f32>().sqrt();
    if da * db < 1e-10 {
        0.0
    } else {
        num / (da * db)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn silence(len: usize) -> Vec<f32> {
        vec![0.0; len]
    }

    fn sine_wave(freq: f32, sr: u32, duration_secs: f32) -> Vec<f32> {
        let n = (sr as f32 * duration_secs) as usize;
        (0..n)
            .map(|i| (2.0 * PI * freq * i as f32 / sr as f32).sin() * 0.5)
            .collect()
    }

    #[test]
    fn rms_of_silence_is_zero() {
        assert!(compute_rms(&silence(1024)) < 1e-6);
    }

    #[test]
    fn rms_of_sine_is_nonzero() {
        let s = sine_wave(440.0, 22050, 1.0);
        let r = compute_rms(&s);
        assert!(r > 0.3 && r < 0.4, "sine RMS should be ~0.354, got {}", r);
    }

    #[test]
    fn bpm_is_in_valid_range() {
        let s = sine_wave(440.0, 22050, 10.0);
        let bpm = estimate_bpm(&s, 22050);
        assert!(bpm >= 40.0 && bpm <= 220.0, "bpm={}", bpm);
    }

    #[test]
    fn chroma_sums_to_one() {
        let s = sine_wave(440.0, 22050, 3.0);
        let frames = compute_fft_frames(&s, 2048, 512);
        if !frames.is_empty() {
            let chroma = compute_chroma(&frames, 22050, 2048);
            let sum: f32 = chroma.iter().sum();
            assert!(
                (sum - 1.0).abs() < 0.01 || sum < 0.01,
                "chroma should sum ~1 or be zero, got {}",
                sum
            );
        }
    }

    #[test]
    fn features_from_sine_complete() {
        let s = sine_wave(440.0, 22050, 5.0);
        let f = extract(&s, 22050, 2048, 512).unwrap();
        assert!(f.rms_energy > 0.0);
        assert!(f.bpm_raw >= 40.0 && f.bpm_raw <= 220.0);
        assert!(f.tonal_valence >= 0.0 && f.tonal_valence <= 1.0);
        assert!((f.bass_energy + f.mid_energy + f.treble_energy - 1.0).abs() < 0.01);
    }
}
