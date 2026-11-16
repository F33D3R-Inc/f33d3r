//! Hash arithmetic: the 64-bit perceptual hash, its band index, and the
//! chromaprint decoder and comparator.
//!
//! The worker computes every perceptual value; Abraxas only compares them.
//! A perceptual hash matches only when the same algorithm produced both
//! sides, so nothing here computes a hash from pixels or samples.

use base64::alphabet;
use base64::engine::general_purpose::{GeneralPurpose, GeneralPurposeConfig};
use base64::engine::DecodePaddingMode;
use base64::Engine;

// ── phash ────────────────────────────────────────────────────────────────────

/// Parses a 64-bit perceptual hash: 16 hex digits (imagehash's text form).
/// Stored as the two's-complement BIGINT of those bits.
pub fn parse_phash_hex(s: &str) -> Option<i64> {
    let s = s.trim();
    if s.len() != 16 || !s.bytes().all(|b| b.is_ascii_hexdigit()) {
        return None;
    }
    u64::from_str_radix(s, 16).ok().map(|u| u as i64)
}

pub fn phash_hex(h: i64) -> String {
    format!("{:016x}", h as u64)
}

pub fn hamming(a: i64, b: i64) -> u32 {
    ((a ^ b) as u64).count_ones()
}

/// Number of bands the 64 bits are split into for multi-index hashing.
pub const PHASH_BANDS: usize = 7;

/// The seven band keys of a phash: bits [9i, 9i+9) for i in 0..6, and
/// [54, 64) for the seventh, each tagged with its band index in the high bits
/// so equal bit patterns in different bands never collide.
///
/// Why seven: two hashes within Hamming distance d differ in at most d bands,
/// so with seven bands any pair at d <= 6 agrees exactly on at least one band.
/// 6 is the radius at which a phash match is acted on (banned block radius and
/// image ownership credit radius, see banned.rs and ownership.rs), so the band
/// lookup finds every pair that can be acted on — recall there is a theorem,
/// not a measurement. The wider review band (7..=12) is found whenever its
/// differing bits leave one band intact, which re-encodes and resizes, whose
/// flipped bits scatter, almost always do; that band is best-effort by design.
/// Nine bits per band keeps each posting list near 1/512 of the index.
pub fn phash_bands(h: i64) -> [i32; PHASH_BANDS] {
    let u = h as u64;
    let mut out = [0i32; PHASH_BANDS];
    for (i, slot) in out.iter_mut().enumerate() {
        let (shift, width) = if i < 6 { (9 * i as u32, 9u32) } else { (54u32, 10u32) };
        let bits = ((u >> shift) & ((1u64 << width) - 1)) as i32;
        *slot = ((i as i32) << 16) | bits;
    }
    out
}

// ── chromaprint ──────────────────────────────────────────────────────────────

/// Seconds per decoded chromaprint frame (algorithm 1/2: 11025 Hz input,
/// 4096-sample frame, 1365-sample step).
pub const CHROMA_FRAME_SECONDS: f64 = 0.1238;

/// Frames slid in each direction when no alignment is known: ~39.6 s.
const CHROMA_MAX_OFFSET_FRAMES: usize = 320;
/// Frames compared once aligned: ~31.7 s, 8192 bits of evidence.
const CHROMA_COMPARE_FRAMES: usize = 256;
/// Below this many common frames a score stops being evidence (measured null
/// maxima: 0.875 at 8 frames, 0.721 at 64 — content-scan's campaign).
pub const CHROMA_MIN_OVERLAP_FRAMES: usize = 64;
/// Silence, tones and broadband noise sit at or below 0.419; music and speech
/// at or above 0.640. The floor sits in the empty gap.
const CHROMA_MIN_BIT_VARIANCE: f64 = 0.50;

/// chromaprint emits URL-safe base64 without padding; accept it with or
/// without, and tolerate non-zero trailing bits rather than refuse a value
/// the producer considers canonical.
const CHROMA_B64: GeneralPurpose = GeneralPurpose::new(
    &alphabet::URL_SAFE,
    GeneralPurposeConfig::new()
        .with_decode_padding_mode(DecodePaddingMode::Indifferent)
        .with_decode_allow_trailing_bits(true),
);

/// An LSB-first bit reader, the packing chromaprint's compressor uses for
/// both its 3-bit and 5-bit arrays.
struct Bits<'a> {
    data: &'a [u8],
    pos: usize,
}

impl<'a> Bits<'a> {
    fn read(&mut self, width: usize) -> Option<u32> {
        if self.pos + width > self.data.len() * 8 {
            return None;
        }
        let mut v = 0u32;
        for i in 0..width {
            let bit = self.pos + i;
            if (self.data[bit / 8] >> (bit % 8)) & 1 == 1 {
                v |= 1 << i;
            }
        }
        self.pos += width;
        Some(v)
    }
}

