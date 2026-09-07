import SwiftUI
import ImageIO
import UniformTypeIdentifiers
import F33D3RKit

/// The composer's GIF picker: a search field over a two-column grid, trending
/// when the field is empty — the web's `gif-picker-overlay`, in a sheet.
///
/// Every answer the server can give is drawn as itself. A server with no
/// provider (`503 gif_search_unavailable`, or a deployment that has no such
/// route yet) says so; a provider that did not answer (`502 gif_search_failed`)
/// says that and offers a retry; a search that found nothing says "No GIFs
/// found". The web's picker draws the first two as an empty grid, which is why
/// the server was given codes for them — see api_v1_media.go.
///
/// Typing waits 350ms before asking, as the web's does, so a word is one
/// request and not five. A tap hands the GIF to the composer and closes.
struct GifPickerSheet: View {
    let client: APIClient
    let pick: (GifResult) -> Void

    @Environment(\.dismiss) private var dismiss

    @State private var query = ""
    @State private var results: [GifResult]?
    @State private var problem: APIError?
    @State private var attempt = 0
    @FocusState private var isSearching: Bool

    private static let columns = Array(repeating: GridItem(.flexible(), spacing: 4), count: 2)
    private static let debounce: Duration = .milliseconds(350)

    var body: some View {
        NavigationStack {
            VStack(spacing: 0) {
                searchField
                    .padding(.horizontal, F33Spacing.lg)
                    .padding(.vertical, F33Spacing.sm)

                content
            }
            .background(F33Color.bg)
            .navigationTitle("GIFs")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") { dismiss() }
                        .foregroundStyle(F33Color.ink2)
                }
            }
        }
        .presentationDetents([.large])
        .presentationDragIndicator(.visible)
        // Keyed on the query and the attempt: a keystroke restarts the wait, a
        // retry asks again with the same words.
        .task(id: "\(attempt)|\(query)") { await search() }
    }

    private var searchField: some View {
        HStack(spacing: F33Spacing.sm) {
            Image(systemName: "magnifyingglass")
                .foregroundStyle(F33Color.ink4)
            TextField("Search GIFs", text: $query)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
                .submitLabel(.search)
                .focused($isSearching)
            if !query.isEmpty {
                Button {
                    query = ""
                } label: {
                    Image(systemName: "xmark.circle.fill")
                        .foregroundStyle(F33Color.ink4)
                        .frame(width: 32, height: F33Layout.minTouchTarget)
                        .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Clear search")
            }
        }
        .f33Field()
    }

    @ViewBuilder
    private var content: some View {
        if let problem {
            problemView(problem)
        } else if let results {
            if results.isEmpty {
                EmptyStateView(
                    icon: "photo.on.rectangle.angled",
                    title: "No GIFs found",
                    message: query.isEmpty ? "Nothing is trending right now." : "Nothing matched “\(query)”."
                )
            } else {
                grid(results)
            }
        } else {
            VStack(spacing: F33Spacing.md) {
                ProgressView().tint(F33Color.accent)
                Text(query.isEmpty ? "Loading trending GIFs…" : "Searching…")
                    .font(.subheadline)
                    .foregroundStyle(F33Color.ink4)
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }

    private func grid(_ results: [GifResult]) -> some View {
        ScrollView {
            LazyVGrid(columns: Self.columns, spacing: 4) {
                ForEach(results) { gif in
                    Button {
                        pick(gif)
                        dismiss()
                    } label: {
                        GifImage(url: gif.previewURL, seed: gif.id)
                            .aspectRatio(1, contentMode: .fill)
                            .clipShape(RoundedRectangle(cornerRadius: F33Radius.sm))
                            .contentShape(RoundedRectangle(cornerRadius: F33Radius.sm))
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel(gif.title.isEmpty ? "GIF" : gif.title)
                    .accessibilityHint("Attaches this GIF")
                }
            }
            .padding(.horizontal, F33Spacing.sm)
            .padding(.bottom, F33Spacing.xl)
        }
        .scrollDismissesKeyboard(.immediately)
    }

    /// The things a failed search can mean, each said as itself: a server
    /// with no provider, a server too old to have the route, and a provider
    /// that did not answer — only the last is worth a retry.
    private func problemView(_ error: APIError) -> some View {
        Group {
            if error.code == "gif_search_unavailable" {
                EmptyStateView(
                    icon: "photo.on.rectangle.angled",
                    title: "GIFs aren't available here",
                    message: "This F33D3R server has no GIF provider."
                )
            } else if error.isUnservedSurface {
                EmptyStateView(
                    icon: "photo.on.rectangle.angled",
                    title: "GIFs aren't available here",
                    message: "This F33D3R server doesn't offer GIF search yet."
                )
            } else {
                ErrorStateView(error: error) { attempt += 1 }
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)
    }

    private func search() async {
        // The pause the web's picker takes between a keystroke and a request.
        // Cancelled by the next keystroke, which is what makes it a debounce.
        if !query.isEmpty {
            do {
                try await Task.sleep(for: Self.debounce)
            } catch {
                return
            }
        }
        results = nil
        problem = nil
        do {
            let found = try await client.gifSearch(query.trimmingCharacters(in: .whitespaces))
            guard !Task.isCancelled else { return }
            withAnimation(F33Motion.easeOut) { results = found }
        } catch let error as APIError {
            guard !Task.isCancelled else { return }
            problem = error
        } catch {
            guard !Task.isCancelled else { return }
            problem = .transport(error.localizedDescription)
        }
    }
}

/// A GIF from the provider, animated.
///
/// The provider's URLs are absolute and on its own host, not walled `/media/`
/// paths on this server. They still go through the app's `MediaLoader`, which
/// resolves an absolute URL as itself and — this is the part that matters —
/// attaches the session only to this instance's origin, so the bearer token is
/// never sent to the provider's CDN. `RemoteImage` would draw the first frame
/// only, since `UIImage(data:)` does not animate; this decodes the frames with
/// ImageIO and hands `UIImageView` the animation, which is what a GIF picker
/// is for.
///
/// A tile that did not load draws the placeholder with a mark on it. It says
/// "this one did not come", which is true, and nothing about the provider's
/// CDN, which the author cannot act on.
struct GifImage: View {
    let url: String
    var seed: String = ""

    @Environment(\.mediaOrigin) private var origin
    @Environment(\.mediaLoader) private var loader

    @State private var image: UIImage?
    @State private var failed = false

    private var resolved: URL? { .media(url, origin: origin) }

    var body: some View {
        Group {
            if let image {
                AnimatedImageView(image: image)
            } else {
                placeholder
                    .overlay {
                        if failed {
                            Image(systemName: "exclamationmark.triangle")
                                .font(.system(size: 16))
                                .foregroundStyle(F33Color.ink4)
                        }
                    }
            }
        }
        .task(id: resolved) { await load() }
    }

    private func load() async {
        image = nil
        failed = false
        guard let resolved else {
            failed = true
            return
        }
        do {
            let data = try await loader.data(for: resolved)
            guard !Task.isCancelled else { return }
            guard let decoded = UIImage.animatedGIF(data) else {
                failed = true
                return
            }
            withAnimation(F33Motion.easeOut) { image = decoded }
        } catch {
            guard !Task.isCancelled else { return }
            failed = true
        }
    }

    private var placeholder: some View {
        let (start, end) = AvatarPalette.colors(for: seed.isEmpty ? url : seed)
        return LinearGradient(
            colors: [start.opacity(0.28), end.opacity(0.28)],
            startPoint: .topLeading,
            endPoint: .bottomTrailing
        )
        .overlay(F33Color.bgSunken.opacity(0.35))
    }
}

/// `UIImageView`, which animates a frame sequence where SwiftUI's `Image`
/// shows the first frame and stops.
private struct AnimatedImageView: UIViewRepresentable {
    let image: UIImage

    func makeUIView(context: Context) -> UIImageView {
        let view = UIImageView()
        view.contentMode = .scaleAspectFill
        view.clipsToBounds = true
        // The tile's frame is the layout; the image's pixel size has no say.
        view.setContentHuggingPriority(.defaultLow, for: .horizontal)
        view.setContentHuggingPriority(.defaultLow, for: .vertical)
        view.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
        view.setContentCompressionResistancePriority(.defaultLow, for: .vertical)
        return view
    }

    func updateUIView(_ view: UIImageView, context: Context) {
        if view.image !== image {
            view.image = image
            if image.images != nil { view.startAnimating() }
        }
    }

    func sizeThatFits(_ proposal: ProposedViewSize, uiView: UIImageView, context: Context) -> CGSize? {
        CGSize(width: proposal.width ?? 96, height: proposal.height ?? 96)
    }
}

extension UIImage {
    /// The frames of a GIF as one animated image, timed as the file says.
    ///
    /// ImageIO reads the per-frame delay from the GIF dictionary; a frame with
    /// no delay, or a delay under the 20ms browsers clamp at, is given 100ms,
    /// which is what every browser does with those files. A file with one
    /// frame is a still, returned as one.
    static func animatedGIF(_ data: Data) -> UIImage? {
        guard let source = CGImageSourceCreateWithData(data as CFData, nil) else { return nil }
        let count = CGImageSourceGetCount(source)
        guard count > 0 else { return nil }
        if count == 1 {
            return CGImageSourceCreateImageAtIndex(source, 0, nil).map { UIImage(cgImage: $0) }
        }
        var frames: [UIImage] = []
        var total: TimeInterval = 0
        frames.reserveCapacity(count)
        for index in 0..<count {
            guard let frame = CGImageSourceCreateImageAtIndex(source, index, nil) else { continue }
            frames.append(UIImage(cgImage: frame))
            total += frameDelay(source, index)
        }
        guard !frames.isEmpty else { return nil }
        return UIImage.animatedImage(with: frames, duration: total)
    }

    private static func frameDelay(_ source: CGImageSource, _ index: Int) -> TimeInterval {
        guard let properties = CGImageSourceCopyPropertiesAtIndex(source, index, nil) as? [CFString: Any],
              let gif = properties[kCGImagePropertyGIFDictionary] as? [CFString: Any]
        else { return 0.1 }
        let unclamped = gif[kCGImagePropertyGIFUnclampedDelayTime] as? Double
        let clamped = gif[kCGImagePropertyGIFDelayTime] as? Double
        let delay = unclamped ?? clamped ?? 0.1
        return delay < 0.02 ? 0.1 : delay
    }
}
