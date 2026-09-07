import SwiftUI
import F33D3RKit

/// The Open Graph card for the first external link in a body.
struct LinkPreviewCard: View {
    let preview: LinkPreview

    @Environment(\.openURL) private var openURL

    var body: some View {
        Button {
            if let url = URL(string: preview.url) { openURL(url) }
        } label: {
            VStack(alignment: .leading, spacing: 0) {
                if preview.imageURL != nil {
                    RemoteImage(path: preview.imageURL, seed: preview.url)
                        .aspectRatio(1.91, contentMode: .fill)
                        .frame(maxWidth: .infinity)
                        .clipped()
                }

                VStack(alignment: .leading, spacing: 2) {
                    Text(preview.host)
                        .font(.system(size: 11))
                        .foregroundStyle(F33Color.ink4)

                    Text(preview.title)
                        .font(.system(size: 14, weight: .semibold))
                        .foregroundStyle(F33Color.ink)
                        .lineLimit(2)
                        .multilineTextAlignment(.leading)

                    if let description = preview.description, !description.isEmpty {
                        Text(description)
                            .font(.system(size: 13))
                            .foregroundStyle(F33Color.ink3)
                            .lineLimit(2)
                            .multilineTextAlignment(.leading)
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(F33Spacing.md)
            }
            .background(F33Color.bgElevated)
            .clipShape(RoundedRectangle(cornerRadius: F33Radius.md))
            .overlay(
                RoundedRectangle(cornerRadius: F33Radius.md)
                    .strokeBorder(F33Color.hairline, lineWidth: 1)
            )
        }
        .buttonStyle(.plain)
        .padding(.top, F33Card.mediaTopInset)
        .accessibilityLabel("Link: \(preview.title), \(preview.host)")
    }
}
