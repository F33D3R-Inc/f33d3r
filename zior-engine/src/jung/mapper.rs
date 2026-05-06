//! Jung Mapper — the core translation layer.
//!
//! Maps audio features onto the 8 Jungian psychological axes,
//! producing the `topic_vector` that AethyrRank uses for ranking.
//!
//! ## Mapping logic
//!
//! Each axis is computed from a weighted combination of audio features.
//! The weightings are derived from psychological theory + empirical observation:
//!
//! ```
//! Axis 0 — Persona:     production_polish, spectral_clarity, vocal_presence
//! Axis 1 — Shadow:      minor_mode, dark_timbre, low_valence, bass_dominance
//! Axis 2 — Agency:      BPM, RMS_energy, onset_density, momentum
//! Axis 3 — Integration: tonal_stability, harmonic_resolution, structure_clarity
//! Axis 4 — Attachment:  vocal_warmth, intimacy_proxy, low_BPM, mid_presence
//! Axis 5 — Disruption:  spectral_flux, genre_outlier, dynamic_contrast
//! Axis 6 — Tension:     dissonance_proxy, unresolved_harmonic, minor_mode
//! Axis 7 — Release:     major_resolution, drop_payoff, high_valence
//! ```

use crate::audio::features::AudioFeatures;
use crate::jung::axes::PsychVector;
use crate::jung::mood::{classify, context_tags, Mood};

pub struct MappedTrack {
    /// The 8D psychological vector — directly compatible with AethyrRank's topic_vector
    pub psych_vector: PsychVector,
    pub mood: Mood,
    pub context_tags: Vec<String>,
    /// Genre-free descriptor string (for display)
    pub descriptor: String,
}

/// Map audio features to the Jung psychological axes.
pub fn map_to_axes(
    features: &AudioFeatures,
    behavioral_vector: Option<&PsychVector>,
) -> MappedTrack {
    let f = features;

    // ── Axis 0: Persona ───────────────────────────────────────────────────────
    // High when: crisp high-frequency presence, clean dynamics, vocal production
    let persona = weighted(
        &[
            f.treble_energy,
            f.vocal_probability,
            1.0 - f.dynamic_range * 0.3,
            f.spectral_centroid,
        ],
        &[0.30, 0.35, 0.20, 0.15],
    );

    // ── Axis 1: Shadow ────────────────────────────────────────────────────────
    // High when: minor mode, low valence, bass-heavy, dark timbre
    let shadow = weighted(
        &[
            if f.is_major { 0.0 } else { 1.0 },
            1.0 - f.tonal_valence,
            f.bass_energy,
            f.dynamic_range,
        ],
        &[0.35, 0.30, 0.20, 0.15],
    );

    // ── Axis 2: Agency ────────────────────────────────────────────────────────
    // High when: fast, loud, energetic
    let agency = weighted(
        &[f.bpm_normalised, f.rms_energy, f.spectral_flux],
        &[0.45, 0.35, 0.20],
    );

    // ── Axis 3: Integration ───────────────────────────────────────────────────
    // High when: tonally stable (major, high valence), low flux (controlled structure)
    let integration = weighted(
        &[
            f.tonal_valence,
            if f.is_major { 1.0 } else { 0.4 },
            1.0 - f.spectral_flux,
            f.mid_energy,
        ],
        &[0.35, 0.25, 0.25, 0.15],
    );

    // ── Axis 4: Attachment ────────────────────────────────────────────────────
    // High when: vocal, intimate, slow, warm mid presence
    let attachment = weighted(
        &[
            f.vocal_probability,
            1.0 - f.bpm_normalised,
            f.mid_energy,
            1.0 - f.treble_energy,
        ],
        &[0.40, 0.25, 0.25, 0.10],
    );

    // ── Axis 5: Disruption ────────────────────────────────────────────────────
    // High when: high spectral flux, genre-bending (low tonal stability), sharp dynamics
    let disruption = weighted(
        &[f.spectral_flux, f.dynamic_range, 1.0 - f.mid_energy],
        &[0.50, 0.30, 0.20],
    );

    // ── Axis 6: Tension ───────────────────────────────────────────────────────
    // High when: minor, dissonant, high spectral rolloff, high bass
    // Dissonance proxy: minor mode + spectral flux + bass energy
    let tension = weighted(
        &[
            if f.is_major { 0.1 } else { 0.8 },
            f.spectral_rolloff,
            f.bass_energy * 0.5,
            f.spectral_flux * 0.3,
        ],
        &[0.40, 0.25, 0.20, 0.15],
    );

    // ── Axis 7: Release ───────────────────────────────────────────────────────
    // High when: major key, high valence, drop payoff (high energy, high brightness)
    let release = weighted(
        &[
            f.tonal_valence,
            if f.is_major { 1.0 } else { 0.2 },
            f.rms_energy * f.spectral_centroid, // energy × brightness = euphoria proxy
        ],
        &[0.40, 0.30, 0.30],
    );

    let mut audio_vec = PsychVector([
        persona,
        shadow,
        agency,
        integration,
        attachment,
        disruption,
        tension,
        release,
    ]);

    // Blend with behavioral vector if available
    let final_vec = if let Some(bv) = behavioral_vector {
        // Behavioral signals carry 35% of the final vector weight
        PsychVector::blend(&audio_vec, bv, 0.35)
    } else {
        audio_vec.normalise();
        audio_vec
    };

    let mood = classify(
        f.bpm_normalised,
        f.rms_energy,
        f.tonal_valence,
        f.spectral_flux,
        f.bass_energy,
    );

    let context_tags = context_tags(f.bpm_raw, f.rms_energy, f.tonal_valence, &mood);

    let descriptor = build_descriptor(features, &mood);

    MappedTrack {
        psych_vector: final_vec,
        mood,
        context_tags,
        descriptor,
    }
}