/// Decodes chromaprint's compressed fingerprint into its 32-bit frames — the
/// inverse of libchromaprint's FingerprintCompressor:
///
///   byte 0      algorithm
///   bytes 1..4  frame count, big-endian 24-bit
///   then        3-bit "normal" values, LSB-first: for each frame, the
///               1-based gaps between set bits of (frame XOR previous frame),
///               terminated by 0; a value of 7 continues in the exception array
///   then        5-bit exception values, one per normal value equal to 7
pub fn decode_chromaprint(fp: &str) -> Result<Vec<u32>, String> {
    let raw = CHROMA_B64
        .decode(fp.trim().as_bytes())
        .map_err(|e| format!("chromaprint base64 does not decode: {e}"))?;
    if raw.len() < 4 {
        return Err("chromaprint shorter than its header".into());
    }
    let count = ((raw[1] as usize) << 16) | ((raw[2] as usize) << 8) | raw[3] as usize;
    if count == 0 {
        return Err("chromaprint holds zero frames".into());
    }
    let body = &raw[4..];

    let mut normal: Vec<u32> = Vec::new();
    let mut reader = Bits { data: body, pos: 0 };
    let mut terminators = 0usize;
    let mut exceptions_needed = 0usize;
    while terminators < count {
        let v = reader
            .read(3)
            .ok_or_else(|| format!("chromaprint truncated after {terminators} of {count} frames"))?;
        if v == 0 {
            terminators += 1;
        } else if v == 7 {
            exceptions_needed += 1;
        }
        normal.push(v);
    }
    // The exception array starts at the byte after the packed 3-bit array.
    let normal_bytes = (normal.len() * 3 + 7) / 8;
    let mut exc = Bits { data: &body[normal_bytes.min(body.len())..], pos: 0 };
    if exceptions_needed > 0 {
        for v in normal.iter_mut().filter(|v| **v == 7) {
            let extra = exc.read(5).ok_or("chromaprint exception array truncated")?;
            *v += extra;
        }
    }

    let mut frames: Vec<u32> = Vec::with_capacity(count);
    let mut value: u32 = 0;
    let mut last_bit: u32 = 0;
    for v in normal {
        if v == 0 {
            let prev = frames.last().copied().unwrap_or(0);
            frames.push(if frames.is_empty() { value } else { value ^ prev });
            value = 0;
            last_bit = 0;
            continue;
        }
        let bit = last_bit + v;
        if bit == 0 || bit > 32 {
            return Err(format!("chromaprint names bit {bit}, outside a 32-bit frame"));
        }
        value |= 1 << (bit - 1);
        last_bit = bit;
    }
    Ok(frames)
}

/// Mean over the 32 bit positions of 4·p·(1−p): 1.0 when every position is an
/// even split over time, 0.0 for a constant fingerprint.
pub fn chroma_bit_variance(frames: &[u32]) -> f64 {
    if frames.is_empty() {
        return 0.0;
    }
    let n = frames.len() as f64;
    let mut total = 0.0;
    for bit in 0..32 {
        let ones = frames.iter().filter(|f| (*f >> bit) & 1 == 1).count() as f64 / n;
        total += 4.0 * ones * (1.0 - ones);
    }
    total / 32.0
}

/// Whether a fingerprint carries enough information to be compared at all.
pub fn chroma_usable(frames: &[u32]) -> Result<(), String> {
    if frames.len() < CHROMA_MIN_OVERLAP_FRAMES {
        return Err(format!(
            "only {} frames (~{:.1}s), minimum {}",
            frames.len(),
            frames.len() as f64 * CHROMA_FRAME_SECONDS,
            CHROMA_MIN_OVERLAP_FRAMES
        ));
    }
    let v = chroma_bit_variance(frames);
    if v < CHROMA_MIN_BIT_VARIANCE {
        return Err(format!(
            "bit variance {v:.3} below {CHROMA_MIN_BIT_VARIANCE:.2} — silence, a tone or noise"
        ));
    }
    Ok(())
}

fn mismatched_bits(a: &[u32], b: &[u32]) -> u64 {
    a.iter().zip(b).map(|(x, y)| (x ^ y).count_ones() as u64).sum()
}

