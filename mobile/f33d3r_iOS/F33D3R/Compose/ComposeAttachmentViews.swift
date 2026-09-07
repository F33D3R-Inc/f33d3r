import SwiftUI
import AVFoundation
import CoreTransferable
import UniformTypeIdentifiers
import F33D3RKit

/// The tiles for what the composer can attach besides pictures: the one
/// video, the one voice note, the one GIF.
///
/// Each draws the composer's state for that attachment and nothing else —
/// where the web puts a spinner, a percentage, a "Processing…", a VIDEO badge
/// or a duplicate banner, the tile here puts the same thing, read off
/// `WorkComposer.video`, `.voice` and `.gif`. The choices a state offers
/// (retry, remove, use anyway) call back into the composer; the tile never
/// decides for it.

// MARK: - Video

/// The video on the draft, through every state it passes on its way to the
/// server — see `WorkComposer.VideoAttachment`.
///
/// The poster under the states is the clip's own first frame while the file
/// is on this device, and the transcoder's poster once the video came back
/// from a draft and the file did not. A tile that changed pictures when the
/// transcode finished would look like a different video arrived.
struct VideoAttachmentTile: View {
    let video: WorkComposer.VideoAttachment
    let remove: () -> Void
    let retry: () -> Void
    let useAnyway: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: F33Spacing.sm) {
            ZStack(alignment: .topTrailing) {
                poster
                    .frame(maxWidth: .infinity)
                    .frame(height: 180)
                    .clipShape(RoundedRectangle(cornerRadius: F33Card.mediaCornerRadius))
                    .overlay { stateOverlay }
                    .overlay(alignment: .bottomLeading) { badges }

                RemoveButton(label: "Remove video", action: remove)
            }
            .padding(.top, 6)
            .padding(.trailing, 6)

            notice
        }
        .animation(F33Motion.easeOut, value: video.state)
        .accessibilityElement(children: .contain)
        .accessibilityLabel(accessibilityLabel)
    }

    @ViewBuilder
    private var poster: some View {
        if let fileURL = video.fileURL {
            LocalVideoPoster(fileURL: fileURL, seed: video.filename)
        } else if let path = video.renditions?.posterURL ?? duplicatePosterPath {
            RemoteImage(path: path, seed: video.filename)
        } else {
            posterPlaceholder
        }
    }

    private var duplicatePosterPath: String? {
        if case .duplicate(_, _, let renditions) = video.state { return renditions?.posterURL }
        return nil
    }

    private var posterPlaceholder: some View {
        let (start, end) = AvatarPalette.colors(for: video.filename)
        return LinearGradient(colors: [start.opacity(0.28), end.opacity(0.28)], startPoint: .topLeading, endPoint: .bottomTrailing)
            .overlay(F33Color.bgSunken.opacity(0.35))
    }

    /// What is happening to the video, over the poster. Ready needs nothing
    /// over it; the badge below says what it is.
    @ViewBuilder
    private var stateOverlay: some View {
        switch video.state {
        case .uploading(let fraction):
            scrim {
                VStack(spacing: F33Spacing.sm) {
                    ProgressView(value: fraction)
                        .progressViewStyle(.linear)
                        .tint(.white)
                        .frame(width: 120)
                    Text("Uploading \(Int((fraction * 100).rounded()))%")
                        .font(.system(size: 13, weight: .semibold).monospacedDigit())
                }
            }
        case .processing:
            scrim {
                VStack(spacing: F33Spacing.sm) {
                    ProgressView().tint(.white)
                    Text("Processing…")
                        .font(.system(size: 13, weight: .semibold))
                }
            }
        case .failed(let reason):
            scrim(opacity: 0.6) {
                VStack(spacing: F33Spacing.xs) {
                    Image(systemName: "xmark.circle")
                        .font(.system(size: 22, weight: .semibold))
                    Text(reason)
                        .font(.system(size: 12, weight: .medium))
                        .multilineTextAlignment(.center)
                        .lineLimit(3)
                        .padding(.horizontal, F33Spacing.lg)
                    if video.fileURL != nil {
                        Button(action: retry) {
                            Label("Retry", systemImage: "arrow.clockwise")
                                .font(.system(size: 13, weight: .semibold))
                                .padding(.horizontal, F33Spacing.md)
                                .frame(height: 30)
                                .background(Color.white.opacity(0.18), in: Capsule())
                                .frame(minHeight: F33Layout.minTouchTarget)
                                .contentShape(Capsule())
                        }
                        .buttonStyle(.plain)
                        .accessibilityLabel("Upload failed. Retry")
                    }
                }
            }
        case .ready, .duplicate:
            EmptyView()
        }
    }

    private func scrim<Content: View>(opacity: Double = 0.45, @ViewBuilder _ content: () -> Content) -> some View {
        ZStack {
            Color.black.opacity(opacity)
            content().foregroundStyle(.white)
        }
        .clipShape(RoundedRectangle(cornerRadius: F33Card.mediaCornerRadius))
    }

    /// The web's `VIDEO` corner label, with the length beside it once known.
    @ViewBuilder
    private var badges: some View {
        if case .ready(let renditions) = video.state {
            HStack(spacing: F33Spacing.xs) {
                MediaBadge(text: "VIDEO")
                MediaBadge(text: DurationClock.label(renditions.durationSecs))
                Image(systemName: "checkmark.circle.fill")
                    .font(.system(size: 14))
                    .foregroundStyle(F33Color.ok)
                    .background(Color.white.opacity(0.9), in: Circle())
                    .accessibilityLabel("Ready")
            }
            .padding(F33Spacing.sm)
        } else if case .duplicate = video.state {
            MediaBadge(text: "VIDEO").padding(F33Spacing.sm)
        }
    }

    /// The duplicate banner — the web's `compose-dup-warning` — and what to do
    /// about it. The credit stays with the original whichever way the author
    /// goes, so it stays on screen after "Use anyway" too.
    @ViewBuilder
    private var notice: some View {
        switch video.state {
        case .duplicate(let handle, _, let renditions):
            VStack(alignment: .leading, spacing: F33Spacing.sm) {
                Label {
                    Text(duplicateText(handle, canUse: renditions != nil))
                        .font(.footnote)
                        .fixedSize(horizontal: false, vertical: true)
                } icon: {
                    Image(systemName: "exclamationmark.triangle")
                }
                .foregroundStyle(F33Color.warn)

                HStack(spacing: F33Spacing.sm) {
                    if renditions != nil {
                        Button("Use anyway", action: useAnyway)
                            .font(.system(size: 13, weight: .semibold))
                            .foregroundStyle(F33Color.accentInk)
                            .padding(.horizontal, F33Spacing.md)
                            .frame(height: 30)
                            .background(F33Color.accent, in: Capsule())
                            .frame(minHeight: F33Layout.minTouchTarget)
                            .contentShape(Capsule())
                            .buttonStyle(.plain)
                    }
                    Button("Remove", action: remove)
                        .font(.system(size: 13, weight: .semibold))
                        .foregroundStyle(F33Color.danger)
                        .padding(.horizontal, F33Spacing.md)
                        .frame(height: 30)
                        .f33Glass(in: Capsule(), interactive: true)
                        .frame(minHeight: F33Layout.minTouchTarget)
                        .contentShape(Capsule())
                        .buttonStyle(.plain)
                }
            }
            .transition(.opacity)
        case .ready(let renditions):
            if let handle = renditions.creditedTo {
                Label {
                    Text("Already on F33D3R — @\(handle) keeps the credit.")
                        .font(.footnote)
                        .fixedSize(horizontal: false, vertical: true)
                } icon: {
                    Image(systemName: "exclamationmark.triangle")
                }
                .foregroundStyle(F33Color.warn)
                .transition(.opacity)
            }
        case .uploading, .processing, .failed:
            EmptyView()
        }
    }

    private func duplicateText(_ handle: String, canUse: Bool) -> String {
        let who = handle.isEmpty ? "another creator" : "@\(handle)"
        return canUse
            ? "This video is already on F33D3R — \(who) will keep the credit. Post it anyway, referencing theirs?"
            : "This video is already on F33D3R under \(who), and the server gave nothing to reference. Remove it."
    }

    private var accessibilityLabel: String {
        switch video.state {
        case .uploading(let fraction): return "Video, uploading \(Int((fraction * 100).rounded())) percent"
        case .processing: return "Video, processing"
        case .ready(let renditions): return "Video, \(DurationClock.label(renditions.durationSecs)), ready"
        case .duplicate(let handle, _, _): return "Video already on F33D3R, credited to \(handle.isEmpty ? "another creator" : handle)"
        case .failed(let reason): return "Video failed: \(reason)"
        }
    }
}

