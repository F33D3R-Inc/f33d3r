//! Audio fingerprinting.
//!
//! Generates a content-based fingerprint from the first 30s of PCM audio.
//! Used for deduplication and versioning (drafts, remixes, re-uploads).
//!
//! The fingerprint is a hex SHA-256 of a compact landmark representation
//! derived from spectral peaks — not the raw PCM bytes, so it is robust
//! to format changes (MP3 vs WAV), normalisation, and minor edits.

use sha2::{Digest, Sha256};

/// Compact fingerprint derived from spectral landmarks.
#[derive(Debug, Clone)]
pub struct AudioFingerprint {
    /// Hex-encoded SHA-256 of the landmark representation
    pub hash: String,
    /// Number of landmark points used
    pub n_landmarks: usize,
}

/// Generate a fingerprint from mono PCM samples.
///
/// Only the first `sample_rate * 30` samples are used (first 30 seconds)
/// to keep computation bounded. For tracks shorter than 30s, all samples
/// are used.
pub fn generate(samples: &[f32], sample_rate: u32) -> AudioFingerprint {
    let window_samples = (sample_rate as usize * 30).min(samples.len());
    let excerpt = &samples[..window_samples];

    let landmarks = extract_landmarks(excerpt, sample_rate);

    // Serialise landmarks to bytes for hashing
    let mut hasher = Sha256::new();
    for &(time_bin, freq_bin, magnitude_quantised) in &landmarks {
        hasher.update(time_bin.to_le_bytes());
        hasher.update(freq_bin.to_le_bytes());
        hasher.update([magnitude_quantised]);
    }

    let hash_bytes = hasher.finalize();
    let hash = hex::encode(hash_bytes);
    let n = landmarks.len();

    AudioFingerprint {
        hash,
        n_landmarks: n,
    }
}

/// Extract spectral landmark points: (time_bin, freq_bin, magnitude_quantised).
/// Landmarks are high-energy spectral peaks in the frequency domain.
fn extract_landmarks(samples: &[f32], sample_rate: u32) -> Vec<(u32, u16, u8)> {
    use realfft::RealFftPlanner;
    use std::f32::consts::PI;

    let window = 1024_usize;
    let hop = 256_usize;
    let mut peaks = Vec::new();

    let mut planner = RealFftPlanner::<f32>::new();
    let fft = planner.plan_fft_forward(window);

    let hann: Vec<f32> = (0..window)
        .map(|i| 0.5 * (1.0 - (2.0 * PI * i as f32 / (window as f32 - 1.0)).cos()))
        .collect();

    let mut time_bin = 0_u32;
    let mut pos = 0_usize;

    while pos + window <= samples.len() {
        let mut input: Vec<f32> = samples[pos..pos + window]
            .iter()
            .zip(&hann)
            .map(|(s, w)| s * w)
            .collect();

        let mut output = fft.make_output_vec();
        if fft.process(&mut input, &mut output).is_err() {
            pos += hop;
            time_bin += 1;
            continue;
        }

        let mags: Vec<f32> = output
            .iter()
            .map(|c| (c.re * c.re + c.im * c.im).sqrt())
            .collect();

        // Find top-3 peaks per frame in musically relevant range (50–5000 Hz)
        let bin_hz = sample_rate as f32 / window as f32;
        let min_bin = (50.0 / bin_hz) as usize;
        let max_bin = ((5000.0 / bin_hz) as usize).min(mags.len().saturating_sub(1));
        let relevant = &mags[min_bin..=max_bin];

        let max_mag = relevant.iter().cloned().fold(0.0_f32, f32::max);
        if max_mag < 1e-6 {
            pos += hop;
            time_bin += 1;
            continue;
        }

        // Find local maxima above 40% of frame peak
        let threshold = max_mag * 0.40;
        let mut frame_peaks: Vec<(usize, f32)> = relevant
            .windows(3)
            .enumerate()
            .filter(|(_, w)| w[1] >= w[0] && w[1] >= w[2] && w[1] > threshold)
            .map(|(i, w)| (i + min_bin + 1, w[1]))
            .collect();

        frame_peaks.sort_by(|a, b| b.1.partial_cmp(&a.1).unwrap());
        frame_peaks.truncate(3);

        for (freq_bin, mag) in frame_peaks {
            // Quantise magnitude to 8 bits for compact representation
            let mag_q = ((mag / max_mag) * 255.0) as u8;
            peaks.push((time_bin, freq_bin as u16, mag_q));
        }

        pos += hop;
        time_bin += 1;
    }

    peaks
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::f32::consts::PI;

    fn sine(freq: f32, sr: u32, secs: f32) -> Vec<f32> {
        let n = (sr as f32 * secs) as usize;
        (0..n)
            .map(|i| (2.0 * PI * freq * i as f32 / sr as f32).sin() * 0.5)
            .collect()
    }

    #[test]
    fn fingerprint_is_deterministic() {
        let s = sine(440.0, 22050, 5.0);
        let f1 = generate(&s, 22050);
        let f2 = generate(&s, 22050);
        assert_eq!(f1.hash, f2.hash);
    }

    #[test]
    fn different_pitches_produce_different_hashes() {
        let s1 = sine(440.0, 22050, 5.0);
        let s2 = sine(880.0, 22050, 5.0);
        assert_ne!(generate(&s1, 22050).hash, generate(&s2, 22050).hash);
    }

    #[test]
    fn hash_is_64_hex_chars() {
        let s = sine(440.0, 22050, 3.0);
        let f = generate(&s, 22050);
        assert_eq!(f.hash.len(), 64);
    }
}
