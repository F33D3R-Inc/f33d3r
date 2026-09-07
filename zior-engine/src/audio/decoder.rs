//! Audio decoding — converts uploaded bytes into normalised mono PCM samples.
//!
//! Supports: WAV, MP3, FLAC, OGG, AAC (via Symphonia).
//! Output: mono f32 PCM. Caller is responsible for resampling.

use anyhow::{anyhow, Result};
use std::io::Cursor;
use symphonia::core::audio::{AudioBufferRef, Signal};
use symphonia::core::codecs::DecoderOptions;
use symphonia::core::formats::FormatOptions;
use symphonia::core::io::MediaSourceStream;
use symphonia::core::meta::MetadataOptions;
use symphonia::core::probe::Hint;

pub struct DecodedAudio {
    /// Mono PCM samples, normalised to [-1.0, 1.0]
    pub samples: Vec<f32>,
    /// Native sample rate from the file header
    pub sample_rate: u32,
    /// Original channel count
    pub channels: usize,
    /// Duration in seconds
    pub duration_secs: f64,
}

/// Decode raw audio bytes into mono PCM.
/// Averages all channels to produce mono output.
pub fn decode_audio(bytes: &[u8], hint_ext: Option<&str>) -> Result<DecodedAudio> {
    let cursor = Cursor::new(bytes.to_vec());
    let mss = MediaSourceStream::new(Box::new(cursor), Default::default());

    let mut hint = Hint::new();
    if let Some(ext) = hint_ext {
        hint.with_extension(ext);
    }

    let probed = symphonia::default::get_probe()
        .format(
            &hint,
            mss,
            &FormatOptions::default(),
            &MetadataOptions::default(),
        )
        .map_err(|e| anyhow!("audio probe failed: {}", e))?;

    let mut format = probed.format;
    let track = format
        .default_track()
        .ok_or_else(|| anyhow!("no audio track found"))?;

    let sample_rate = track
        .codec_params
        .sample_rate
        .ok_or_else(|| anyhow!("unknown sample rate"))?;
    let channels = track.codec_params.channels.map(|c| c.count()).unwrap_or(1);
    let track_id = track.id;

    let mut decoder = symphonia::default::get_codecs()
        .make(&track.codec_params, &DecoderOptions::default())
        .map_err(|e| anyhow!("codec init: {}", e))?;

    let mut all_samples: Vec<f32> = Vec::new();

    loop {
        let packet = match format.next_packet() {
            Ok(p) => p,
            Err(_) => break,
        };
        if packet.track_id() != track_id {
            continue;
        }

        let decoded = match decoder.decode(&packet) {
            Ok(d) => d,
            Err(_) => continue,
        };

        match decoded {
            AudioBufferRef::F32(buf) => {
                for frame in 0..buf.frames() {
                    let mut sum = 0.0_f32;
                    for ch in 0..channels {
                        sum += buf.chan(ch)[frame];
                    }
                    all_samples.push(sum / channels as f32);
                }
            }
            AudioBufferRef::S16(buf) => {
                for frame in 0..buf.frames() {
                    let mut sum = 0.0_f32;
                    for ch in 0..channels {
                        sum += buf.chan(ch)[frame] as f32 / 32768.0;
                    }
                    all_samples.push(sum / channels as f32);
                }
            }
            AudioBufferRef::S32(buf) => {
                for frame in 0..buf.frames() {
                    let mut sum = 0.0_f32;
                    for ch in 0..channels {
                        sum += buf.chan(ch)[frame] as f32 / 2_147_483_648.0;
                    }
                    all_samples.push(sum / channels as f32);
                }
            }
            _ => {
                // Fallback: convert via planar f32
                let spec = *decoded.spec();
                let n_frames = decoded.capacity();
                let mut buf =
                    symphonia::core::audio::AudioBuffer::<f32>::new(n_frames as u64, spec);
                decoded.convert(&mut buf);
                for frame in 0..buf.frames() {
                    let n = buf.spec().channels.count();
                    let avg = (0..n).map(|ch| buf.chan(ch)[frame]).sum::<f32>() / n as f32;
                    all_samples.push(avg);
                }
            }
        }
    }

    if all_samples.is_empty() {
        return Err(anyhow!("decoded audio is empty"));
    }

    let duration_secs = all_samples.len() as f64 / sample_rate as f64;

    Ok(DecodedAudio {
        samples: all_samples,
        sample_rate,
        channels,
        duration_secs,
    })
}

/// Linear downsampler. Averages chunks to reduce sample rate.
pub fn downsample(samples: &[f32], source_rate: u32, target_rate: u32) -> Vec<f32> {
    if source_rate <= target_rate {
        return samples.to_vec();
    }
    let ratio = source_rate as f64 / target_rate as f64;
    let out_len = (samples.len() as f64 / ratio) as usize;
    let mut output = Vec::with_capacity(out_len);
    let mut pos = 0.0_f64;
    while pos + ratio < samples.len() as f64 {
        let start = pos as usize;
        let end = (pos + ratio) as usize;
        let chunk = &samples[start..end.min(samples.len())];
        output.push(chunk.iter().sum::<f32>() / chunk.len() as f32);
        pos += ratio;
    }
    output
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn downsample_halves_length() {
        let input: Vec<f32> = (0..1000).map(|i| i as f32 / 1000.0).collect();
        let out = downsample(&input, 44100, 22050);
        assert!((out.len() as f64 - 500.0).abs() < 5.0, "got {}", out.len());
    }

    #[test]
    fn downsample_same_rate_noop() {
        let input = vec![0.1_f32, 0.2, 0.3];
        assert_eq!(downsample(&input, 22050, 22050), input);
    }
}
