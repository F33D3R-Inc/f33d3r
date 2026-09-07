import SwiftUI
import AVFoundation
import Observation
import F33D3RKit

/// Recording a voice note for the composer.
///
/// The web's `compose-voice-recorder`: a timer, a live level meter, a stop
/// button; then a preview to listen back to, and the choice to use it or
/// record again. What the sheet hands back is a file and a length — the length
/// is read off the file with AVFoundation rather than off a clock, because the
/// server does not measure audio and the number the work carries has to be the
/// recording's, not the time the button was held.
///
/// The recording is AAC in an M4A container. That is the one format an
/// `AVAudioRecorder` writes that the server's sniff recognises by its bytes
/// (the `ftypM4A ` brand), so the file is admitted for what it is whatever it
/// is called.
///
/// Permission is asked for on the first Record, not on open, and a refusal is
/// a message on the screen with the way to Settings — never a button that does
/// nothing. A Simulator has no microphone; the recorder reports that too.
struct VoiceRecorderSheet: View {
    /// Called with the recording and its length in whole seconds. The sheet
    /// closes itself afterwards.
    let use: (URL, Int) -> Void

    @Environment(\.dismiss) private var dismiss
    @State private var recorder = VoiceRecorder()

    var body: some View {
        NavigationStack {
            VStack(spacing: F33Spacing.xl) {
                Spacer(minLength: 0)

                Text(DurationClock.label(recorder.elapsedSeconds))
                    .font(.system(size: 54, weight: .light).monospacedDigit())
                    .foregroundStyle(F33Color.ink)
                    .accessibilityLabel(elapsedLabel)

                LevelMeter(levels: recorder.levels, isLive: recorder.phase == .recording)
                    .frame(height: 56)
                    .padding(.horizontal, F33Spacing.xl)

                statusLine

                Spacer(minLength: 0)

                controls
            }
            .padding(.horizontal, F33Spacing.xl)
            .padding(.bottom, F33Spacing.xl)
            .background(F33Color.bg)
            .navigationTitle("Voice note")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") {
                        recorder.discard()
                        dismiss()
                    }
                    .foregroundStyle(F33Color.ink2)
                }
            }
        }
        .presentationDetents([.medium, .large])
        .presentationDragIndicator(.visible)
        .interactiveDismissDisabled(recorder.phase == .recording)
        .onDisappear { recorder.stopEverything() }
    }

    @ViewBuilder
    private var statusLine: some View {
        if let problem = recorder.problem {
            VStack(spacing: F33Spacing.sm) {
                Text(problem)
                    .font(.footnote)
                    .foregroundStyle(F33Color.danger)
                    .multilineTextAlignment(.center)
                    .fixedSize(horizontal: false, vertical: true)
                if recorder.isPermissionDenied, let settings = URL(string: UIApplication.openSettingsURLString) {
                    Link("Open Settings", destination: settings)
                        .font(.footnote.weight(.semibold))
                        .foregroundStyle(F33Color.accent)
                        .frame(minHeight: F33Layout.minTouchTarget)
                }
            }
        } else {
            Text(hint)
                .font(.footnote)
                .foregroundStyle(F33Color.ink4)
                .multilineTextAlignment(.center)
        }
    }

    private var hint: String {
        switch recorder.phase {
        case .idle: return "Tap to start recording."
        case .recording: return "Recording. Tap to stop."
        case .measuring: return "Reading the recording…"
        case .recorded: return "Listen back, then use it or record again."
        }
    }

    private var elapsedLabel: String {
        let seconds = recorder.elapsedSeconds
        return "\(seconds / 60) minutes \(seconds % 60) seconds"
    }

    @ViewBuilder
    private var controls: some View {
        switch recorder.phase {
        case .idle, .recording:
            Button {
                Task { await recorder.toggleRecording() }
            } label: {
                ZStack {
                    Circle()
                        .strokeBorder(F33Color.hairlineStrong, lineWidth: 3)
                        .frame(width: 76, height: 76)
                    if recorder.phase == .recording {
                        RoundedRectangle(cornerRadius: 6)
                            .fill(F33Color.danger)
                            .frame(width: 30, height: 30)
                    } else {
                        Circle()
                            .fill(F33Color.danger)
                            .frame(width: 60, height: 60)
                    }
                }
                .contentShape(Circle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel(recorder.phase == .recording ? "Stop recording" : "Start recording")

        case .measuring:
            // The file is being read for its length. The Record button is
            // withheld rather than shown: a tap here would start a second
            // recording under the first one's verdict.
            ProgressView()
                .tint(F33Color.accent)
                .frame(width: 76, height: 76)
                .accessibilityLabel("Reading the recording")

        case .recorded(let url, let seconds):
            VStack(spacing: F33Spacing.md) {
                HStack(spacing: F33Spacing.lg) {
                    Button {
                        recorder.togglePreview()
                    } label: {
                        Label(recorder.isPreviewing ? "Pause" : "Play", systemImage: recorder.isPreviewing ? "pause.fill" : "play.fill")
                            .font(.system(size: 15, weight: .semibold))
                            .foregroundStyle(F33Color.accent)
                            .padding(.horizontal, F33Spacing.lg)
                            .frame(height: 36)
                            .f33Glass(in: Capsule(), interactive: true)
                            .frame(minHeight: F33Layout.minTouchTarget)
                            .contentShape(Capsule())
                    }
                    .buttonStyle(.plain)

                    Button {
                        recorder.discard()
                    } label: {
                        Label("Record again", systemImage: "arrow.counterclockwise")
                            .font(.system(size: 15, weight: .semibold))
                            .foregroundStyle(F33Color.ink2)
                            .padding(.horizontal, F33Spacing.lg)
                            .frame(height: 36)
                            .f33Glass(in: Capsule(), interactive: true)
                            .frame(minHeight: F33Layout.minTouchTarget)
                            .contentShape(Capsule())
                    }
                    .buttonStyle(.plain)
                }

                Button("Use this recording") {
                    recorder.stopEverything()
                    use(url, seconds)
                    dismiss()
                }
                .buttonStyle(F33PrimaryButtonStyle())
            }
        }
    }
}

/// The recorder behind the sheet: the microphone, the file, the meter.
///
/// A class the view observes rather than state in the view, because the
/// recording outlives any one body evaluation and the `AVAudioRecorder` and
/// `AVAudioPlayer` have to be held by something that is not rebuilt.
@MainActor
@Observable
final class VoiceRecorder {
    enum Phase: Equatable {
        case idle
        case recording
        /// Stopped; the file is being read for its length. Neither button
        /// applies until the read has an answer.
        case measuring
        /// The file and its length in whole seconds, as AVFoundation read it.
        case recorded(URL, Int)
    }

    private(set) var phase: Phase = .idle
    private(set) var elapsedSeconds = 0
    /// The last few dozen levels, newest last, each 0…1. The meter draws them
    /// as bars — the web's 24 — so the shape of the last second is visible,
    /// not just the loudness of this instant.
    private(set) var levels: [Double] = Array(repeating: 0, count: 24)
    private(set) var problem: String?
    private(set) var isPermissionDenied = false
    private(set) var isPreviewing = false

    @ObservationIgnored private var recorder: AVAudioRecorder?
    @ObservationIgnored private var player: AVAudioPlayer?
    @ObservationIgnored private var meterTask: Task<Void, Never>?
    @ObservationIgnored private var playbackWatch: Task<Void, Never>?
    @ObservationIgnored private var startedAt: Date?

    /// Below this a tap on Record and Stop is a mistake, not a voice note,
    /// and the payload's whole-second length would be zero.
    static let minimumSeconds = 1

    func toggleRecording() async {
        switch phase {
        case .recording: stopRecording()
        case .measuring: return
        case .idle, .recorded: await startRecording()
        }
    }

    private func startRecording() async {
        problem = nil
        isPermissionDenied = false
        guard await requestMicrophone() else {
            isPermissionDenied = true
            problem = "F33D3R can't use the microphone. Allow it in Settings to record a voice note."
            return
        }

        let session = AVAudioSession.sharedInstance()
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("f33d3r-voice-\(UUID().uuidString).m4a", isDirectory: false)
        do {
            try session.setCategory(.playAndRecord, mode: .spokenAudio, options: [.defaultToSpeaker])
            try session.setActive(true)
            // AAC in M4A: the container the server's sniff knows by its bytes.
            let settings: [String: Any] = [
                AVFormatIDKey: Int(kAudioFormatMPEG4AAC),
                AVSampleRateKey: 44_100,
                AVNumberOfChannelsKey: 1,
                AVEncoderAudioQualityKey: AVAudioQuality.high.rawValue,
            ]
            let recorder = try AVAudioRecorder(url: url, settings: settings)
            recorder.isMeteringEnabled = true
            guard recorder.record() else {
                // `record()` says no without a reason on a device with no
                // input route — a Simulator, most often.
                problem = "Recording couldn't start. This device has no microphone input right now."
                try? session.setActive(false, options: .notifyOthersOnDeactivation)
                return
            }
            self.recorder = recorder
            startedAt = Date()
            elapsedSeconds = 0
            levels = Array(repeating: 0, count: 24)
            phase = .recording
            startMetering()
        } catch {
            problem = "Recording couldn't start: \(error.localizedDescription)"
        }
    }

    private func stopRecording() {
        guard let recorder else { return }
        meterTask?.cancel()
        meterTask = nil
        recorder.stop()
        self.recorder = nil
        let url = recorder.url
        try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
        phase = .measuring
        Task { await measure(url) }
    }

    /// Reads the length off the finished file. This, and not the timer, is the
    /// number the work carries.
    private func measure(_ url: URL) async {
        do {
            let duration = try await AVURLAsset(url: url).load(.duration)
            // Cancelled while the file was being read: the sheet is gone, or
            // the note was thrown away. The file goes with it.
            guard phase == .measuring else {
                try? FileManager.default.removeItem(at: url)
                return
            }
            let seconds = Int(CMTimeGetSeconds(duration).rounded())
            guard seconds >= Self.minimumSeconds else {
                try? FileManager.default.removeItem(at: url)
                problem = "That was too short to be a voice note. Hold Record a little longer."
                phase = .idle
                elapsedSeconds = 0
                return
            }
            elapsedSeconds = seconds
            phase = .recorded(url, seconds)
        } catch {
            try? FileManager.default.removeItem(at: url)
            problem = "The recording couldn't be read back: \(error.localizedDescription)"
            phase = .idle
            elapsedSeconds = 0
        }
    }

    private func startMetering() {
        meterTask = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: .milliseconds(50))
                guard let self, let recorder = self.recorder, recorder.isRecording else { return }
                recorder.updateMeters()
                // Average power is in decibels, silence around -160 and full
                // scale at 0; the usable range of a voice is the top fifty.
                let db = recorder.averagePower(forChannel: 0)
                let level = max(0, min(1, Double(db + 50) / 50))
                self.levels.removeFirst()
                self.levels.append(level)
                if let startedAt = self.startedAt {
                    self.elapsedSeconds = Int(Date().timeIntervalSince(startedAt))
                }
            }
        }
    }

    func togglePreview() {
        guard case .recorded(let url, _) = phase else { return }
        if isPreviewing {
            player?.pause()
            isPreviewing = false
            playbackWatch?.cancel()
            return
        }
        do {
            if player == nil {
                let session = AVAudioSession.sharedInstance()
                try session.setCategory(.playback, mode: .spokenAudio)
                try session.setActive(true)
                player = try AVAudioPlayer(contentsOf: url)
            }
            guard let player, player.play() else {
                problem = "Playback couldn't start."
                return
            }
            isPreviewing = true
            playbackWatch?.cancel()
            playbackWatch = Task { [weak self] in
                while !Task.isCancelled {
                    try? await Task.sleep(for: .milliseconds(200))
                    guard let self else { return }
                    if self.player?.isPlaying != true {
                        self.isPreviewing = false
                        return
                    }
                }
            }
        } catch {
            problem = "Playback couldn't start: \(error.localizedDescription)"
        }
    }

    /// Throws the recording away and goes back to the start.
    func discard() {
        stopEverything()
        if case .recorded(let url, _) = phase {
            try? FileManager.default.removeItem(at: url)
        }
        phase = .idle
        elapsedSeconds = 0
        levels = Array(repeating: 0, count: 24)
        problem = nil
    }

    /// Stops the microphone and the speaker without touching the file.
    func stopEverything() {
        meterTask?.cancel()
        meterTask = nil
        playbackWatch?.cancel()
        playbackWatch = nil
        if let recorder {
            recorder.stop()
            self.recorder = nil
        }
        player?.stop()
        player = nil
        isPreviewing = false
        if phase == .recording || phase == .measuring { phase = .idle }
        try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
    }

    private func requestMicrophone() async -> Bool {
        switch AVAudioApplication.shared.recordPermission {
        case .granted: return true
        case .denied: return false
        case .undetermined: return await AVAudioApplication.requestRecordPermission()
        @unknown default: return false
        }
    }
}

/// Twenty-four bars, the web's `cvr-waveform`, drawn from the recorder's
/// recent levels. Still bars at rest; a live meter while recording.
private struct LevelMeter: View {
    let levels: [Double]
    let isLive: Bool

    var body: some View {
        HStack(alignment: .center, spacing: 3) {
            ForEach(levels.indices, id: \.self) { index in
                RoundedRectangle(cornerRadius: 2)
                    .fill(isLive ? F33Color.accent : F33Color.ink5)
                    .frame(width: 6, height: max(4, levels[index] * 52))
            }
        }
        .frame(maxWidth: .infinity)
        .animation(.linear(duration: 0.05), value: levels)
        .accessibilityHidden(true)
    }
}