/// The clip's first frame, from the file on this device.
///
/// Read once per file through `AVAssetImageGenerator`; a failure to read one
/// — a container the phone can decode but not seek, a file that vanished —
/// leaves the placeholder, since the transcoder's poster is what will be
/// shown from here on anyway.
private struct LocalVideoPoster: View {
    let fileURL: URL
    let seed: String

    @State private var image: UIImage?

    var body: some View {
        Group {
            if let image {
                Color.clear
                    .overlay { Image(uiImage: image).resizable().aspectRatio(contentMode: .fill) }
                    .clipped()
            } else {
                let (start, end) = AvatarPalette.colors(for: seed)
                LinearGradient(colors: [start.opacity(0.28), end.opacity(0.28)], startPoint: .topLeading, endPoint: .bottomTrailing)
                    .overlay(F33Color.bgSunken.opacity(0.35))
            }
        }
        .task(id: fileURL) {
            let generator = AVAssetImageGenerator(asset: AVURLAsset(url: fileURL))
            generator.appliesPreferredTrackTransform = true
            generator.maximumSize = CGSize(width: 800, height: 800)
            guard let (frame, _) = try? await generator.image(at: .zero) else { return }
            guard !Task.isCancelled else { return }
            withAnimation(F33Motion.easeOut) { image = UIImage(cgImage: frame) }
        }
    }
}

