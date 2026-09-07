import SwiftUI
import F33D3RKit

/// Watching a room: the stream, a chat you can hide, and a tip that never
/// makes you leave.
///
/// Video is not wired in this build. Where the picture would be, the server's
/// own sentence says so — not a spinner, not a poster pretending to buffer.
/// Everything else is live: chat, presence, the pinned goal, hearts, tips.
struct LiveViewerView: View {
    let streamID: String

    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    @State private var room: LiveRoomStore?
    @State private var draft = ""
    @State private var chatHidden = false
    @State private var isTipping = false
    @State private var following: Bool?
    @State private var floatingHearts: [UUID] = []
    @State private var tipError: String?

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()
            if let room {
                content(room)
            } else {
                ProgressView().tint(.white)
            }
        }
        .statusBarHidden()
        // No compose button here — a live room is somewhere you are, not a page
        // you are reading, and it hides the tab bar for the same reason.
        .hidesComposeFAB()
        .preferredColorScheme(.dark)
        .toolbar(.hidden, for: .navigationBar)
        .toolbar(.hidden, for: .tabBar)
        .task {
            if room == nil {
                let store = model.liveRoom(id: streamID)
                room = store
                await store.connect()
                if let handle = store.stream?.author.handle, handle != model.state.user?.user.handle {
                    following = try? await model.profile(handle: handle).viewerFollows
                }
            }
        }
        .onDisappear { room?.disconnect() }
        .sheet(isPresented: $isTipping) {
            if let room, let stream = room.stream {
                LiveTipSheet(room: room, handle: stream.author.handle)
            }
        }
    }

    @ViewBuilder
    private func content(_ room: LiveRoomStore) -> some View {
        switch room.phase {
        case .idle, .loading:
            ProgressView().tint(.white)
        case .failed(let error):
            ErrorStateView(error: error) { Task { await room.connect() } }
                .foregroundStyle(.white)
        case .live, .ended:
            stage(room)
            LinearGradient(colors: [.black.opacity(0.6), .clear, .clear, .black.opacity(0.65)], startPoint: .top, endPoint: .bottom)
                .ignoresSafeArea()
                .allowsHitTesting(false)
            overlay(room)
            if room.phase == .ended {
                endedOverlay(room)
            }
            hearts
        }
    }

    /// Where the video would be. Honest about its absence.
    private func stage(_ room: LiveRoomStore) -> some View {
        ZStack {
            LinearGradient(colors: [Color(hex: 0x1A1A1A), Color(hex: 0x2A2140)], startPoint: .top, endPoint: .bottom)
            VStack(spacing: 10) {
                Image(systemName: "dot.radiowaves.left.and.right")
                    .font(.system(size: 34))
                    .foregroundStyle(.white.opacity(0.5))
                Text(room.stream?.title ?? "")
                    .font(.system(size: 20, weight: .semibold))
                    .foregroundStyle(.white)
                    .multilineTextAlignment(.center)
                    .padding(.horizontal, F33Spacing.xl)
                if let note = room.stream?.videoUnavailable, room.stream?.hasVideo == false {
                    Text(note)
                        .font(.system(size: 12))
                        .foregroundStyle(.white.opacity(0.6))
                        .multilineTextAlignment(.center)
                        .padding(.horizontal, 40)
                }
            }
        }
        .ignoresSafeArea()
        .accessibilityElement(children: .combine)
    }

    @ViewBuilder
    private func overlay(_ room: LiveRoomStore) -> some View {
        VStack(spacing: 0) {
            header(room)
                .padding(.horizontal, 12)
                .padding(.top, 8)

            if let pinned = room.pinned {
                HStack(spacing: 6) {
                    Label(pinned, systemImage: "pin.fill")
                        .font(.system(size: 12, weight: .medium))
                        .foregroundStyle(.white)
                        .lineLimit(2)
                        .padding(.horizontal, 10)
                        .padding(.vertical, 6)
                        .background(Color.black.opacity(0.55), in: Capsule())
                    Spacer(minLength: 0)
                }
                .padding(.horizontal, 12)
                .padding(.top, 10)
                .transition(.opacity)
            }

            Spacer(minLength: 0)

            HStack(alignment: .bottom, spacing: 10) {
                if chatHidden {
                    Spacer(minLength: 0)
                } else {
                    LiveChatBubbles(messages: room.chat)
                }
                rail(room)
            }
            .padding(.horizontal, 12)
            .padding(.bottom, 10)

            if room.phase == .live {
                HStack(spacing: 8) {
                    LiveChatComposer(text: $draft) { Task { await send(room) } }
                    ForEach([500, 2000, 5000], id: \.self) { hundredths in
                        Button {
                            Task { await quickTip(room, hundredths: hundredths) }
                        } label: {
                            Text("\(hundredths / 100)")
                                .font(.system(size: 13, weight: .semibold).monospacedDigit())
                                .foregroundStyle(.white)
                                .frame(width: 40, height: 40)
                                .overlay(Circle().strokeBorder(.white.opacity(0.7), lineWidth: 1))
                                .frame(minHeight: F33Layout.minTouchTarget)
                                .contentShape(Circle())
                        }
                        .buttonStyle(.plain)
                        .accessibilityLabel("Tip \(hundredths / 100) AET")
                    }
                }
                .padding(.horizontal, 12)
                .padding(.bottom, 12)
            }

            if let error = tipError ?? room.lastError {
                Text(error)
                    .font(.footnote)
                    .foregroundStyle(Color(hex: 0xFF8A80))
                    .multilineTextAlignment(.center)
                    .padding(.horizontal, F33Spacing.lg)
                    .padding(.bottom, 8)
            }
        }
        .animation(F33Motion.easeOut, value: room.pinned)
        .animation(F33Motion.easeOut, value: chatHidden)
    }

    private func header(_ room: LiveRoomStore) -> some View {
        HStack(spacing: 10) {
            if let author = room.stream?.author {
                NavigationLink(value: Route.profile(handle: author.handle)) {
                    F33Avatar(author: author, size: 34)
                        .overlay(Circle().strokeBorder(.white.opacity(0.9), lineWidth: 1.5))
                }
                .buttonStyle(.plain)

                VStack(alignment: .leading, spacing: 1) {
                    Text("@\(author.handle)")
                        .font(.system(size: 14, weight: .semibold))
                        .foregroundStyle(.white)
                    Text([room.stream?.laneLabel, room.viewerCount == 1 ? "1 watching" : "\(Counts.exact(room.viewerCount)) watching"].compactMap { $0 }.joined(separator: " · "))
                        .font(.system(size: 11))
                        .foregroundStyle(.white.opacity(0.75))
                        .lineLimit(1)
                }

                if author.handle != model.state.user?.user.handle, let following {
                    Button { Task { await toggleFollow(author.handle, following) } } label: {
                        Text(following ? "Following" : "Follow")
                            .font(.system(size: 12, weight: .semibold))
                            .foregroundStyle(following ? .white.opacity(0.8) : .black)
                            .padding(.horizontal, 10)
                            .frame(height: 28)
                            .background(following ? .clear : .white, in: Capsule())
                            .overlay(Capsule().strokeBorder(.white.opacity(following ? 0.7 : 0), lineWidth: 1))
                            .frame(minHeight: F33Layout.minTouchTarget)
                            .contentShape(Capsule())
                    }
                    .buttonStyle(.plain)
                }
            }

            LiveBadgeMark(text: room.phase == .ended ? "ENDED" : "LIVE")

            Spacer(minLength: 0)

            Button { dismiss() } label: {
                Image(systemName: "xmark")
                    .font(.system(size: 17, weight: .semibold))
                    .foregroundStyle(.white)
                    .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Leave")
        }
    }

    private func rail(_ room: LiveRoomStore) -> some View {
        VStack(spacing: 10) {
            RailButton(icon: "heart.fill", label: "Send a heart", caption: Counts.label(room.heartCount), tint: F33Action.like) {
                floatHeart()
                Task { await room.heart() }
            }
            RailButton(icon: "bolt.fill", label: "Tip", filled: Color(hex: 0xFFE27A)) { isTipping = true }
            if let stream = room.stream {
                ShareLink(item: model.client.baseURL.appendingPathComponent("live/\(stream.id)")) {
                    VStack(spacing: 3) {
                        Image(systemName: "square.and.arrow.up")
                            .font(.system(size: 18, weight: .semibold))
                            .foregroundStyle(.white)
                            .frame(width: 48, height: 48)
                            .overlay(Circle().strokeBorder(.white.opacity(0.7), lineWidth: 1.5))
                    }
                }
                .accessibilityLabel("Share this room")
            }
            RailButton(icon: chatHidden ? "bubble.left.fill" : "bubble.left.and.exclamationmark.bubble.right", label: chatHidden ? "Show chat" : "Hide chat") {
                chatHidden.toggle()
            }
            Menu {
                if let handle = room.stream?.author.handle {
                    Button(role: .destructive) {
                        Task { try? await model.setMuted(true, handle: handle); dismiss() }
                    } label: {
                        Label("Mute @\(handle)", systemImage: "speaker.slash")
                    }
                }
            } label: {
                Image(systemName: "ellipsis")
                    .font(.system(size: 18, weight: .semibold))
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

    private func endedOverlay(_ room: LiveRoomStore) -> some View {
        VStack(spacing: F33Spacing.sm) {
            Text("This stream has ended")
                .font(.headline)
                .foregroundStyle(.white)
            Button("Back to Live") { dismiss() }
                .font(.subheadline.weight(.semibold))
                .foregroundStyle(.black)
                .padding(.horizontal, 18)
                .frame(height: F33Layout.minTouchTarget)
                .background(Color.white, in: Capsule())
        }
        .padding(F33Spacing.xl)
        .f33Glass(in: RoundedRectangle(cornerRadius: F33Radius.lg), elevation: .floating)
    }

    /// Hearts drift up from the rail and fade; the count itself is the
    /// server's.
    private var hearts: some View {
        ZStack {
            ForEach(floatingHearts, id: \.self) { id in
                FloatingHeart()
                    .id(id)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .bottomTrailing)
        .padding(.trailing, 36)
        .padding(.bottom, 220)
        .allowsHitTesting(false)
    }

    private func floatHeart() {
        let id = UUID()
        floatingHearts.append(id)
        Task {
            try? await Task.sleep(for: .seconds(1.6))
            floatingHearts.removeAll { $0 == id }
        }
    }

    private func send(_ room: LiveRoomStore) async {
        let text = draft
        draft = ""
        await room.send(text)
    }

    private func quickTip(_ room: LiveRoomStore, hundredths: Int) async {
        tipError = nil
        do { try await room.tip(amountHundredths: hundredths) } catch { tipError = room.lastError }
    }

    private func toggleFollow(_ handle: String, _ current: Bool) async {
        do {
            try await model.setFollowing(!current, handle: handle)
            following = try? await model.profile(handle: handle).viewerFollows
        } catch {
            tipError = (error as? MalkuthError)?.description ?? error.localizedDescription
        }
    }
}

private struct FloatingHeart: View {
    @State private var lifted = false
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        Image(systemName: "heart.fill")
            .font(.system(size: 22))
            .foregroundStyle(F33Action.like)
            .offset(x: lifted ? CGFloat.random(in: -30...30) : 0, y: lifted ? -160 : 0)
            .opacity(lifted ? 0 : 1)
            .onAppear {
                withAnimation(reduceMotion ? .easeOut(duration: 0.3) : .easeOut(duration: 1.5)) { lifted = true }
            }
    }
}

/// A tip inside a room, attributed to the stream so the goal moves.
struct LiveTipSheet: View {
    let room: LiveRoomStore
    let handle: String

    @Environment(\.dismiss) private var dismiss

    @State private var amountText = "5"
    @State private var isSending = false
    @State private var error: String?
    @State private var sent = false

    private var hundredths: Int? {
        guard let value = Double(amountText.replacingOccurrences(of: ",", with: ".")), value > 0 else { return nil }
        return Int((value * 100).rounded())
    }

    var body: some View {
        VStack(alignment: .leading, spacing: F33Spacing.lg) {
            Capsule().fill(F33Color.ink5).frame(width: 36, height: 5).frame(maxWidth: .infinity).padding(.top, F33Spacing.sm)
            Text("Tip @\(handle)")
                .font(.title3.weight(.semibold))
                .foregroundStyle(F33Color.ink)
            if room.tipGoalUAET > 0 {
                Text("\(AET.label(uAET: room.tipTotalUAET)) of \(AET.label(uAET: room.tipGoalUAET)) so far.")
                    .font(.footnote)
                    .foregroundStyle(F33Color.ink4)
            }
            HStack(spacing: F33Spacing.sm) {
                ForEach([1, 5, 20, 50], id: \.self) { aet in
                    Button { amountText = "\(aet)" } label: {
                        Text("\(aet)")
                            .font(.system(size: 15, weight: .semibold).monospacedDigit())
                            .foregroundStyle(amountText == "\(aet)" ? F33Color.accentInk : F33Color.ink2)
                            .frame(maxWidth: .infinity)
                            .frame(height: F33Layout.minTouchTarget)
                            .f33Glass(in: Capsule(), tint: amountText == "\(aet)" ? F33Action.tip : nil, interactive: true)
                    }
                    .buttonStyle(.plain)
                }
            }
            HStack(spacing: F33Spacing.sm) {
                TextField("Amount", text: $amountText).keyboardType(.decimalPad).f33Field()
                Text("AET").font(.system(size: 14, weight: .semibold)).foregroundStyle(F33Color.ink3)
            }
            if let error {
                Text(error).font(.footnote).foregroundStyle(F33Color.danger)
            }
            Spacer(minLength: 0)
            Button {
                if sent { dismiss() } else { Task { await send() } }
            } label: {
                if isSending { ProgressView().tint(F33Color.accentInk) }
                else if sent { Text("Done") }
                else if let hundredths { Text("Send \(AET.label(uAET: Int64(hundredths) * (AET.microsPerAET / 100)))") }
                else { Text("Send a tip") }
            }
            .buttonStyle(F33PrimaryButtonStyle())
            .disabled(hundredths == nil && !sent)
            .padding(.bottom, F33Spacing.lg)
        }
        .padding(.horizontal, F33Spacing.xl)
        .frame(maxWidth: .infinity, alignment: .leading)
        .f33GlassSheet()
        .presentationDetents([.height(380)])
    }

    private func send() async {
        guard let hundredths else { return }
        isSending = true
        defer { isSending = false }
        do {
            try await room.tip(amountHundredths: hundredths)
            sent = true
        } catch {
            self.error = room.lastError ?? error.localizedDescription
        }
    }
}
