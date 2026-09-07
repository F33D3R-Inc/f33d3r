import SwiftUI
import AVFoundation
import Network
import F33D3RKit

/// Going live: the camera, a title, and every setting as a visible chip.
///
/// Network and microphone are checked before the button enables — a
/// broadcast that starts and drops in the first ten seconds is worse than a
/// button that waits. The countdown is three seconds and can be cancelled.
struct GoLiveSheet: View {
    let onStarted: (LiveStream) -> Void

    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    @State private var camera = CameraController()
    @State private var network = NetworkStatus()
    @State private var micAuthorized: Bool?
    @State private var request = LiveStartRequest()
    @State private var countdown: Int?
    @State private var isStarting = false
    @State private var failure: String?
    @FocusState private var isTitleFocused: Bool

    private static let goals = [50, 100, 200, 500]

    private var checksPass: Bool {
        network.isSatisfied && (micAuthorized ?? false)
    }

    private var canGo: Bool {
        request.isValid && checksPass && countdown == nil && !isStarting
    }

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()

            VStack(spacing: 0) {
                header
                    .padding(.horizontal, F33Spacing.md)
                    .padding(.top, 8)

                preview
                    .padding(.horizontal, 12)
                    .padding(.top, 12)

                ScrollView {
                    settingsCard
                        .padding(.horizontal, 12)
                        .padding(.top, 10)
                }
                .scrollDismissesKeyboard(.interactively)

                goButton
                    .padding(.horizontal, F33Spacing.lg)
                    .padding(.top, 10)
                    .padding(.bottom, 14)
            }
        }
        .statusBarHidden()
        .preferredColorScheme(.dark)
        .task {
            await camera.start()
            network.start()
            micAuthorized = await requestMicrophone()
        }
        .onDisappear {
            camera.stop()
            network.stop()
        }
    }

    private var header: some View {
        HStack(spacing: 10) {
            Button { dismiss() } label: {
                Image(systemName: "xmark")
                    .font(.system(size: 17, weight: .semibold))
                    .foregroundStyle(.white)
                    .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Close")
            Spacer()
            Text("Go live")
                .font(.system(size: 16, weight: .semibold))
                .foregroundStyle(.white)
            Spacer()
            if camera.isRunning {
                Button { Task { await camera.flip() } } label: {
                    Text("flip")
                        .font(.system(size: 12, weight: .semibold))
                        .foregroundStyle(.white)
                        .padding(.horizontal, 10)
                        .frame(height: 28)
                        .overlay(Capsule().strokeBorder(.white.opacity(0.8), lineWidth: 1))
                        .frame(minHeight: F33Layout.minTouchTarget)
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Flip camera")
            } else {
                Color.clear.frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
            }
        }
    }

    /// The viewfinder with the two checks pinned to its corner.
    private var preview: some View {
        ZStack(alignment: .topLeading) {
            Group {
                if camera.isRunning {
                    CameraPreview(session: camera.session)
                } else {
                    ZStack {
                        Color(white: 0.1)
                        VStack(spacing: 8) {
                            Image(systemName: "camera.metering.none")
                                .font(.system(size: 30))
                                .foregroundStyle(.white.opacity(0.6))
                            Text(camera.isAvailable ? (camera.failure ?? "Starting camera…") : "No camera on this device")
                                .font(.system(size: 13))
                                .foregroundStyle(.white.opacity(0.7))
                                .multilineTextAlignment(.center)
                                .padding(.horizontal, F33Spacing.lg)
                        }
                    }
                }
            }
            .frame(maxWidth: .infinity)
            .frame(height: 260)
            .clipShape(RoundedRectangle(cornerRadius: F33Radius.lg))

            HStack(spacing: 6) {
                checkChip(network.isSatisfied ? "network good" : "no network", ok: network.isSatisfied)
                checkChip(micLabel, ok: micAuthorized ?? false)
            }
            .padding(12)

            if let countdown {
                ZStack {
                    Color.black.opacity(0.55)
                    Text("\(countdown)")
                        .font(.system(size: 72, weight: .bold, design: .rounded).monospacedDigit())
                        .foregroundStyle(.white)
                        .contentTransition(.numericText(countsDown: true))
                }
                .clipShape(RoundedRectangle(cornerRadius: F33Radius.lg))
                .onTapGesture { self.countdown = nil }
                .accessibilityLabel("Going live in \(countdown). Tap to cancel.")
            }
        }
    }

    private var micLabel: String {
        switch micAuthorized {
        case .some(true): return "mic ✓"
        case .some(false): return "mic off"
        case .none: return "mic…"
        }
    }

    private func checkChip(_ text: String, ok: Bool) -> some View {
        Text(text)
            .font(.system(size: 11, weight: .semibold))
            .foregroundStyle(ok ? Color(hex: 0x7FD08A) : Color(hex: 0xFF8A80))
            .padding(.horizontal, 8)
            .padding(.vertical, 4)
            .background(Color.black.opacity(0.45), in: Capsule())
            .overlay(Capsule().strokeBorder((ok ? Color(hex: 0x7FD08A) : Color(hex: 0xFF8A80)).opacity(0.8), lineWidth: 1))
    }

    /// Title, audience, lane, notify, tips and goal, replay, and who you are.
    private var settingsCard: some View {
        VStack(alignment: .leading, spacing: 10) {
            TextField("Stream title…", text: $request.title)
                .font(.system(size: 15))
                .focused($isTitleFocused)
                .submitLabel(.done)
                .f33Field()

            FlowLayout(spacing: 8) {
                choiceChip("Everyone", isOn: request.audience == "everyone") { request.audience = "everyone" }
                choiceChip("Subs only", isOn: request.audience == "subscribers", tint: F33Color.ok) { request.audience = "subscribers" }
                choiceChip("Music lane", isOn: request.lane == "music") { request.lane = request.lane == "music" ? "" : "music" }
                choiceChip(request.notifyFollowers ? "Notify followers ✓" : "Notify followers", isOn: request.notifyFollowers) { request.notifyFollowers.toggle() }
            }

            FlowLayout(spacing: 8) {
                Menu {
                    Button("Tips off") { request.tipsEnabled = false }
                    ForEach(Self.goals, id: \.self) { goal in
                        Button("Goal \(goal) AET") { request.tipsEnabled = true; request.tipGoalAET = goal }
                    }
                } label: {
                    chipLabel(request.tipsEnabled ? "Tips ✓ · goal \(request.tipGoalAET) AET ▾" : "Tips off ▾", isOn: request.tipsEnabled)
                }
                choiceChip(request.saveReplay ? "Save replay ✓" : "Save replay", isOn: request.saveReplay) { request.saveReplay.toggle() }
                if model.state.user?.isAdult == true {
                    choiceChip(request.isNSFW ? "18+ ✓" : "18+", isOn: request.isNSFW, tint: F33Color.danger) { request.isNSFW.toggle() }
                }
            }

            if let user = model.state.user?.user {
                HStack(spacing: 8) {
                    F33Avatar(user: user, size: 22)
                    Text("@\(user.handle) · \(Counts.exact(user.followerCount)) follower\(user.followerCount == 1 ? "" : "s")")
                        .font(.system(size: 11))
                        .foregroundStyle(F33Color.ink3)
                        .lineLimit(1)
                    Text("safety scan on")
                        .font(.system(size: 10, weight: .semibold))
                        .foregroundStyle(F33Color.ok)
                        .padding(.horizontal, 6)
                        .padding(.vertical, 2)
                        .overlay(Capsule().strokeBorder(F33Color.ok.opacity(0.7), lineWidth: 1))
                    Spacer(minLength: 0)
                }
            }

            if let failure {
                Text(failure)
                    .font(.footnote)
                    .foregroundStyle(F33Color.danger)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .padding(14)
        .background(F33Color.bgElevated, in: RoundedRectangle(cornerRadius: 14))
        .environment(\.colorScheme, .light)
    }

    private func choiceChip(_ text: String, isOn: Bool, tint: Color = F33Color.ink, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            chipLabel(text, isOn: isOn, tint: tint)
        }
        .buttonStyle(.plain)
        .accessibilityAddTraits(isOn ? [.isSelected, .isButton] : .isButton)
    }

    private func chipLabel(_ text: String, isOn: Bool, tint: Color = F33Color.ink) -> some View {
        Text(text)
            .font(.system(size: 12, weight: .semibold))
            .foregroundStyle(isOn ? (tint == F33Color.ink ? F33Color.accentInk : .white) : tint)
            .lineLimit(1)
            .padding(.horizontal, 10)
            .frame(height: 30)
            .background(isOn ? tint : .clear, in: Capsule())
            .overlay(Capsule().strokeBorder(tint.opacity(isOn ? 0 : 0.5), lineWidth: 1))
            .frame(minHeight: F33Layout.minTouchTarget)
            .contentShape(Capsule())
    }

    private var goButton: some View {
        Button {
            Task { await startCountdown() }
        } label: {
            Group {
                if isStarting {
                    ProgressView().tint(.white)
                } else if let countdown {
                    Text("Going live · \(countdown)s")
                } else {
                    Text("Go live · 3s")
                }
            }
            .font(.system(size: 16, weight: .bold))
            .foregroundStyle(.white)
            .frame(maxWidth: .infinity)
            .frame(height: 52)
            .background(F33Color.danger, in: Capsule())
            .opacity(canGo || countdown != nil ? 1 : 0.45)
        }
        .buttonStyle(.plain)
        .disabled(!canGo && countdown == nil)
        .accessibilityLabel(canGo ? "Go live in three seconds" : "Go live, waiting on checks")
    }

    private func startCountdown() async {
        guard canGo else { return }
        isTitleFocused = false
        failure = nil
        for n in stride(from: 3, through: 1, by: -1) {
            withAnimation { countdown = n }
            try? await Task.sleep(for: .seconds(1))
            if countdown == nil { return }   // cancelled by tap
        }
        countdown = nil
        isStarting = true
        defer { isStarting = false }
        do {
            let stream = try await model.client.startLive(request)
            onStarted(stream)
        } catch let error as MalkuthError {
            if case .rejected(_, let reason) = error { failure = reason } else { failure = error.description }
        } catch let error as APIError {
            failure = error.userMessage
        } catch {
            failure = error.localizedDescription
        }
    }

    private func requestMicrophone() async -> Bool {
        if #available(iOS 17.0, *) {
            switch AVAudioApplication.shared.recordPermission {
            case .granted: return true
            case .denied: return false
            default: return await AVAudioApplication.requestRecordPermission()
            }
        }
        return false
    }
}

/// Whether the network is up, as NWPathMonitor sees it.
@MainActor
@Observable
final class NetworkStatus {
    private(set) var isSatisfied = false
    private let monitor = NWPathMonitor()

    func start() {
        monitor.pathUpdateHandler = { [weak self] path in
            Task { @MainActor in self?.isSatisfied = path.status == .satisfied }
        }
        monitor.start(queue: DispatchQueue(label: "com.f33d3r.ios.network"))
    }

    func stop() {
        monitor.cancel()
    }
}
