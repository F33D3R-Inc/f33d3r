import SwiftUI
import F33D3RKit

/// Three bars rising and falling: the mark that says this room is on air.
///
/// Hand-drawn from a `TimelineView` rather than pulled from a library, and
/// driven by the clock rather than by a repeating animation, so a pill scrolled
/// off and back on does not restart mid-beat and twenty of them on one row stay
/// in step with each other.
///
/// It is decoration for a fact stated in words beside it, so Reduce Motion
/// freezes it at a staggered pose rather than hiding it: the shape still reads
/// as sound, it just stops moving.
struct FrequencyAudioGlyph: View {
    var tint: Color = F33Color.accent
    var size: CGFloat = 14

    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        Group {
            if reduceMotion {
                bars(at: 0)
            } else {
                TimelineView(.animation) { context in
                    bars(at: context.date.timeIntervalSinceReferenceDate)
                }
            }
        }
        .frame(width: size, height: size)
        .accessibilityHidden(true)
    }

    private func bars(at time: TimeInterval) -> some View {
        HStack(alignment: .center, spacing: size * 0.16) {
            ForEach(0..<3, id: \.self) { index in
                Capsule(style: .continuous)
                    .fill(tint)
                    .frame(width: size * 0.18, height: height(index, at: time))
            }
        }
        .frame(width: size, height: size)
    }

    /// A sine per bar, a third of a cycle apart. The floor keeps a bar from
    /// vanishing at the bottom of its travel, which reads as a dropped frame.
    private func height(_ index: Int, at time: TimeInterval) -> CGFloat {
        let phase = Double(index) * 0.9
        let wave = (sin(time * 4.2 + phase) + 1) / 2
        return size * (0.3 + 0.7 * wave)
    }
}

/// Faces, overlapping, in the order the room ranks them: host first.
///
/// Takes as many authors as it is given and says how many more there are. The
/// count is the server's `participant_count` minus the faces shown, so it stays
/// honest when only the host's face is known.
struct FrequencyAvatarStack: View {
    let authors: [WorkAuthor]
    /// Everyone not pictured. Zero draws nothing.
    var overflow: Int = 0
    var size: CGFloat = 22
    var maxFaces: Int = 3

    var body: some View {
        HStack(spacing: -size * 0.32) {
            ForEach(Array(authors.prefix(maxFaces).enumerated()), id: \.element.handle) { index, author in
                F33Avatar(author: author, size: size)
                    .overlay(Circle().strokeBorder(F33Color.bg, lineWidth: 1.5))
                    .zIndex(Double(maxFaces - index))
            }
            if overflow > 0 {
                Text("+\(Counts.label(overflow) ?? "\(overflow)")")
                    .font(.system(size: size * 0.42, weight: .semibold).monospacedDigit())
                    .foregroundStyle(F33Color.ink3)
                    .padding(.leading, size * 0.42)
                    .fixedSize()
            }
        }
        .accessibilityHidden(true)
    }
}

/// LIVE / SCHEDULED / ENDED, in the room's own colour.
struct FrequencyStateChip: View {
    let frequency: Frequency

    var body: some View {
        Text(label)
            .font(.system(size: 10, weight: .bold))
            .tracking(0.5)
            .foregroundStyle(tint)
            .padding(.horizontal, 7)
            .padding(.vertical, 3)
            .background(tint.opacity(0.14), in: Capsule())
            .overlay(Capsule().strokeBorder(tint.opacity(0.3), lineWidth: 1))
            .fixedSize()
    }

    private var label: String {
        switch frequency.stateValue {
        case .live, .starting: return "ON AIR"
        case .scheduled: return "SCHEDULED"
        case .draft: return "DRAFT"
        case .ending: return "ENDING"
        case .cancelled: return "CALLED OFF"
        case .moderationTerminated: return "STOPPED"
        case .failed: return "FAILED"
        case .processingReplay: return "REPLAY SOON"
        case .archived, .ended: return "ENDED"
        case nil: return frequency.state.replacingOccurrences(of: "_", with: " ").uppercased()
        }
    }

    private var tint: Color {
        switch frequency.stateValue {
        case .live, .starting, .ending: return F33Color.accent
        case .scheduled, .draft: return F33Color.ink3
        case .failed, .moderationTerminated: return F33Color.danger
        default: return F33Color.ink4
        }
    }
}

/// The quiet line that says there is no sound yet.
///
/// The server writes this sentence — `audio_unavailable` — and the room shows
/// it exactly as sent. v1 of Frequencies has the room, the roles and the queue
/// and no audio transport, and a screen that stayed silent about that would be
/// a screen the reader thinks is broken.
struct FrequencyAudioNotice: View {
    let message: String

    var body: some View {
        HStack(alignment: .top, spacing: F33Spacing.sm) {
            Image(systemName: "speaker.slash")
                .font(.system(size: 13, weight: .medium))
                .foregroundStyle(F33Color.warn)
            Text(message)
                .font(.system(size: 13))
                .foregroundStyle(F33Color.ink3)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 0)
        }
        .padding(F33Spacing.md)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(F33Color.warn.opacity(0.08), in: RoundedRectangle(cornerRadius: F33Radius.sm))
        .overlay(
            RoundedRectangle(cornerRadius: F33Radius.sm)
                .strokeBorder(F33Color.warn.opacity(0.22), lineWidth: 1)
        )
    }
}

/// The red line a failed call leaves behind, above whatever asked for it.
struct FrequencyErrorLine: View {
    let message: String

    var body: some View {
        Text(message)
            .font(.footnote)
            .foregroundStyle(F33Color.danger)
            .frame(maxWidth: .infinity, alignment: .leading)
            .fixedSize(horizontal: false, vertical: true)
    }
}