fn build_descriptor(f: &AudioFeatures, mood: &Mood) -> String {
    let note_names = [
        "C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B",
    ];
    let key_str = format!(
        "{} {}",
        note_names[f.key as usize],
        if f.is_major { "major" } else { "minor" }
    );
    let bpm_str = format!("{:.0} BPM", f.bpm_raw);
    let vocal_str = if f.vocal_probability > 0.5 {
        "vocal"
    } else {
        "instrumental"
    };
    format!(
        "{} · {} · {} · {}",
        mood.as_str(),
        key_str,
        bpm_str,
        vocal_str
    )
}

fn weighted(values: &[f32], weights: &[f32]) -> f32 {
    debug_assert_eq!(values.len(), weights.len());
    values
        .iter()
        .zip(weights.iter())
        .map(|(v, w)| v.clamp(0.0, 1.0) * w)
        .sum::<f32>()
        .clamp(0.0, 1.0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::audio::features::AudioFeatures;

    fn dark_heavy_features() -> AudioFeatures {
        AudioFeatures {
            bpm_raw: 140.0,
            bpm_normalised: 0.56,
            key: 9,
            is_major: false,
            tonal_valence: 0.2,
            rms_energy: 0.8,
            dynamic_range: 0.6,
            spectral_centroid: 0.3,
            spectral_rolloff: 0.5,
            spectral_flux: 0.7,
            bass_energy: 0.7,
            mid_energy: 0.2,
            treble_energy: 0.1,
            vocal_probability: 0.2,
        }
    }

    fn bright_ballad_features() -> AudioFeatures {
        AudioFeatures {
            bpm_raw: 72.0,
            bpm_normalised: 0.18,
            key: 0,
            is_major: true,
            tonal_valence: 0.85,
            rms_energy: 0.3,
            dynamic_range: 0.4,
            spectral_centroid: 0.6,
            spectral_rolloff: 0.4,
            spectral_flux: 0.2,
            bass_energy: 0.2,
            mid_energy: 0.6,
            treble_energy: 0.2,
            vocal_probability: 0.9,
        }
    }

    #[test]
    fn dark_track_has_high_shadow_and_agency() {
        let mapped = map_to_axes(&dark_heavy_features(), None);
        let v = mapped.psych_vector.0;
        assert!(v[1] > 0.4, "shadow={:.3}", v[1]); // shadow
        assert!(v[2] > 0.4, "agency={:.3}", v[2]); // agency
    }

    #[test]
    fn ballad_has_high_attachment_and_release() {
        let mapped = map_to_axes(&bright_ballad_features(), None);
        let v = mapped.psych_vector.0;
        assert!(v[4] > 0.4, "attachment={:.3}", v[4]); // attachment
        assert!(v[7] > 0.3, "release={:.3}", v[7]); // release
    }

    #[test]
    fn all_axes_in_unit_interval() {
        for f in [dark_heavy_features(), bright_ballad_features()] {
            let mapped = map_to_axes(&f, None);
            for &x in &mapped.psych_vector.0 {
                assert!(x >= 0.0 && x <= 1.0, "axis out of range: {}", x);
            }
        }
    }

    #[test]
    fn dark_track_not_euphoric() {
        let mapped = map_to_axes(&dark_heavy_features(), None);
        assert_ne!(mapped.mood, crate::jung::mood::Mood::Euphoric);
    }

    #[test]
    fn descriptor_contains_bpm() {
        let mapped = map_to_axes(&dark_heavy_features(), None);
        assert!(mapped.descriptor.contains("BPM"), "{}", mapped.descriptor);
    }
}
