import SwiftUI
import Photos
import F33D3RKit

/// What the viewer opens on: a work's images and which one was tapped.
struct MediaViewerRequest: Identifiable {
    let id = UUID()
    let urls: [String]
    let index: Int
    let seed: String
}

/// Images full-screen, over black — the way Photos shows them.
///
/// The picture grows out of the tile that was tapped and shrinks back into it
/// when put away. Swipe sideways between a work's images; pinch or double-tap
/// to zoom, which hides the chrome; one tap brings it back or hides it again.
/// Pull down to leave: on iOS 18 the system drags the picture back toward its
/// tile, and below that the image follows the finger while the black behind
/// it thins, so letting go anywhere short of the threshold puts everything
/// back. The chrome is glass — a close button and a counter above, share and
/// save below — floating over the picture.
struct MediaViewerView: View {
    let request: MediaViewerRequest
    /// The namespace the tiles were declared in. Nil opens with a plain cover.
    var transitionNamespace: Namespace.ID?

    @Environment(\.dismiss) private var dismiss
    @Environment(\.mediaOrigin) private var origin
    @Environment(\.mediaLoader) private var loader
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    @State private var index: Int
    @State private var drag: CGSize = .zero
    @State private var isChromeHidden = false
    @State private var isZoomed = false
    @State private var saveState: SaveState = .idle

    private enum SaveState: Equatable {
        case idle, saving, saved, failed(String)
    }

    init(request: MediaViewerRequest, transitionNamespace: Namespace.ID? = nil) {
        self.request = request
        self.transitionNamespace = transitionNamespace
        _index = State(initialValue: min(max(0, request.index), max(0, request.urls.count - 1)))
    }

    /// 0 at rest, 1 at the point of dismissal.
    private var dragProgress: CGFloat {
        min(1, max(0, drag.height) / 240)
    }

    private var currentURL: URL? {
        URL.media(request.urls[safe: index], origin: origin)
    }

    /// The tile the picture shrinks back into: the page's own, or the last
    /// tile — the one wearing the overflow count — for a page past the grid.
    private var transitionSourceID: Int {
        min(index, MediaGrid.maxTiles - 1)
    }

    /// Whether the system's zoom transition owns the pull-down. When it does,
    /// running our own on the same finger would move the picture twice.
    private var isSystemDismiss: Bool {
        ZoomTransitionDestination.isSystemDriven(transitionNamespace)
    }

    var body: some View {
        ZStack {
            Color.black
                .opacity(Double(1 - dragProgress * 0.85))
                .ignoresSafeArea()

            pages
                .offset(drag)
                .scaleEffect(1 - dragProgress * 0.18)
                .simultaneousGesture(dismissDrag, including: isZoomed || isSystemDismiss ? .none : .all)

            if !isChromeHidden {
                chrome
                    .transition(.opacity)
            }
        }
        .statusBarHidden(isChromeHidden)
        .preferredColorScheme(.dark)
        .animation(reduceMotion ? nil : F33Motion.easeOut, value: isChromeHidden)
        // Zooming in is a request to look at the picture; the chrome gets out
        // of the way and stays away until asked back with a tap, as in Photos.
        .onChange(of: isZoomed) { _, zoomed in
            if zoomed { isChromeHidden = true }
        }
        .modifier(ZoomTransitionDestination(sourceID: transitionSourceID, namespace: transitionNamespace))
    }

    private var pages: some View {
        TabView(selection: $index) {
            ForEach(Array(request.urls.enumerated()), id: \.offset) { offset, path in
                ZoomableImage(path: path, seed: "\(request.seed)-\(offset)", isZoomed: $isZoomed) {
                    withAnimation(F33Motion.easeOut) { isChromeHidden.toggle() }
                }
                .tag(offset)
            }
        }
        .tabViewStyle(.page(indexDisplayMode: .never))
        .ignoresSafeArea()
    }

    /// The pull-down for systems without a zoom transition to do it.
    private var dismissDrag: some Gesture {
        DragGesture(minimumDistance: 12, coordinateSpace: .global)
            .onChanged { value in
                // Sideways is paging; only a mostly-vertical pull moves the image.
                guard abs(value.translation.height) > abs(value.translation.width) || drag != .zero else { return }
                drag = CGSize(width: value.translation.width * 0.4, height: max(0, value.translation.height))
            }
            .onEnded { value in
                if value.translation.height > 140 || value.predictedEndTranslation.height > 320 {
                    dismiss()
                } else {
                    withAnimation(F33Motion.spring) { drag = .zero }
                }
            }
    }

