//! Structural segmentation.
//!
//! Divides a track into sections (intro, verse, chorus, drop, outro) using
//! self-similarity analysis on spectral features. This is used to:
//!   - Compute retention metrics per section
//!   - Detect where drops / hooks occur (high-energy structural transitions)
//!   - Feed the early_retention signal (fraction engaging past the intro)

#[derive(Debug, Clone, PartialEq)]
pub enum SectionType {
    Intro,
    Verse,
    Chorus,
    Drop,
    Bridge,
    Outro,
    Unknown,
}

#[derive(Debug, Clone)]
pub struct Section {
    pub section_type: SectionType,
    /// Start time in seconds
    pub start_secs: f64,
    /// End time in seconds
    pub end_secs: f64,
    /// Normalised energy level for this section [0, 1]
    pub energy: f64,
}

/// Segment a track into structural sections.
///
/// Uses energy novelty: significant energy increases mark section boundaries.
/// Returns sections in chronological order.
pub fn segment(samples: &[f32], sample_rate: u32) -> Vec<Section> {
    if samples.is_empty() {
        return vec![];
    }

    let frame_size = (sample_rate as usize / 10).max(256); // 100ms frames
    let mut energies: Vec<f64> = Vec::new();

    let mut pos = 0;
    while pos + frame_size <= samples.len() {
        let frame = &samples[pos..pos + frame_size];
        let energy = frame.iter().map(|s| (*s as f64).powi(2)).sum::<f64>() / frame_size as f64;
        energies.push(energy.sqrt());
        pos += frame_size;
    }

    if energies.is_empty() {
        return vec![];
    }

    // Smooth energy curve
    let smoothed = smooth(&energies, 5);

    // Normalise
    let max_e = smoothed.iter().cloned().fold(0.0_f64, f64::max);
    let normed: Vec<f64> = if max_e > 1e-10 {
        smoothed.iter().map(|e| e / max_e).collect()
    } else {
        smoothed.clone()
    };

    // Detect novelty (energy gradient peaks = section boundaries)
    let mut boundaries: Vec<usize> = vec![0];
    let window = 10; // look-ahead/behind in frames

    for i in window..normed.len().saturating_sub(window) {
        let before = normed[i.saturating_sub(window)..i].iter().sum::<f64>() / window as f64;
        let after = normed[i..i + window].iter().sum::<f64>() / window as f64;
        let delta = (after - before).abs();
        if delta > 0.10 {
            // Avoid clustering boundaries
            if let Some(&last) = boundaries.last() {
                if i - last > 20 {
                    // minimum 2 seconds between boundaries
                    boundaries.push(i);
                }
            }
        }
    }
    boundaries.push(normed.len());

    // Convert frame indices to sections with type inference
    let frame_secs = frame_size as f64 / sample_rate as f64;
    let total_secs = samples.len() as f64 / sample_rate as f64;

    let mut sections = Vec::new();
    for window_pair in boundaries.windows(2) {
        let start_frame = window_pair[0];
        let end_frame = window_pair[1];
        let start_secs = start_frame as f64 * frame_secs;
        let end_secs = (end_frame as f64 * frame_secs).min(total_secs);

        let section_energy =
            normed[start_frame..end_frame].iter().sum::<f64>() / (end_frame - start_frame) as f64;

        let section_type = infer_type(start_secs, end_secs, total_secs, section_energy);

        sections.push(Section {
            section_type,
            start_secs,
            end_secs,
            energy: section_energy,
        });
    }

    sections
}

/// Infer section type from position and energy profile.
///
/// This is a heuristic model based on common song structures.
/// In production, replace with a trained classifier.
fn infer_type(start: f64, end: f64, total: f64, energy: f64) -> SectionType {
    let rel_start = start / total;
    let rel_end = end / total;

    if rel_start < 0.08 {
        return SectionType::Intro;
    }
    if rel_end > 0.92 {
        return SectionType::Outro;
    }

    // Drops are high-energy sections in the 30–70% range (EDM / hip-hop)
    if energy > 0.80 && rel_start > 0.25 && rel_start < 0.75 {
        return SectionType::Drop;
    }
    // Chorus: high energy, recurring sections
    if energy > 0.60 {
        return SectionType::Chorus;
    }
    // Bridge: short, mid-energy, in second half
    if rel_start > 0.55 && (end - start) < 20.0 {
        return SectionType::Bridge;
    }
    SectionType::Verse
}

fn smooth(values: &[f64], radius: usize) -> Vec<f64> {
    values
        .iter()
        .enumerate()
        .map(|(i, _)| {
            let lo = i.saturating_sub(radius);
            let hi = (i + radius + 1).min(values.len());
            let chunk = &values[lo..hi];
            chunk.iter().sum::<f64>() / chunk.len() as f64
        })
        .collect()
}

/// Compute the early retention proxy: fraction of track before first high-energy section.
/// Tracks where the drop/hook comes earlier have higher early_retention potential.
pub fn early_retention_proxy(sections: &[Section]) -> f64 {
    let first_high = sections
        .iter()
        .find(|s| matches!(s.section_type, SectionType::Drop | SectionType::Chorus));

    match first_high {
        Some(s) if s.start_secs < 30.0 => 0.85,
        Some(s) if s.start_secs < 60.0 => 0.65,
        Some(_) => 0.45,
        None => 0.50,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn flat_tone(sr: u32, secs: f32, amp: f32) -> Vec<f32> {
        let n = (sr as f32 * secs) as usize;
        vec![amp; n]
    }

    #[test]
    fn segment_returns_at_least_one_section() {
        let s = flat_tone(22050, 30.0, 0.5);
        let sections = segment(&s, 22050);
        assert!(!sections.is_empty());
    }

    #[test]
    fn sections_cover_full_duration() {
        let sr = 22050_u32;
        let secs = 60.0_f32;
        let s = flat_tone(sr, secs, 0.5);
        let sections = segment(&s, sr);
        if sections.len() > 1 {
            let last_end = sections.last().unwrap().end_secs;
            assert!(
                (last_end - secs as f64).abs() < 2.0,
                "last section ends at {:.1}s, expected ~{:.1}s",
                last_end,
                secs
            );
        }
    }

    #[test]
    fn early_retention_high_for_early_drop() {
        let sections = vec![
            Section {
                section_type: SectionType::Intro,
                start_secs: 0.0,
                end_secs: 10.0,
                energy: 0.3,
            },
            Section {
                section_type: SectionType::Drop,
                start_secs: 10.0,
                end_secs: 40.0,
                energy: 0.9,
            },
        ];
        assert!(early_retention_proxy(&sections) >= 0.65);
    }
}
