import SwiftUI
import F33D3RKit

/// One creator's visions, full-bleed.
///
/// A segmented timer along the top, the vision at its native aspect on black,
/// and a footer that offers exactly two things: a private reply that lands
/// in the author's notifications, and a tip. No public like, no repost — pressure-free
/// by design. Tap the right half for the next vision, the left for the
/// previous; hold to pause; swipe down to dismiss; swipe sideways to the next
/// creator. Each vision is marked seen the moment it is shown, and the ring in
/// the tray flips when the server has recorded it.
struct VisionViewerView: View {
    let store: VisionTrayStore
    /// The ring to open on. Later rings come from the store, so a ring that
    /// changed while the viewer was open is drawn fresh.
    let initial: VisionRing

    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    @State private var handle: String
    @State private var index: Int
    @State private var progress: Double = 0
    @State private var isPaused = false
    @State private var dragOffset: CGFloat = 0
    @State private var reply = ""
    @State private var isSendingReply = false
    @State private var toast: String?
    @State private var isTipping = false
    @State private var isShowingWhy = false
    @State private var isConfirmingDelete = false
    @State private var isShowingViewers = false
    @FocusState private var isReplyFocused: Bool

    /// Seconds a photo or text vision stays before advancing.
    private let dwell: Double = 6

    init(store: VisionTrayStore, initial: VisionRing) {
        self.store = store
        self.initial = initial
        _handle = State(initialValue: initial.author.handle)
        _index = State(initialValue: initial.startIndex)
    }

    private var ring: VisionRing { store.ring(for: handle) ?? initial }
    private var vision: Vision? { ring.visions.indices.contains(index) ? ring.visions[index] : ring.visions.last }
    private var isOwn: Bool { ring.author.handle == model.state.user?.user.handle }

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()

