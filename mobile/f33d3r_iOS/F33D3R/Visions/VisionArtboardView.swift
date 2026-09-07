import SwiftUI
import F33D3RKit

/// The vision itself, full-bleed: a text card on one of the server's preset
/// backgrounds, or a photo at its native aspect letterboxed on black.
///
/// Every visual choice arrives as a preset key and is resolved here the same
/// way `styles.css` resolves it on the web — the gradients below are the
/// `.vision-artboard--bg-*` rules transcribed, so a vision looks the same on
/// both.
struct VisionArtboardView: View {
    let vision: Vision

    var body: some View {
        ZStack {
            if vision.isImage {
                Color.black
                RemoteImage(path: vision.mediaURLs.first, seed: vision.id, contentMode: .fit)
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                VisionBackground(preset: vision.artboard.background)
                Text(vision.body)
                    .font(VisionType.font(typeface: vision.artboard.typeface, scale: vision.artboard.typeScale))
                    .foregroundStyle(VisionBackground.isLight(vision.artboard.background) ? Color(hex: 0x231B13) : .white)
                    .multilineTextAlignment(VisionType.alignment(vision.artboard.align))
                    .lineSpacing(4)
                    .shadow(color: VisionBackground.isLight(vision.artboard.background) ? .clear : .black.opacity(0.35), radius: 8, y: 2)
                    .padding(.horizontal, 28)
                    .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: VisionType.frameAlignment(vision.artboard.align))
                    .fixedSize(horizontal: false, vertical: true)
            }

            if vision.isImage, !vision.body.isEmpty {
                VStack {
                    Spacer()
                    Text(vision.body)
                        .font(.system(size: 15, weight: .medium))
                        .foregroundStyle(.white)
                        .multilineTextAlignment(.center)
                        .padding(.horizontal, F33Spacing.md)
                        .padding(.vertical, F33Spacing.sm)
                        .background(Color.black.opacity(0.45), in: RoundedRectangle(cornerRadius: F33Radius.sm))
                        .padding(.bottom, F33Spacing.xl)
                }
            }
        }
        .clipped()
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(vision.isImage ? "Photo vision\(vision.body.isEmpty ? "" : ": \(vision.body)")" : "Text vision: \(vision.body)")
    }
}

/// The six server presets, as `styles.css` paints them.
struct VisionBackground: View {
    let preset: String

    var body: some View {
        switch preset {
        case "ember":
            LinearGradient(colors: [Color(hex: 0xFF5F2E), Color(hex: 0xC2185B), Color(hex: 0x4A0E3A)], startPoint: .topLeading, endPoint: .bottomTrailing)
        case "tide":
            LinearGradient(colors: [Color(hex: 0x0F8CFF), Color(hex: 0x0B3FA8), Color(hex: 0x061A3D)], startPoint: .topLeading, endPoint: .bottomTrailing)
        case "bloom":
            LinearGradient(colors: [Color(hex: 0xFF8FD0), Color(hex: 0x9B5CFF), Color(hex: 0x3A1F7A)], startPoint: .topLeading, endPoint: .bottomTrailing)
        case "pulp":
            Color(hex: 0xF4EAD6)
        case "signal":
            Color(hex: 0xF6FF3D)
        default:
            Color.black
        }
    }

    static func isLight(_ preset: String) -> Bool {
        preset == "pulp" || preset == "signal"
    }

    static let names: [(String, String)] = [
        ("void", "Void"), ("ember", "Ember"), ("tide", "Tide"), ("bloom", "Bloom"), ("pulp", "Pulp"), ("signal", "Signal"),
    ]
}

/// Typeface and scale presets to fonts.
enum VisionType {
    static func font(typeface: String, scale: String) -> Font {
        let size: CGFloat
        switch scale {
        case "xl": size = 44
        case "l": size = 34
        case "m": size = 26
        default: size = 20
        }
        switch typeface {
        case "serif": return .system(size: size, weight: .medium, design: .serif)
        case "mono": return .system(size: size, weight: .medium, design: .monospaced)
        case "display": return .system(size: size, weight: .heavy, design: .rounded)
        default: return .system(size: size, weight: .semibold, design: .default)
        }
    }

    static func alignment(_ align: String) -> TextAlignment {
        switch align {
        case "left": return .leading
        case "right": return .trailing
        default: return .center
        }
    }

    static func frameAlignment(_ align: String) -> Alignment {
        switch align {
        case "left": return .leading
        case "right": return .trailing
        default: return .center
        }
    }

    static let names: [(String, String)] = [("grotesk", "Aa"), ("serif", "Aa"), ("mono", "Aa"), ("display", "Aa")]
}
