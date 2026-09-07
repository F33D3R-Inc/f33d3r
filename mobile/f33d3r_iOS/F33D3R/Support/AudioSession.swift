import AVFoundation
import os

/// The app's audio session, put into a state that plays sound out loud.
///
/// The session starts in the ambient category, which is right for a feed of
/// muted autoplaying clips: they make no sound, and they do not interrupt
/// whatever the reader has playing. The moment something asks for sound —
/// unmuting a tile, opening the full-screen player — the session moves to
/// `.playback`, so the video is heard with the ring switch off, keeps going
/// when the screen locks or the app goes behind Picture in Picture, and takes
/// the audio route the way a video app does.
///
/// **This is asserted on every request, not once per launch.** It used to latch
/// on a `isConfigured` flag, which is wrong for the one thing an audio session
/// does on its own: iOS deactivates it during a phone call, an alarm, or when
/// another app takes the route, and the only way back is to activate it again.
/// With the latch, the first interruption of the app's life left every later
/// video silent, with the mute button showing sound was on. `setCategory` and
/// `setActive` are both cheap and both idempotent, so there is nothing to save
/// by skipping them and a whole class of silence to avoid by not.
///
/// ``MusicPlayer`` owns its own session for the same reason — it needs
/// `.longFormAudio`, which puts a track on the lock screen — and re-asserts it
/// on every `resume()`. Two callers, one rule: whoever is about to make sound
/// says so first.
@MainActor
enum AudioSession {
    private static let log = Logger(subsystem: "com.f33d3r.ios", category: "audio")

    /// Makes the session one that plays video sound, and makes it active.
    ///
    /// Call immediately before sound is expected: on unmute, on going full
    /// screen, and when a player starts already unmuted. A failure is logged
    /// with the reason rather than swallowed — silent video with no explanation
    /// is the hardest kind of bug to be told about.
    static func activateForPlayback() {
        let session = AVAudioSession.sharedInstance()
        do {
            // Only when it differs: setting the category redundantly can
            // interrupt audio that is already playing under it.
            if session.category != .playback || session.mode != .moviePlayback {
                try session.setCategory(.playback, mode: .moviePlayback)
            }
            try session.setActive(true)
        } catch {
            log.error("Audio session could not be activated for playback: \(error.localizedDescription, privacy: .public)")
        }
    }
}