            if let vision {
                content(vision)
            } else {
                EmptyStateView(icon: "circle.dashed", title: "Nothing here now", message: "This vision has expired.")
                    .foregroundStyle(.white)
            }
        }
        .offset(y: dragOffset)
        .opacity(1 - Double(min(1, max(0, dragOffset / 400))))
        .gesture(dismissDrag)
        .statusBarHidden()
        .preferredColorScheme(.dark)
        .task(id: vision?.id) { await run() }
        .sheet(isPresented: $isTipping) { TipSheet(handle: ring.author.handle) }
        .sheet(isPresented: $isShowingWhy) {
            if let provenance = ring.provenance {
                WhyThisSheet(provenance: provenance, handle: ring.author.handle)
            }
        }
        .sheet(isPresented: $isShowingViewers) {
            if let vision { VisionViewersSheet(vision: vision) }
        }
        .confirmationDialog("Delete this vision?", isPresented: $isConfirmingDelete, titleVisibility: .visible) {
            Button("Delete", role: .destructive) {
                guard let vision else { return }
                Task {
                    try? await store.deleteOwn(vision)
                    if store.ring(for: handle) == nil { dismiss() } else { index = 0 }
                }
            }
        }
        .onChange(of: isTipping) { _, open in isPaused = open }
        .onChange(of: isShowingWhy) { _, open in isPaused = open }
        .onChange(of: isShowingViewers) { _, open in isPaused = open }
        .onChange(of: isReplyFocused) { _, focused in isPaused = focused }
        .animation(reduceMotion ? nil : F33Motion.easeOut, value: toast)
    }

    // MARK: - Layout

    @ViewBuilder
    private func content(_ vision: Vision) -> some View {
        VStack(spacing: 0) {
            segments
                .padding(.horizontal, 12)
                .padding(.top, 8)

            header(vision)
                .padding(.horizontal, F33Spacing.lg)
                .padding(.top, 10)

            ZStack(alignment: .bottom) {
                VisionArtboardView(vision: vision)
                    .clipShape(RoundedRectangle(cornerRadius: F33Radius.lg))
                    .overlay(tapZones)
                    .onLongPressGesture(minimumDuration: 0.2, pressing: { isPaused = $0 || isReplyFocused }) {}

                if let poll = vision.poll {
                    VisionPollOverlay(poll: poll, isOwn: isOwn) { option in
                        Task { try? await store.vote(vision, option: option) }
                    }
                    .padding(12)
                }
            }
            .padding(.horizontal, 12)
            .padding(.top, 14)

            footer(vision)
                .padding(.horizontal, 12)
                .padding(.top, 12)
                .padding(.bottom, 12)
        }
        .overlay(alignment: .top) {
            if let toast {
                Text(toast)
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundStyle(F33Color.ink)
                    .padding(.horizontal, F33Spacing.lg)
                    .padding(.vertical, F33Spacing.sm)
                    .f33Glass(in: Capsule(), elevation: .floating)
                    .padding(.top, 60)
                    .transition(.move(edge: .top).combined(with: .opacity))
            }
        }
    }

    private var segments: some View {
        HStack(spacing: 4) {
            ForEach(Array(ring.visions.enumerated()), id: \.element.id) { i, _ in
                GeometryReader { geo in
                    ZStack(alignment: .leading) {
                        Capsule().fill(Color.white.opacity(0.3))
                        Capsule()
                            .fill(Color.white)
                            .frame(width: geo.size.width * fill(for: i))
                    }
                }
                .frame(height: 3)
            }
        }
        .accessibilityHidden(true)
    }

    private func fill(for i: Int) -> CGFloat {
        if i < index { return 1 }
        if i == index { return CGFloat(progress) }
        return 0
    }

    private func header(_ vision: Vision) -> some View {
        HStack(spacing: 10) {
            NavigationLink(value: Route.profile(handle: ring.author.handle)) {
                F33Avatar(author: ring.author, size: 34)
                    .overlay(Circle().strokeBorder(.white.opacity(0.9), lineWidth: 1.5))
            }
            .buttonStyle(.plain)

            VStack(alignment: .leading, spacing: 1) {
                Text("@\(ring.author.handle)")
                    .font(.system(size: 14, weight: .semibold))
                    .foregroundStyle(.white)
                Text(meta(vision))
                    .font(.system(size: 11))
                    .foregroundStyle(.white.opacity(0.7))
                    .lineLimit(1)
            }

            Spacer(minLength: F33Spacing.sm)

            if ring.provenance != nil {
                Button { isShowingWhy = true } label: {
                    Text("why: follow")
                        .font(.system(size: 10.5, weight: .medium))
                        .foregroundStyle(.white)
                        .padding(.horizontal, 8)
                        .padding(.vertical, 4)
                        .overlay(Capsule().strokeBorder(.white.opacity(0.7), lineWidth: 1))
                        .frame(minHeight: F33Layout.minTouchTarget)
                        .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Why you're seeing this")
            }

            Button { dismiss() } label: {
                Image(systemName: "xmark")
                    .font(.system(size: 17, weight: .semibold))
                    .foregroundStyle(.white)
                    .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Close")
        }
    }

    /// "3 of 4 · 5h left · 1.2k views" — views only for the author.
    private func meta(_ vision: Vision) -> String {
        var parts = ["\(index + 1) of \(ring.visions.count)", RelativeTime.countdown(to: vision.expiresAt)]
        if let views = vision.viewCount {
            parts.append(views == 1 ? "1 view" : "\(Counts.label(views) ?? "0") views")
        }
        return parts.joined(separator: " · ")
    }

    /// Left third goes back, the rest goes forward. Drawn over the artboard
    /// and under nothing, so a poll's buttons still win.
    private var tapZones: some View {
        HStack(spacing: 0) {
            Color.clear.contentShape(Rectangle()).onTapGesture { previous() }
                .frame(maxWidth: .infinity)
            Color.clear.contentShape(Rectangle()).onTapGesture { next() }
                .frame(maxWidth: .infinity)
                .frame(maxWidth: .infinity)
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("Vision")
        .accessibilityHint("Swipe up for the next vision, down for the previous")
        .accessibilityAdjustableAction { direction in
            switch direction {
            case .increment: next()
            case .decrement: previous()
            @unknown default: break
            }
        }
    }

    @ViewBuilder
    private func footer(_ vision: Vision) -> some View {
        HStack(spacing: 10) {
            if isOwn {
                Button { isShowingViewers = true } label: {
                    HStack(spacing: 6) {
                        Image(systemName: "eye")
                        Text(vision.viewCount.map { $0 == 1 ? "1 view" : "\(Counts.exact($0)) views" } ?? "Views")
                    }
                    .font(.system(size: 14, weight: .medium))
                    .foregroundStyle(.white)
                    .frame(maxWidth: .infinity)
                    .frame(height: 48)
                    .overlay(Capsule().strokeBorder(.white.opacity(0.7), lineWidth: 1.5))
                    .contentShape(Capsule())
                }
                .buttonStyle(.plain)

                roundButton("trash", label: "Delete") { isConfirmingDelete = true }
            } else {
                if vision.allowReplies {
                    HStack(spacing: F33Spacing.sm) {
                        TextField("Reply privately…", text: $reply)
                            .font(.system(size: 15))
                            .foregroundStyle(.white)
                            .tint(.white)
                            .submitLabel(.send)
                            .focused($isReplyFocused)
                            .onSubmit { Task { await sendReply(vision) } }
                        if !reply.trimmingCharacters(in: .whitespaces).isEmpty {
                            Button { Task { await sendReply(vision) } } label: {
                                Image(systemName: "arrow.up.circle.fill")
                                    .font(.system(size: 24))
                                    .foregroundStyle(.white)
                            }
                            .buttonStyle(.plain)
                            .disabled(isSendingReply)
                            .accessibilityLabel("Send reply")
                        }
                    }
                    .padding(.horizontal, 16)
                    .frame(height: 48)
                    .overlay(Capsule().strokeBorder(.white.opacity(0.7), lineWidth: 1.5))
                } else {
                    Text("Replies are off")
                        .font(.system(size: 14))
                        .foregroundStyle(.white.opacity(0.6))
                        .frame(maxWidth: .infinity)
                        .frame(height: 48)
                }

                Button { isTipping = true } label: {
                    Image(systemName: "bolt.fill")
                        .font(.system(size: 18, weight: .semibold))
                        .foregroundStyle(Color(hex: 0x14120C))
                        .frame(width: 48, height: 48)
                        .background(Color(hex: 0xFFE27A), in: Circle())
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Tip @\(ring.author.handle)")

                Menu {
                    Button(role: .destructive) {
                        Task {
                            try? await store.muteVisions(from: ring.author.handle)
                            dismiss()
                        }
                    } label: {
                        Label("Mute visions from @\(ring.author.handle)", systemImage: "eye.slash")
                    }
                    if ring.provenance != nil {
                        Button { isShowingWhy = true } label: {
                            Label("Why am I seeing this", systemImage: "questionmark.circle")
                        }
                    }
                } label: {
                    Image(systemName: "ellipsis")
                        .font(.system(size: 17, weight: .semibold))
                        .foregroundStyle(.white)
                        .frame(width: 48, height: 48)
                        .overlay(Circle().strokeBorder(.white.opacity(0.7), lineWidth: 1.5))
                        .contentShape(Circle())
                }
                .menuStyle(.button)
                .buttonStyle(.plain)
                .accessibilityLabel("More")
            }
        }
    }

    private func roundButton(_ icon: String, label: String, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Image(systemName: icon)
                .font(.system(size: 17, weight: .semibold))
                .foregroundStyle(.white)
                .frame(width: 48, height: 48)
                .overlay(Circle().strokeBorder(.white.opacity(0.7), lineWidth: 1.5))
                .contentShape(Circle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel(label)
    }

    // MARK: - Timer and navigation

    /// Runs one vision: marks it seen, then advances the timer until it is
    /// full, paused, or the vision changes (which cancels this task).
    private func run() async {
        guard let vision else { return }
        progress = 0
        await store.markSeen(vision)
        let tick = 0.05
        while !Task.isCancelled {
            try? await Task.sleep(for: .milliseconds(50))
            if Task.isCancelled { return }
            if isPaused { continue }
            progress = min(1, progress + tick / dwell)
            if progress >= 1 {
                next()
                return
            }
        }
    }

    private func next() {
        if index + 1 < ring.visions.count {
            index += 1
        } else if let following = store.ring(after: handle) {
            handle = following.author.handle
            index = following.startIndex
        } else {
            dismiss()
        }
    }

    private func previous() {
        if progress > 0.15 {
            progress = 0
        } else if index > 0 {
            index -= 1
        } else if let earlier = store.ring(before: handle) {
            handle = earlier.author.handle
            index = max(0, earlier.visions.count - 1)
        } else {
            progress = 0
        }
    }

    private var dismissDrag: some Gesture {
        DragGesture(minimumDistance: 20)
            .onChanged { value in
                if abs(value.translation.height) > abs(value.translation.width), value.translation.height > 0 {
                    dragOffset = value.translation.height
                    isPaused = true
                }
            }
            .onEnded { value in
                defer { isPaused = isReplyFocused }
                if value.translation.height > 120 {
                    dismiss()
                    return
                }
                withAnimation(F33Motion.spring) { dragOffset = 0 }
                if abs(value.translation.width) > 60, abs(value.translation.width) > abs(value.translation.height) {
                    if value.translation.width < 0 {
                        if let following = store.ring(after: handle) {
                            handle = following.author.handle
                            index = following.startIndex
                        } else {
                            dismiss()
                        }
                    } else if let earlier = store.ring(before: handle) {
                        handle = earlier.author.handle
                        index = earlier.startIndex
                    }
                }
            }
    }

    private func sendReply(_ vision: Vision) async {
        let text = reply.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !text.isEmpty, !isSendingReply else { return }
        isSendingReply = true
        defer { isSendingReply = false }
        do {
            try await model.client.replyToVision(id: vision.id, body: text)
            reply = ""
            isReplyFocused = false
            show("Sent privately to @\(ring.author.handle)")
        } catch let error as MalkuthError {
            show(error.description)
        } catch {
            show(error.localizedDescription)
        }
    }

    private func show(_ message: String) {
        toast = message
        Task {
            try? await Task.sleep(for: .seconds(2.2))
            if toast == message { toast = nil }
        }
    }
}

/// A poll inside a vision: a card over the artboard's foot.
struct VisionPollOverlay: View {
    let poll: Poll
    let isOwn: Bool
    let onVote: (Int) -> Void

    var body: some View {
        VStack(spacing: 6) {
            ForEach(poll.results) { result in
                if poll.showsResults || isOwn {
                    HStack {
                        Text(result.label)
                            .font(.system(size: 14, weight: result.isWinner ? .semibold : .regular))
                        if result.voted {
                            Image(systemName: "checkmark.circle.fill").font(.system(size: 12))
                        }
                        Spacer()
                        Text("\(result.pct)%").font(.system(size: 13, weight: .semibold).monospacedDigit())
                    }
                    .foregroundStyle(F33Color.ink)
                    .padding(.horizontal, 12)
                    .frame(height: 36)
                    .background(alignment: .leading) {
                        GeometryReader { geo in
                            RoundedRectangle(cornerRadius: 6).fill(F33Color.accent.opacity(result.voted ? 0.35 : 0.18))
                                .frame(width: geo.size.width * CGFloat(result.pct) / 100)
                        }
                    }
                    .background(F33Color.bgSunken, in: RoundedRectangle(cornerRadius: 6))
                } else {
                    Button { onVote(result.index) } label: {
                        Text(result.label)
                            .font(.system(size: 14, weight: .medium))
                            .foregroundStyle(F33Color.accent)
                            .frame(maxWidth: .infinity)
                            .frame(height: F33Layout.minTouchTarget)
                            .overlay(Capsule().strokeBorder(F33Color.accent.opacity(0.6), lineWidth: 1.5))
                            .contentShape(Capsule())
                    }
                    .buttonStyle(.plain)
                }
            }
            Text(poll.totalVotes == 1 ? "1 vote" : "\(Counts.exact(poll.totalVotes)) votes")
                .font(.system(size: 11))
                .foregroundStyle(F33Color.ink4)
        }
        .padding(10)
        .background(Color.white.opacity(0.92), in: RoundedRectangle(cornerRadius: F33Radius.md))
        .environment(\.colorScheme, .light)
    }
}

/// Who watched one of the reader's own visions. Creator-only.
struct VisionViewersSheet: View {
    let vision: Vision

    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    @State private var viewers: [WorkAuthor] = []
    @State private var error: String?

    var body: some View {
        NavigationStack {
            List {
                if viewers.isEmpty, error == nil {
                    Text("Nobody yet.")
                        .foregroundStyle(F33Color.ink4)
                }
                ForEach(viewers) { viewer in
                    HStack(spacing: F33Spacing.md) {
                        F33Avatar(author: viewer, size: 34)
                        VStack(alignment: .leading, spacing: 1) {
                            Text(viewer.displayName).font(.system(size: 15, weight: .medium))
                            Text("@\(viewer.handle)").font(.system(size: 13)).foregroundStyle(F33Color.ink4)
                        }
                    }
                }
                if let error {
                    Text(error).foregroundStyle(F33Color.danger)
                }
            }
            .listStyle(.plain)
            .navigationTitle("Viewers")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .confirmationAction) { Button("Done") { dismiss() } }
            }
        }
        .f33GlassSheet()
        .presentationDetents([.medium, .large])
        .task {
            struct Page: Decodable { let viewers: [WorkAuthor] }
            do {
                let page: Page = try await model.client.send(.visionViewers(id: vision.id), body: Optional<Never>.none, as: Page.self)
                viewers = page.viewers
            } catch let apiError as APIError {
                error = apiError.userMessage
            } catch {
                self.error = error.localizedDescription
            }
        }
    }
}