// MARK: - Voice

/// The voice note on the draft: what it is, how long, and whether the server
/// has it yet.
struct VoiceAttachmentRow: View {
    let voice: WorkComposer.VoiceAttachment
    let remove: () -> Void
    let retry: () -> Void

    var body: some View {
        HStack(spacing: F33Spacing.sm) {
            Image(systemName: "waveform")
                .font(.system(size: 15, weight: .semibold))
                .foregroundStyle(F33Color.accent)

            VStack(alignment: .leading, spacing: 2) {
                Text("Voice note · \(DurationClock.label(voice.durationSecs))")
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundStyle(F33Color.ink)
                if let failure = voice.failure {
                    Text(failure)
                        .font(.system(size: 12))
                        .foregroundStyle(F33Color.danger)
                        .lineLimit(2)
                } else if voice.isUploading {
                    Text("Uploading…")
                        .font(.system(size: 12))
                        .foregroundStyle(F33Color.ink4)
                }
            }

            Spacer(minLength: 0)

            if voice.isUploading {
                ProgressView().tint(F33Color.accent)
            } else if voice.failure != nil, voice.fileURL != nil {
                Button(action: retry) {
                    Image(systemName: "arrow.clockwise")
                        .font(.system(size: 14, weight: .semibold))
                        .foregroundStyle(F33Color.accent)
                        .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                        .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Upload failed. Retry")
            } else if voice.remotePath != nil {
                Image(systemName: "checkmark.circle.fill")
                    .font(.system(size: 16))
                    .foregroundStyle(F33Color.ok)
                    .accessibilityLabel("Uploaded")
            }

            Button(action: remove) {
                Image(systemName: "xmark")
                    .font(.system(size: 11, weight: .bold))
                    .foregroundStyle(F33Color.ink3)
                    .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Remove voice note")
        }
        .padding(.leading, F33Spacing.md)
        .frame(minHeight: F33Layout.minTouchTarget)
        .background(F33Color.bgSunken, in: RoundedRectangle(cornerRadius: F33Radius.md))
        .animation(F33Motion.easeOut, value: voice)
    }
}

// MARK: - GIF

/// The GIF on the draft, in the media strip beside any pictures, wearing the
/// web's `GIF` corner badge so it is not mistaken for one of them.
struct GifAttachmentTile: View {
    let gif: GifResult
    let remove: () -> Void

    var body: some View {
        ZStack(alignment: .topTrailing) {
            GifImage(url: gif.previewURL, seed: gif.id)
                .frame(width: 96, height: 96)
                .clipShape(RoundedRectangle(cornerRadius: F33Radius.sm))
                .overlay(alignment: .bottomLeading) {
                    MediaBadge(text: "GIF").padding(6)
                }

            RemoveButton(label: "Remove GIF", action: remove)
        }
        .padding(.top, 6)
        .padding(.trailing, 6)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(gif.title.isEmpty ? "GIF" : "GIF, \(gif.title)")
    }
}

// MARK: - Shared pieces

/// The small dark corner label the web puts on a media tile: `VIDEO`, `GIF`,
/// a running time.
struct MediaBadge: View {
    let text: String

    var body: some View {
        Text(text)
            .font(.system(size: 9, weight: .bold).monospacedDigit())
            .kerning(0.5)
            .foregroundStyle(.white)
            .padding(.horizontal, 6)
            .padding(.vertical, 2)
            .background(Color.black.opacity(0.65), in: RoundedRectangle(cornerRadius: 3))
    }
}

/// The cross in a tile's corner.
private struct RemoveButton: View {
    let label: String
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            Image(systemName: "xmark")
                .font(.system(size: 10, weight: .bold))
                .foregroundStyle(.white)
                .frame(width: 22, height: 22)
                .background(Color.black.opacity(0.6), in: Circle())
                .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget, alignment: .topTrailing)
                .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .offset(x: 6, y: -6)
        .accessibilityLabel(label)
    }
}

