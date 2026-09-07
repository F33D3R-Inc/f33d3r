import SwiftUI
import F33D3RKit

/// The creator's side of a room: their camera full-bleed, the tip goal and
/// top tippers (theirs alone), the chat, and a rail of 48pt targets.
///
/// Ending shows a summary — earned, peak viewers, new followers — and only
/// then dismisses, because the moment after a stream is when a creator wants
/// to know how it went.
struct BroadcasterView: View {
    let stream: LiveStream

    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    @State private var room: LiveRoomStore?
    @State private var camera = CameraController()
    @State private var draft = ""
    @State private var isPinning = false
    @State private var pinDraft = ""
    @State private var isConfirmingEnd = false
    @State private var summary: LiveSummary?
    @State private var startedAt = Date()
    @State private var endError: String?

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()

            if camera.isRunning {
                CameraPreview(session: camera.session).ignoresSafeArea()
            } else {
                stageFallback
            }

            LinearGradient(colors: [.black.opacity(0.55), .clear, .clear, .black.opacity(0.6)], startPoint: .top, endPoint: .bottom)
                .ignoresSafeArea()
                .allowsHitTesting(false)

            if let room {
                overlay(room)
            }
        }
        .statusBarHidden()
        .preferredColorScheme(.dark)
        .task {
            if room == nil {
                let store = model.liveRoom(id: stream.id)
                room = store
                await store.connect()
            }
            startedAt = stream.startedAt ?? Date()
            await camera.start()
        }
        .onDisappear {
            camera.stop()
            room?.disconnect()
        }
        .confirmationDialog("End the broadcast?", isPresented: $isConfirmingEnd, titleVisibility: .visible) {
            Button("End", role: .destructive) { Task { await end() } }
        } message: {
            Text("Viewers see the stream end. Tips already received stay yours.")
        }
        .alert("Pin a line", isPresented: $isPinning) {
            TextField("What tips unlock, or a note for the room", text: $pinDraft)
            Button("Pin") { Task { await room?.pin(pinDraft) } }
            Button("Clear", role: .destructive) { pinDraft = ""; Task { await room?.pin("") } }
            Button("Cancel", role: .cancel) {}
        }
        .sheet(item: $summary, onDismiss: { dismiss() }) { summary in
            LiveSummarySheet(summary: summary, title: stream.title)
        }
    }

    private var stageFallback: some View {
        ZStack {
            Color(white: 0.08)
            VStack(spacing: 8) {
                Image(systemName: "camera.metering.none")
                    .font(.system(size: 30))
                    .foregroundStyle(.white.opacity(0.5))
                Text(camera.isAvailable ? (camera.failure ?? "Starting camera…") : "No camera on this device")
                    .font(.system(size: 13))
                    .foregroundStyle(.white.opacity(0.7))
            }
        }
        .ignoresSafeArea()
    }

    @ViewBuilder
    private func overlay(_ room: LiveRoomStore) -> some View {
        VStack(spacing: 0) {
            topChips(room)
                .padding(.horizontal, 12)
                .padding(.top, 8)

            if room.tipGoalUAET > 0 {
                TipGoalCard(totalUAET: room.tipTotalUAET, goalUAET: room.tipGoalUAET, topTippers: room.stream?.topTippers ?? [])
                    .padding(.horizontal, 12)
                    .padding(.top, 10)
            }

            Spacer(minLength: 0)

            HStack(alignment: .bottom, spacing: 10) {
                LiveChatBubbles(messages: room.chat)
                VStack(spacing: 10) {
                    RailButton(icon: "pin", label: "Pin a line") { pinDraft = room.pinned ?? ""; isPinning = true }
                    RailButton(icon: "arrow.triangle.2.circlepath.camera", label: "Flip camera") { Task { await camera.flip() } }
                    RailButton(icon: "heart.fill", label: "Hearts", caption: Counts.label(room.heartCount), tint: F33Action.like) {}
                        .allowsHitTesting(false)
                }
            }
            .padding(.horizontal, 12)
            .padding(.bottom, 10)

            HStack(spacing: 10) {
                LiveChatComposer(text: $draft) { Task { await send(room) } }
                Button { isConfirmingEnd = true } label: {
                    Text("End")
                        .font(.system(size: 15, weight: .bold))
                        .foregroundStyle(.black)
                        .padding(.horizontal, 18)
                        .frame(height: 48)
                        .background(Color.white, in: Capsule())
                }
                .buttonStyle(.plain)
            }
            .padding(.horizontal, 12)
            .padding(.bottom, 12)

            if let error = room.lastError ?? endError {
                Text(error)
                    .font(.footnote)
                    .foregroundStyle(Color(hex: 0xFF8A80))
                    .padding(.bottom, 8)
            }
        }
    }

    private func topChips(_ room: LiveRoomStore) -> some View {
        HStack(spacing: 6) {
            TimelineView(.periodic(from: startedAt, by: 1)) { context in
                LiveBadgeMark(text: "LIVE \(elapsed(context.date))")
            }
            HStack(spacing: 4) {
                Image(systemName: "eye")
                Text("\(room.viewerCount)")
            }
            .font(.system(size: 11, weight: .semibold).monospacedDigit())
            .foregroundStyle(.white)
            .padding(.horizontal, 8)
            .padding(.vertical, 4)
            .background(Color.black.opacity(0.45), in: Capsule())
            .overlay(Capsule().strokeBorder(.white.opacity(0.8), lineWidth: 1))
            .accessibilityLabel("\(room.viewerCount) watching")
            chip(room.isConnected ? "good" : "reconnecting", tint: room.isConnected ? Color(hex: 0x7FD08A) : Color(hex: 0xFF8A80))
            Spacer(minLength: 0)
            Button { isConfirmingEnd = true } label: {
                Image(systemName: "xmark")
                    .font(.system(size: 17, weight: .semibold))
                    .foregroundStyle(.white)
                    .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("End broadcast")
        }
    }

    private func chip(_ text: String, tint: Color) -> some View {
        Text(text)
            .font(.system(size: 11, weight: .semibold).monospacedDigit())
            .foregroundStyle(tint)
            .padding(.horizontal, 8)
            .padding(.vertical, 4)
            .background(Color.black.opacity(0.45), in: Capsule())
            .overlay(Capsule().strokeBorder(tint.opacity(0.8), lineWidth: 1))
    }

    private func elapsed(_ now: Date) -> String {
        let secs = max(0, Int(now.timeIntervalSince(startedAt)))
        return String(format: "%d:%02d", secs / 60, secs % 60)
    }

    private func send(_ room: LiveRoomStore) async {
        let text = draft
        draft = ""
        await room.send(text)
    }

    private func end() async {
        guard let room else { return }
        do {
            summary = try await room.end()
        } catch let error as MalkuthError {
            endError = error.description
        } catch {
            endError = error.localizedDescription
        }
    }
}