    // MARK: Chrome

    private var chrome: some View {
        VStack {
            HStack {
                glassButton("xmark", label: "Close") { dismiss() }
                Spacer()
                if request.urls.count > 1 {
                    Text("\(index + 1) of \(request.urls.count)")
                        .font(.system(size: 13, weight: .semibold).monospacedDigit())
                        .foregroundStyle(.white)
                        .padding(.horizontal, F33Spacing.md)
                        .padding(.vertical, 8)
                        .f33Glass(in: Capsule())
                }
            }
            .padding(.horizontal, F33Spacing.lg)
            .padding(.top, F33Spacing.sm)

            Spacer()

            HStack(spacing: F33Spacing.md) {
                if let url = currentURL {
                    ShareLink(item: url) {
                        Label("Share", systemImage: "square.and.arrow.up")
                            .labelStyle(.iconOnly)
                            .font(.system(size: 17, weight: .semibold))
                            .foregroundStyle(.white)
                            .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                            .f33Glass(in: Circle(), interactive: true)
                    }
                    .accessibilityLabel("Share image")
                }

                Spacer()

                saveButton
            }
            .padding(.horizontal, F33Spacing.lg)
            .padding(.bottom, F33Spacing.sm)
        }
        // The chrome goes first on a pull-down, well before the picture does.
        .opacity(Double(1 - dragProgress * 2))
    }

    private func glassButton(_ symbol: String, label: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Image(systemName: symbol)
                .font(.system(size: 15, weight: .bold))
                .foregroundStyle(.white)
                .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                .f33Glass(in: Circle(), interactive: true)
        }
        .buttonStyle(.plain)
        .accessibilityLabel(label)
    }

    private var saveButton: some View {
        Button {
            Task { await save() }
        } label: {
            HStack(spacing: 6) {
                switch saveState {
                case .idle:
                    Image(systemName: "arrow.down.to.line")
                    Text("Save")
                case .saving:
                    ProgressView().tint(.white)
                    Text("Saving")
                case .saved:
                    Image(systemName: "checkmark")
                    Text("Saved to Photos")
                case .failed(let why):
                    Image(systemName: "exclamationmark.triangle")
                    Text(why)
                }
            }
            .font(.system(size: 14, weight: .semibold))
            .foregroundStyle(.white)
            .padding(.horizontal, F33Spacing.lg)
            .frame(height: F33Layout.minTouchTarget)
            .f33Glass(in: Capsule(), interactive: true)
        }
        .buttonStyle(.plain)
        .disabled(saveState != .idle)
        .animation(F33Motion.easeOut, value: saveState)
        .accessibilityLabel("Save image to Photos")
    }

    private func save() async {
        guard let url = currentURL else { return }
        saveState = .saving
        do {
            // Through the loader: a walled image is 403 to an anonymous
            // request, and "Save" would fail on exactly the pictures the
            // reader is entitled to keep.
            let data = try await loader.data(for: url)
            guard let image = UIImage(data: data) else {
                saveState = .failed("Not an image")
                return
            }
            let status = await PHPhotoLibrary.requestAuthorization(for: .addOnly)
            guard status == .authorized || status == .limited else {
                saveState = .failed("Photos access is off")
                return
            }
            try await PHPhotoLibrary.shared().performChanges {
                PHAssetChangeRequest.creationRequestForAsset(from: image)
            }
            saveState = .saved
            try? await Task.sleep(for: .seconds(2))
            saveState = .idle
        } catch {
            saveState = .failed("Couldn't save")
            try? await Task.sleep(for: .seconds(2))
            saveState = .idle
        }
    }
}

/// One image that pinches and pans, on a scroll view.
///
/// A `UIScrollView` rather than a scale and an offset of our own, because the
/// scroll view is what makes this feel like Photos: pinching past the limits
/// rubber-bands, a pan decelerates and settles, and — the deciding reason — the
/// iOS 18 zoom transition's own pull-to-dismiss defers to a scroll view that
/// can scroll. A vertical drag on a zoomed picture pans it; only a picture at
/// fit is dragged away. Zoom runs from fit to 4×, a double tap goes to 2.5×
/// centred on the point tapped and back, and `isZoomed` tells the viewer when
/// drags belong to the picture instead of to paging or dismissal.
private struct ZoomableImage: UIViewRepresentable {
    let path: String
    let seed: String
    @Binding var isZoomed: Bool
    /// A single tap on the picture, or on the black around it.
    let onTap: () -> Void

