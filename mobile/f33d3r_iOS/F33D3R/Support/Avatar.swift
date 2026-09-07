import SwiftUI
import F33D3RKit

/// The avatar circle. One definition, used by every surface that shows a person.
///
/// Falls back to the same handle-seeded gradient and initial the web draws, so a
/// user without a picture is the same colour here as there.
struct F33Avatar: View {
    let handle: String
    let displayName: String
    let avatarURL: String?
    var size: CGFloat = F33Card.avatarColumnWidth
    /// The realm this person has reached, 1 to 5. Wanderer wears no ring; the
    /// four above it each have their own, matching the web exactly — blue,
    /// cyan to green, purple to pink, and the Guardian's turning fire.
    var realm: Int = 1

    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var spin = false

    var body: some View {
        avatar
            .padding(ring == nil ? 0 : Self.ringWidth)
            .background(ringBackground)
            .accessibilityHidden(true)
    }

    private var avatar: some View {
        RemoteImage(path: avatarURL, seed: handle)
            .frame(width: size, height: size)
            .background(gradient)
            .clipShape(Circle())
            .overlay(Circle().strokeBorder(F33Color.hairline, lineWidth: 0.5))
            // The image is decorative — the name is already in the row beside
            // it, and a second announcement of it is noise in VoiceOver.
    }

    /// The gap between the ring and the picture, so the ring reads as a ring
    /// rather than as a border on the photograph.
    private static let ringWidth: CGFloat = 2.5

    @ViewBuilder
    private var ringBackground: some View {
        if let ring {
            Circle()
                .fill(ring)
                .rotationEffect(.degrees(spin ? 360 : 0))
                .onAppear {
                    guard realm >= 5, !reduceMotion else { return }
                    withAnimation(.linear(duration: 2.5).repeatForever(autoreverses: false)) {
                        spin = true
                    }
                }
        }
    }

    /// R1 Wanderer has no ring: everyone starts there, so a ring on it would
    /// say nothing about anybody.
    private var ring: AnyShapeStyle? {
        switch realm {
        case 2:
            return AnyShapeStyle(Color(red: 0.23, green: 0.51, blue: 0.96))
        case 3:
            return AnyShapeStyle(LinearGradient(
                colors: [Color(red: 0.02, green: 0.71, blue: 0.83), Color(red: 0.13, green: 0.77, blue: 0.37)],
                startPoint: .topLeading, endPoint: .bottomTrailing))
        case 4:
            return AnyShapeStyle(LinearGradient(
                colors: [Color(red: 0.66, green: 0.33, blue: 0.97), Color(red: 0.93, green: 0.28, blue: 0.60)],
                startPoint: .topLeading, endPoint: .bottomTrailing))
        case let r where r >= 5:
            return AnyShapeStyle(AngularGradient(
                colors: [
                    Color(red: 0.96, green: 0.62, blue: 0.04),
                    Color(red: 0.94, green: 0.27, blue: 0.27),
                    Color(red: 0.66, green: 0.33, blue: 0.97),
                    Color(red: 0.23, green: 0.51, blue: 0.96),
                    Color(red: 0.96, green: 0.62, blue: 0.04),
                ],
                center: .center))
        default:
            return nil
        }
    }

    private var gradient: some View {
        let (start, end) = AvatarPalette.colors(for: handle)
        return LinearGradient(colors: [start, end], startPoint: .topLeading, endPoint: .bottomTrailing)
            .overlay(
                Text(AvatarPalette.initial(for: displayName.isEmpty ? handle : displayName))
                    .font(.system(size: size * 0.42, weight: .semibold, design: .rounded))
                    .foregroundStyle(.white)
            )
    }
}

extension F33Avatar {
    init(author: WorkAuthor, size: CGFloat = F33Card.avatarColumnWidth) {
        self.init(
            handle: author.handle,
            displayName: author.displayName,
            avatarURL: author.avatarURL,
            size: size,
            realm: author.realm
        )
    }

    init(user: User, size: CGFloat = F33Card.avatarColumnWidth) {
        self.init(
            handle: user.handle,
            displayName: user.displayName,
            avatarURL: user.avatarURL,
            size: size,
            realm: user.realm
        )
    }
}
