//! Time-alignment voting: the step that turns "these two assets share hashes"
//! into "this stretch of one IS that stretch of the other".
//!
//! Every index hit is a pair (t_query, t_reference). If the query contains the
//! reference's material, the true pairs satisfy
//!
//! ```text
//!     t_query = scale · t_reference + offset
//! ```
//!
//! for one (scale, offset): scale = 1 for a plain copy, an excerpt or a trim;
//! scale ≠ 1 when the speed was changed. Spurious hits — shared hashes between
//! unrelated media — scatter uniformly over offsets. So the match is a peak in
//! a histogram of offsets, and its height is the evidence. This is the Shazam
//! histogram vote; searching a grid of scales (a Hough transform over the
//! line's slope) extends it to speed changes.
//!
//! An excerpt anywhere inside a long upload produces the same peak as a full
//! copy, just with fewer votes, and the peak's position IS the alignment
//! offset — which is why the same voter answers exact, derivative and excerpt.

use std::collections::HashMap;

/// One index hit. `qid` identifies the query item (a landmark or a segment) so
/// one query item is counted once per alignment however many reference items
/// it hit: repetitive material (a loop, a held note) must not vote many times.
#[derive(Debug, Clone, Copy)]
pub struct Pair {
    pub tq: f64,
    pub tr: f64,
    pub qid: u32,
}

/// The winning line through a candidate's hits.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Alignment {
    pub scale: f64,
    /// Seconds: the reference's t = 0 lands at `offset` in the query.
    pub offset: f64,
    /// Distinct query items on the line.
    pub aligned: usize,
    pub q_start: f64,
    pub q_end: f64,
    pub r_start: f64,
    pub r_end: f64,
}

impl Alignment {
    pub fn q_span(&self) -> f64 {
        (self.q_end - self.q_start).max(0.0)
    }
    pub fn r_span(&self) -> f64 {
        (self.r_end - self.r_start).max(0.0)
    }
    /// True when the match needed a scale other than 1 — a speed change.
    pub fn rescaled(&self) -> bool {
        (self.scale - 1.0).abs() > 0.01
    }
}

/// Plain copies, trims and excerpts: no speed change searched.
pub const UNIT_SCALE: &[f64] = &[1.0];

/// Scales searched for speed-changed media: 0.70× to 1.40× (the range of
/// "sped up" and "slowed + reverb" re-uploads), in geometric steps of 0.3%.
///
/// The step is set by the bin: a scale error δ drifts the offset by δ·T over a
/// span of T seconds, and the peak stays in one two-bin window while δ·T is
/// under the bin width. At 0.3% steps the worst error is 0.15%, which holds a
/// 100 ms bin for ~65 s of continuous match — longer matches still peak, over
/// their best 65 s, which is far more evidence than any threshold asks for.
pub fn speed_scales() -> Vec<f64> {
    let mut out = Vec::new();
    let step = 1.003f64;
    let mut s = 1.0f64;
    while s >= 0.70 {
        out.push(s);
        s /= step;
    }
    let mut s = 1.0f64 * step;
    while s <= 1.40 {
        out.push(s);
        s *= step;
    }
    out
}

/// The best line through `pairs` over the given scales, binning offsets at
/// `bin` seconds and taking the densest window of two adjacent bins (a true
/// offset that falls on a bin edge splits its votes; the two-bin window takes
/// them back).
pub fn best_alignment(pairs: &[Pair], scales: &[f64], bin: f64) -> Option<Alignment> {
    if pairs.is_empty() || bin <= 0.0 {
        return None;
    }
    let mut best: Option<(usize, f64, i64)> = None; // (raw votes, scale, bin)
    let mut counts: HashMap<i64, u32> = HashMap::with_capacity(pairs.len());
    for &scale in scales {
        counts.clear();
        for p in pairs {
            let off = p.tq - scale * p.tr;
            *counts.entry((off / bin).floor() as i64).or_insert(0) += 1;
        }
        for (&b, &n) in &counts {
            let window = n as usize + counts.get(&(b + 1)).copied().unwrap_or(0) as usize;
            let better = match best {
                None => true,
                // Prefer more votes; on a tie prefer the scale nearer 1 (the
                // simpler explanation).
                Some((votes, s, _)) => {
                    window > votes || (window == votes && (scale - 1.0).abs() < (s - 1.0).abs())
                }
            };
            if better {
                best = Some((window, scale, b));
            }
        }
    }
    let (_, scale, b) = best?;

    // Re-walk the winning window to count distinct query items and measure
    // the spans on both sides.
    let lo = b as f64 * bin;
    let hi = lo + 2.0 * bin;
    let mut seen: HashMap<u32, ()> = HashMap::new();
    let (mut q0, mut q1, mut r0, mut r1) = (f64::MAX, f64::MIN, f64::MAX, f64::MIN);
    let mut sum_off = 0.0;
    let mut n = 0usize;
    for p in pairs {
        let off = p.tq - scale * p.tr;
        if off < lo || off >= hi {
            continue;
        }
        sum_off += off;
        n += 1;
        q0 = q0.min(p.tq);
        q1 = q1.max(p.tq);
        r0 = r0.min(p.tr);
        r1 = r1.max(p.tr);
        seen.insert(p.qid, ());
    }
    if n == 0 {
        return None;
    }
    Some(Alignment {
        scale,
        offset: sum_off / n as f64,
        aligned: seen.len(),
        q_start: q0,
        q_end: q1,
        r_start: r0,
        r_end: r1,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn line(scale: f64, offset: f64, n: u32, from: u32) -> Vec<Pair> {
        (0..n)
            .map(|i| {
                let tr = (from + i) as f64 * 0.5;
                Pair { tq: scale * tr + offset, tr, qid: from + i }
            })
            .collect()
    }

    #[test]
    fn finds_an_excerpt_and_its_offset() {
        let mut pairs = line(1.0, 125.0, 40, 0);
        // Noise: hits scattered over unrelated offsets.
        for i in 0..200u32 {
            pairs.push(Pair { tq: (i * 37 % 900) as f64, tr: (i * 11 % 300) as f64, qid: 1000 + i });
        }
        let a = best_alignment(&pairs, UNIT_SCALE, 0.1).expect("aligned");
        assert!(a.aligned >= 40);
        assert!((a.offset - 125.0).abs() < 0.2);
    }

    #[test]
    fn finds_a_speed_change() {
        let pairs = line(1.25, 3.0, 60, 0);
        let a = best_alignment(&pairs, &speed_scales(), 0.1).expect("aligned");
        assert!(a.rescaled());
        assert!((a.scale - 1.25).abs() < 0.01);
        assert!(a.aligned >= 50);
    }
}