    @Environment(\.mediaOrigin) private var origin

    func makeCoordinator() -> Coordinator {
        Coordinator(parent: self)
    }

    func makeUIView(context: Context) -> ZoomScrollView {
        let view = ZoomScrollView()
        view.host(RemoteImage(path: path, seed: seed, contentMode: .fit).environment(\.mediaOrigin, origin))
        let coordinator = context.coordinator
        view.onZoomChange = { zoomed in coordinator.parent.isZoomed = zoomed }
        view.onTap = { coordinator.parent.onTap() }
        view.accessibilityLabel = "Image"
        view.accessibilityTraits = .image
        return view
    }

    func updateUIView(_ view: ZoomScrollView, context: Context) {
        context.coordinator.parent = self
    }

    /// Holds the latest copy of the view, so the scroll view's callbacks write
    /// to the binding that is current rather than the one from `makeUIView`.
    final class Coordinator {
        var parent: ZoomableImage
        init(parent: ZoomableImage) { self.parent = parent }
    }
}

/// The scroll view behind `ZoomableImage`: one hosted content view, zoomed.
private final class ZoomScrollView: UIScrollView, UIScrollViewDelegate {
    var onZoomChange: ((Bool) -> Void)?
    var onTap: (() -> Void)?

    private var hosting: UIViewController?
    private var laidOutFor: CGSize = .zero

    private static let doubleTapScale: CGFloat = 2.5

    override init(frame: CGRect) {
        super.init(frame: frame)
        delegate = self
        minimumZoomScale = 1
        maximumZoomScale = 4
        bouncesZoom = true
        showsVerticalScrollIndicator = false
        showsHorizontalScrollIndicator = false
        contentInsetAdjustmentBehavior = .never
        backgroundColor = .clear

        let doubleTap = UITapGestureRecognizer(target: self, action: #selector(handleDoubleTap))
        doubleTap.numberOfTapsRequired = 2
        addGestureRecognizer(doubleTap)

        // A single tap waits for the second, so the first half of a double
        // tap never toggles the chrome on its way to zooming.
        let singleTap = UITapGestureRecognizer(target: self, action: #selector(handleSingleTap))
        singleTap.require(toFail: doubleTap)
        addGestureRecognizer(singleTap)
    }

    @available(*, unavailable)
    required init?(coder: NSCoder) {
        fatalError("ZoomScrollView is built in code")
    }

    /// Puts a SwiftUI view in as the thing that zooms.
    ///
    /// The image is still `RemoteImage`, so a slow or dead URL settles to the
    /// same placeholder here as in the grid. The hosting controller is not
    /// made a child — there is no parent to give it from inside a
    /// representable — which is fine for a view with nothing to present, and
    /// its safe area is switched off so the picture is laid out against the
    /// scroll view's bounds and not against a notch it cannot see.
    func host<V: View>(_ view: V) {
        let controller = UIHostingController(rootView: view)
        controller.view.backgroundColor = .clear
        controller.safeAreaRegions = []
        addSubview(controller.view)
        hosting = controller
    }

    override func layoutSubviews() {
        super.layoutSubviews()
        guard bounds.size != laidOutFor, let content = hosting?.view else { return }
        // A new size — first layout, or a rotation — starts over at fit.
        laidOutFor = bounds.size
        zoomScale = 1
        content.frame = CGRect(origin: .zero, size: bounds.size)
        contentSize = bounds.size
    }

    // MARK: Taps

    @objc private func handleSingleTap() {
        onTap?()
    }

    @objc private func handleDoubleTap(_ recognizer: UITapGestureRecognizer) {
        if zoomScale > 1.01 {
            setZoomScale(1, animated: true)
        } else if let content = hosting?.view {
            // The rect that ends up filling the view, centred on the tap.
            let point = recognizer.location(in: content)
            let size = CGSize(width: bounds.width / Self.doubleTapScale, height: bounds.height / Self.doubleTapScale)
            zoom(to: CGRect(x: point.x - size.width / 2, y: point.y - size.height / 2, width: size.width, height: size.height), animated: true)
        }
    }

    // MARK: UIScrollViewDelegate

    func viewForZooming(in scrollView: UIScrollView) -> UIView? {
        hosting?.view
    }

    func scrollViewDidZoom(_ scrollView: UIScrollView) {
        onZoomChange?(zoomScale > 1.01)
    }
}

private extension Array {
    subscript(safe index: Int) -> Element? {
        indices.contains(index) ? self[index] : nil
    }
}