/// Fraction of matching bits at the best alignment within ±39.6 s, or None
/// when the two share too few frames to be evidence. Both directions are slid
/// so the offset may be positive or negative.
pub fn chroma_similarity(a: &[u32], b: &[u32]) -> Option<f64> {
    let overlap = a.len().min(b.len()).min(CHROMA_COMPARE_FRAMES);
    if overlap < CHROMA_MIN_OVERLAP_FRAMES {
        return None;
    }
    let mut best = 0.0f64;
    for (slid, fixed) in [(a, b), (b, a)] {
        let span = CHROMA_MAX_OFFSET_FRAMES.min(slid.len() - overlap);
        let mut least = u64::MAX;
        for off in 0..=span {
            least = least.min(mismatched_bits(&slid[off..off + overlap], &fixed[..overlap]));
        }
        best = best.max(1.0 - least as f64 / (32.0 * overlap as f64));
    }
    Some(best)
}

/// Similarity with a known alignment: `a` frame `a_start` lines up with `b`
/// frame `b_start`, give or take `slack` frames. Used to verify a landmark
/// alignment independently, including an excerpt hours into a long upload,
/// which the unaligned ±39.6 s search cannot reach.
pub fn chroma_similarity_at(a: &[u32], b: &[u32], a_start: usize, b_start: usize, slack: usize) -> Option<f64> {
    let mut best: Option<f64> = None;
    for delta in -(slack as i64)..=(slack as i64) {
        let a0 = a_start as i64 + delta;
        if a0 < 0 || a0 as usize >= a.len() || b_start >= b.len() {
            continue;
        }
        let a0 = a0 as usize;
        let overlap = (a.len() - a0).min(b.len() - b_start).min(CHROMA_COMPARE_FRAMES);
        if overlap < CHROMA_MIN_OVERLAP_FRAMES {
            continue;
        }
        let m = mismatched_bits(&a[a0..a0 + overlap], &b[b_start..b_start + overlap]);
        let s = 1.0 - m as f64 / (32.0 * overlap as f64);
        best = Some(best.map_or(s, |x: f64| x.max(s)));
    }
    best
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The compressor, written from libchromaprint's FingerprintCompressor, so
    /// the decoder is tested against the exact inverse it claims to be.
    fn compress(frames: &[u32], algorithm: u8) -> String {
        let mut normal: Vec<u32> = Vec::new();
        for (i, f) in frames.iter().enumerate() {
            let mut x = if i == 0 { *f } else { f ^ frames[i - 1] };
            let (mut bit, mut last) = (1u32, 0u32);
            while x != 0 {
                if x & 1 == 1 {
                    normal.push(bit - last);
                    last = bit;
                }
                x >>= 1;
                bit += 1;
            }
            normal.push(0);
        }
        let exceptions: Vec<u32> = normal.iter().filter(|v| **v >= 7).map(|v| v - 7).collect();
        let pack = |vals: &[u32], width: usize| -> Vec<u8> {
            let mut out = vec![0u8; (vals.len() * width + 7) / 8];
            for (i, v) in vals.iter().enumerate() {
                for b in 0..width {
                    if (v >> b) & 1 == 1 {
                        let pos = i * width + b;
                        out[pos / 8] |= 1 << (pos % 8);
                    }
                }
            }
            out
        };
        let clipped: Vec<u32> = normal.iter().map(|v| (*v).min(7)).collect();
        let n = frames.len();
        let mut raw = vec![algorithm, (n >> 16) as u8, (n >> 8) as u8, n as u8];
        raw.extend(pack(&clipped, 3));
        raw.extend(pack(&exceptions, 5));
        base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(raw)
    }

    #[test]
    fn chromaprint_round_trips() {
        let frames: Vec<u32> = (0..300u32).map(|i| i.wrapping_mul(2654435761) ^ (i << 7)).collect();
        let decoded = decode_chromaprint(&compress(&frames, 1)).expect("decodes");
        assert_eq!(decoded, frames);
    }

    #[test]
    fn identical_audio_scores_one_and_shifted_audio_is_found() {
        let frames: Vec<u32> = (0..400u32).map(|i| i.wrapping_mul(2654435761)).collect();
        assert_eq!(chroma_similarity(&frames, &frames), Some(1.0));
        let shifted = &frames[37..];
        assert_eq!(chroma_similarity(&frames, shifted), Some(1.0));
        assert_eq!(chroma_similarity_at(&frames, shifted, 37, 0, 2), Some(1.0));
    }

    #[test]
    fn phash_bands_find_every_pair_within_six_bits() {
        let a: i64 = 0x5a5a_1234_9876_f0f0u64 as i64;
        // Flip one bit in each of six different bands: one band must survive.
        let b = a ^ (1 << 0) ^ (1 << 10) ^ (1 << 20) ^ (1 << 30) ^ (1 << 40) ^ (1 << 50);
        assert_eq!(hamming(a, b), 6);
        let (ba, bb) = (phash_bands(a), phash_bands(b));
        assert!(ba.iter().zip(bb.iter()).any(|(x, y)| x == y));
        assert_eq!(parse_phash_hex(&phash_hex(a)), Some(a));
    }
}
