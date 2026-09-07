//! Mood classification.
//!
//! Maps audio features to named moods and context tags.
//! These are human-readable labels attached to track metadata.
//! The underlying representation is always the PsychVector.

#[derive(Debug, Clone, PartialEq)]
pub enum Mood {
    // High-energy
    Euphoric,
    Aggressive,
    Intense,
    Energetic,
    // Mid-energy
    Confident,
    Melancholic,
    Nostalgic,
    Mysterious,
    // Low-energy
    Intimate,
    Peaceful,
    Reflective,
    Tense,
    // Special
    Dark,
    Dreamlike,
}

impl Mood {
    pub fn as_str(&self) -> &'static str {
        match self {
            Mood::Euphoric => "euphoric",
            Mood::Aggressive => "aggressive",
            Mood::Intense => "intense",
            Mood::Energetic => "energetic",
            Mood::Confident => "confident",
            Mood::Melancholic => "melancholic",
            Mood::Nostalgic => "nostalgic",
            Mood::Mysterious => "mysterious",
            Mood::Intimate => "intimate",
            Mood::Peaceful => "peaceful",
            Mood::Reflective => "reflective",
            Mood::Tense => "tense",
            Mood::Dark => "dark",
            Mood::Dreamlike => "dreamlike",
        }
    }
}

/// Context tags for the track (usage contexts).
pub fn context_tags(bpm: f32, rms_energy: f32, tonal_valence: f32, mood: &Mood) -> Vec<String> {
    let mut tags = Vec::new();

    if bpm > 125.0 && rms_energy > 0.5 {
        tags.push("workout".to_string());
    }
    if bpm > 120.0 && rms_energy > 0.6 {
        tags.push("club".to_string());
    }
    if bpm < 90.0 && rms_energy < 0.4 {
        tags.push("study".to_string());
    }
    if bpm < 80.0 && tonal_valence < 0.4 {
        tags.push("late_night".to_string());
    }
    if bpm < 75.0 && rms_energy < 0.3 {
        tags.push("sleep".to_string());
    }
    if tonal_valence > 0.7 && bpm > 100.0 {
        tags.push("commute".to_string());
    }

    match mood {
        Mood::Melancholic | Mood::Reflective => tags.push("heartbreak".to_string()),
        Mood::Euphoric => tags.push("celebration".to_string()),
        Mood::Aggressive | Mood::Intense => tags.push("focus".to_string()),
        Mood::Peaceful | Mood::Dreamlike => tags.push("meditation".to_string()),
        Mood::Nostalgic => tags.push("memories".to_string()),
        _ => {}
    }

    tags.dedup();
    tags
}

/// Classify a track's primary mood from audio features.
pub fn classify(
    bpm_normalised: f32,
    rms_energy: f32,
    tonal_valence: f32,
    spectral_flux: f32,
    bass_energy: f32,
) -> Mood {
    let is_fast = bpm_normalised > 0.50; // > ~130 BPM
    let is_loud = rms_energy > 0.55;
    let is_bright = tonal_valence > 0.55;
    let is_dynamic = spectral_flux > 0.45;
    let is_bassy = bass_energy > 0.50;

    match (is_fast, is_loud, is_bright, is_dynamic, is_bassy) {
        (true, true, true, _, _) => Mood::Euphoric,
        (true, true, false, true, true) => Mood::Aggressive,
        (true, true, false, _, _) => Mood::Intense,
        (true, false, true, _, _) => Mood::Energetic,
        (false, true, true, _, _) => Mood::Confident,
        // Slow, quiet, bright AND in motion (high spectral flux) is dreamlike;
        // it must be decided before the still, bright case below, which used
        // to shadow it and left Dreamlike unreachable.
        (false, false, true, true, _) => Mood::Dreamlike,
        (false, false, true, _, _) => Mood::Nostalgic,
        (false, false, false, false, false) => Mood::Peaceful,
        (false, false, false, true, _) => Mood::Tense,
        (_, _, false, _, true) => Mood::Dark,
        (false, _, false, false, _) => Mood::Reflective,
        _ => Mood::Mysterious,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn high_energy_major_is_euphoric() {
        let mood = classify(0.8, 0.8, 0.8, 0.5, 0.3);
        assert_eq!(mood, Mood::Euphoric);
    }

    #[test]
    fn slow_quiet_minor_is_peaceful_or_reflective() {
        let mood = classify(0.1, 0.1, 0.3, 0.1, 0.1);
        assert!(matches!(mood, Mood::Peaceful | Mood::Reflective));
    }

    #[test]
    fn slow_quiet_bright_moving_is_dreamlike() {
        assert_eq!(classify(0.2, 0.2, 0.8, 0.7, 0.2), Mood::Dreamlike);
        assert_eq!(classify(0.2, 0.2, 0.8, 0.1, 0.2), Mood::Nostalgic);
    }

    #[test]
    fn context_tags_workout_for_high_bpm() {
        let tags = context_tags(140.0, 0.7, 0.6, &Mood::Energetic);
        assert!(tags.contains(&"workout".to_string()));
    }
}