/// `m:ss`, the way the feed's voice player and the web's `cvp-label` write
/// a running time.
enum DurationClock {
    static func label(_ seconds: Int) -> String {
        let total = max(0, seconds)
        if total >= 3600 { return String(format: "%d:%02d:%02d", total / 3600, (total % 3600) / 60, total % 60) }
        return String(format: "%d:%02d", total / 60, total % 60)
    }
}

// MARK: - Picking a video

/// A movie chosen in the Photos picker, as a file on this device.
///
/// `FileRepresentation` rather than a `Data` transfer on purpose: the picker
/// would otherwise hand over the whole clip in memory, which for a phone's
/// video is the thing `APIClient.uploadVideo(fileURL:)` exists to avoid. The
/// file the picker provides lives only for the duration of the import closure,
/// so it is copied into the app's temporary directory and the copy is what the
/// composer uploads and removes.
struct PickedMovie: Transferable {
    let url: URL

    /// The MIME type the upload declares. The server decides what the file is
    /// from its bytes and only borrows the name's extension when it agrees,
    /// so this is a hint, not a claim.
    var mimeType: String {
        switch url.pathExtension.lowercased() {
        case "mov": return "video/quicktime"
        case "webm": return "video/webm"
        case "mkv": return "video/x-matroska"
        default: return "video/mp4"
        }
    }

    static var transferRepresentation: some TransferRepresentation {
        FileRepresentation(contentType: .movie) { movie in
            SentTransferredFile(movie.url)
        } importing: { received in
            let ext = received.file.pathExtension.isEmpty ? "mov" : received.file.pathExtension
            let copy = FileManager.default.temporaryDirectory
                .appendingPathComponent("f33d3r-pick-\(UUID().uuidString).\(ext)", isDirectory: false)
            try FileManager.default.copyItem(at: received.file, to: copy)
            return PickedMovie(url: copy)
        }
    }
}