/// What the creator sees when the stream ends.
struct LiveSummarySheet: View {
    let summary: LiveSummary
    let title: String

    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(alignment: .leading, spacing: F33Spacing.lg) {
            Capsule().fill(F33Color.ink5).frame(width: 36, height: 5).frame(maxWidth: .infinity).padding(.top, F33Spacing.sm)

            VStack(alignment: .leading, spacing: 4) {
                Text("Stream ended")
                    .font(.title2.weight(.semibold))
                    .foregroundStyle(F33Color.ink)
                Text(title)
                    .font(.callout)
                    .foregroundStyle(F33Color.ink3)
            }

            LazyVGrid(columns: [GridItem(.flexible()), GridItem(.flexible())], spacing: F33Spacing.md) {
                stat(AET.label(uAET: summary.tipsUAET), "earned in tips", tint: F33Action.tip)
                stat(duration, "live")
                stat(Counts.exact(summary.peakViewers), "peak viewers")
                stat(Counts.exact(summary.newFollowers), "new followers")
                stat(Counts.exact(summary.chatLines), "chat lines")
                stat(Counts.exact(summary.hearts), "hearts")
            }

            Label(summary.replaySaved ? "Replay saved to your profile" : "No replay kept",
                  systemImage: summary.replaySaved ? "checkmark.circle" : "circle.slash")
                .font(.footnote)
                .foregroundStyle(F33Color.ink4)

            Spacer(minLength: 0)

            Button("Done") { dismiss() }
                .buttonStyle(F33PrimaryButtonStyle())
                .padding(.bottom, F33Spacing.lg)
        }
        .padding(.horizontal, F33Spacing.xl)
        .frame(maxWidth: .infinity, alignment: .leading)
        .f33GlassSheet()
        .presentationDetents([.height(480)])
        .presentationDragIndicator(.hidden)
        .interactiveDismissDisabled()
    }

    private var duration: String {
        let s = Int(summary.durationSecs)
        if s >= 3600 { return String(format: "%d:%02d:%02d", s / 3600, (s % 3600) / 60, s % 60) }
        return String(format: "%d:%02d", s / 60, s % 60)
    }

    private func stat(_ value: String, _ label: String, tint: Color = F33Color.ink) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(value)
                .font(.system(size: 22, weight: .semibold).monospacedDigit())
                .foregroundStyle(tint)
                .lineLimit(1)
                .minimumScaleFactor(0.7)
            Text(label)
                .font(.system(size: 12))
                .foregroundStyle(F33Color.ink4)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(F33Spacing.md)
        .background(F33Color.bgElevated, in: RoundedRectangle(cornerRadius: F33Radius.md))
        .accessibilityElement(children: .combine)
    }
}

extension LiveSummary: Identifiable {
    public var id: String { "\(durationSecs)-\(tipsUAET)-\(peakViewers)" }
}
