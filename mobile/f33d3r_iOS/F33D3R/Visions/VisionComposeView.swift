import SwiftUI
import PhotosUI
import F33D3RKit

/// Writing a vision: camera-first, with a text card as the other mode.
///
/// Audience and lifetime are visible chips, not buried settings; a
/// subscribers-only vision is a gated one. On a device with a camera the photo
/// mode is a viewfinder with a shutter; on a Simulator it says there is no
/// camera and opens the photo library instead.
struct VisionComposeView: View {
    let onPosted: () -> Void

    @Environment(AppModel.self) private var model
    @Environment(\.dismiss) private var dismiss

    @State private var composer: VisionComposer?
    @State private var camera = CameraController()
    @State private var pickedItem: PhotosPickerItem?
    @State private var isPickingPhoto = false
    @FocusState private var isEditing: Bool

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()
            if let composer {
                content(composer)
            }
        }
        .statusBarHidden()
        .preferredColorScheme(.dark)
        .onAppear {
            if composer == nil { composer = VisionComposer(client: model.client) }
        }
        .onChange(of: composer?.mode) { _, mode in
            Task {
                if mode == .photo { await camera.start() } else { camera.stop() }
            }
        }
        .onDisappear { camera.stop() }
        .photosPicker(isPresented: $isPickingPhoto, selection: $pickedItem, matching: .images)
        .onChange(of: pickedItem) { _, item in
            guard let item, let composer else { return }
            pickedItem = nil
            Task {
                guard let data = try? await item.loadTransferable(type: Data.self) else { return }
                let (mime, ext) = ImageSniff.type(of: data)
                composer.mode = .photo
                await composer.attach(data, filename: "vision-\(UUID().uuidString.prefix(8)).\(ext)", mimeType: mime)
            }
        }
    }

    @ViewBuilder
    private func content(_ composer: VisionComposer) -> some View {
        @Bindable var composer = composer

        VStack(spacing: 0) {
            // Header
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
                Text("New vision")
                    .font(.system(size: 16, weight: .semibold))
                    .foregroundStyle(.white)
                Spacer()
                if composer.mode == .photo, camera.isRunning {
                    Button { Task { await camera.flip() } } label: {
                        Image(systemName: "arrow.triangle.2.circlepath.camera")
                            .font(.system(size: 16, weight: .semibold))
                            .foregroundStyle(.white)
                            .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel("Flip camera")
                } else {
                    Color.clear.frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                }
            }
            .padding(.horizontal, F33Spacing.md)
            .padding(.top, 8)

            // Stage
            ZStack(alignment: .topTrailing) {
                stage(composer)
                    .clipShape(RoundedRectangle(cornerRadius: F33Radius.lg))

                VStack(spacing: 10) {
                    if composer.mode == .text {
                        stageButton("textformat.size", label: "Typeface") {
                            let all = VisionArtboard.typefaces
                            let i = all.firstIndex(of: composer.typeface) ?? 0
                            composer.typeface = all[(i + 1) % all.count]
                        }
                        stageButton(alignIcon(composer.align), label: "Alignment") {
                            composer.align = ["center", "left", "right"].first { $0 != composer.align && ($0 == "left" ? composer.align == "center" : ($0 == "right" ? composer.align == "left" : true)) } ?? "center"
                        }
                        stageButton("chart.bar.xaxis", label: composer.pollOptions == nil ? "Add a poll" : "Remove poll", isOn: composer.pollOptions != nil) {
                            composer.togglePoll()
                        }
                    }
                }
                .padding(12)

                VStack {
                    Spacer()
                    if composer.mode == .text {
                        swatches(composer)
                            .padding(.bottom, 12)
                    }
                }
            }
            .padding(.horizontal, 12)
            .padding(.top, 12)
            .frame(maxHeight: .infinity)

            // Mode
            HStack(spacing: 8) {
                ForEach(VisionComposer.Mode.allCases, id: \.self) { mode in
                    Button { composer.mode = mode } label: {
                        Text(mode == .text ? "Text" : "Photo")
                            .font(.system(size: 13, weight: .semibold))
                            .foregroundStyle(composer.mode == mode ? .black : .white)
                            .padding(.horizontal, 14)
                            .frame(height: 32)
                            .background(composer.mode == mode ? Color.white : .clear, in: Capsule())
                            .overlay(Capsule().strokeBorder(.white.opacity(composer.mode == mode ? 0 : 0.7), lineWidth: 1))
                            .frame(minHeight: F33Layout.minTouchTarget)
                            .contentShape(Capsule())
                    }
                    .buttonStyle(.plain)
                    .accessibilityAddTraits(composer.mode == mode ? [.isSelected, .isButton] : .isButton)
                }
            }
            .padding(.top, 10)

            // Audience, lifetime, replies, 18+
            ScrollView(.horizontal, showsIndicators: false) {
                HStack(spacing: 8) {
                    Menu {
                        Picker("Audience", selection: $composer.audience) {
                            Label("Everyone", systemImage: "globe").tag("everyone")
                            Label("Subs only", systemImage: "lock").tag("subscribers")
                        }
                    } label: {
                        chip(composer.audience == "everyone" ? "Everyone ▾" : "Subs only ▾", tint: composer.audience == "everyone" ? .white : F33Color.ok)
                    }
                    Menu {
                        Picker("Lifetime", selection: $composer.ttlHours) {
                            ForEach(VisionComposer.lifetimes, id: \.hours) { life in
                                Text(life.label).tag(life.hours)
                            }
                        }
                    } label: {
                        chip((VisionComposer.lifetimes.first { $0.hours == composer.ttlHours }?.label ?? "24h") + " ▾", tint: .white)
                    }
                    Button { composer.allowReplies.toggle() } label: {
                        chip(composer.allowReplies ? "Allow replies ✓" : "Replies off", tint: .white)
                    }
                    .buttonStyle(.plain)
                    if model.state.user?.isAdult == true {
                        Button { composer.isNSFW.toggle() } label: {
                            chip(composer.isNSFW ? "18+ ✓" : "18+", tint: composer.isNSFW ? F33Color.danger : .white)
                        }
                        .buttonStyle(.plain)
                    }
                }
                .padding(.horizontal, F33Spacing.lg)
            }
            .padding(.top, 12)

            if let failure = composer.failure ?? composer.uploadFailure ?? camera.failure {
                Text(failure)
                    .font(.footnote)
                    .foregroundStyle(Color(hex: 0xFF8A80))
                    .multilineTextAlignment(.center)
                    .padding(.horizontal, F33Spacing.lg)
                    .padding(.top, 8)
            }

            // Roll · shutter · Post
            HStack(spacing: 18) {
                Button { isPickingPhoto = true } label: {
                    Group {
                        if let data = composer.imageData, let image = UIImage(data: data) {
                            Image(uiImage: image).resizable().aspectRatio(contentMode: .fill)
                        } else {
                            Image(systemName: "photo.on.rectangle")
                                .font(.system(size: 18))
                                .foregroundStyle(.white)
                        }
                    }
                    .frame(width: 44, height: 44)
                    .background(Color.white.opacity(0.15), in: RoundedRectangle(cornerRadius: 8))
                    .clipShape(RoundedRectangle(cornerRadius: 8))
                    .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(.white.opacity(0.5), lineWidth: 1))
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Choose from library")

                Spacer()

                if composer.mode == .photo, camera.isRunning {
                    Button {
                        Task {
                            guard let data = await camera.capture() else { return }
                            await composer.attach(data, filename: "vision-\(UUID().uuidString.prefix(8)).jpg", mimeType: "image/jpeg")
                        }
                    } label: {
                        Circle()
                            .strokeBorder(.white, lineWidth: 5)
                            .frame(width: 72, height: 72)
                            .overlay(Circle().fill(.white).padding(8))
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel("Take photo")
                } else {
                    Color.clear.frame(width: 72, height: 72)
                }

                Spacer()

                Button {
                    Task {
                        if let _ = await composer.submit(source: camera.isRunning ? "camera" : "composer") {
                            onPosted()
                            dismiss()
                        }
                    }
                } label: {
                    Group {
                        if composer.isSubmitting {
                            ProgressView().tint(.black)
                        } else {
                            Text("Post →").font(.system(size: 15, weight: .semibold))
                        }
                    }
                    .foregroundStyle(.black)
                    .padding(.horizontal, 18)
                    .frame(height: 44)
                    .background(Color.white, in: Capsule())
                    .opacity(composer.canSubmit ? 1 : 0.5)
                }
                .buttonStyle(.plain)
                .disabled(!composer.canSubmit)
            }
            .padding(.horizontal, F33Spacing.lg)
            .padding(.top, 14)
            .padding(.bottom, 12)
        }
    }

    // MARK: - Stage

    @ViewBuilder
    private func stage(_ composer: VisionComposer) -> some View {
        @Bindable var composer = composer
        switch composer.mode {
        case .text:
            ZStack {
                VisionBackground(preset: composer.background)
                VStack(alignment: .leading, spacing: 0) {
                    TextEditor(text: $composer.body)
                        .font(VisionType.font(typeface: composer.typeface, scale: scale(for: composer.body)))
                        .foregroundStyle(VisionBackground.isLight(composer.background) ? Color(hex: 0x231B13) : .white)
                        .multilineTextAlignment(VisionType.alignment(composer.align))
                        .scrollContentBackground(.hidden)
                        .tint(.white)
                        .focused($isEditing)
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                        .padding(.horizontal, 20)
                        .padding(.top, 60)
                        .padding(.bottom, composer.pollOptions == nil ? 80 : 20)
                        .overlay(alignment: .center) {
                            if composer.body.isEmpty {
                                Text("Say something.")
                                    .font(VisionType.font(typeface: composer.typeface, scale: "l"))
                                    .foregroundStyle((VisionBackground.isLight(composer.background) ? Color(hex: 0x231B13) : .white).opacity(0.45))
                                    .allowsHitTesting(false)
                            }
                        }
                    if let options = composer.pollOptions {
                        pollBuilder(composer, options: options)
                            .padding(.horizontal, 20)
                            .padding(.bottom, 90)
                    }
                }
            }
        case .photo:
            ZStack {
                Color(white: 0.08)
                if let data = composer.imageData, let image = UIImage(data: data) {
                    Image(uiImage: image)
                        .resizable()
                        .aspectRatio(contentMode: .fit)
                        .overlay(alignment: .topLeading) {
                            Button { composer.removeImage() } label: {
                                Label("Retake", systemImage: "arrow.uturn.backward")
                                    .font(.system(size: 13, weight: .semibold))
                                    .foregroundStyle(.white)
                                    .padding(.horizontal, 12)
                                    .frame(height: 34)
                                    .background(Color.black.opacity(0.5), in: Capsule())
                            }
                            .buttonStyle(.plain)
                            .padding(12)
                        }
                    if composer.isUploading {
                        ProgressView().tint(.white)
                    }
                } else if camera.isRunning {
                    CameraPreview(session: camera.session)
                } else if camera.isAvailable {
                    ProgressView().tint(.white)
                } else {
                    VStack(spacing: F33Spacing.md) {
                        Image(systemName: "camera.metering.none")
                            .font(.system(size: 34))
                            .foregroundStyle(.white.opacity(0.6))
                        Text("No camera on this device")
                            .font(.headline)
                            .foregroundStyle(.white)
                        Text("Pick a photo from the library instead.")
                            .font(.subheadline)
                            .foregroundStyle(.white.opacity(0.7))
                        Button("Choose photo") { isPickingPhoto = true }
                            .font(.subheadline.weight(.semibold))
                            .foregroundStyle(.black)
                            .padding(.horizontal, 18)
                            .frame(height: F33Layout.minTouchTarget)
                            .background(Color.white, in: Capsule())
                    }
                }
                if !composer.body.isEmpty || composer.imageData != nil {
                    VStack {
                        Spacer()
                        TextField("Add a caption", text: $composer.body)
                            .font(.system(size: 15, weight: .medium))
                            .foregroundStyle(.white)
                            .multilineTextAlignment(.center)
                            .padding(.horizontal, F33Spacing.md)
                            .frame(height: 40)
                            .background(Color.black.opacity(0.45), in: RoundedRectangle(cornerRadius: F33Radius.sm))
                            .padding(.horizontal, 40)
                            .padding(.bottom, 20)
                    }
                }
            }
        }
    }

    private func pollBuilder(_ composer: VisionComposer, options: [String]) -> some View {
        VStack(spacing: 6) {
            ForEach(options.indices, id: \.self) { i in
                HStack(spacing: 6) {
                    TextField("Option \(i + 1)", text: Binding(
                        get: { composer.pollOptions?[i] ?? "" },
                        set: { new in
                            guard var opts = composer.pollOptions, opts.indices.contains(i) else { return }
                            opts[i] = String(new.prefix(40))
                            composer.pollOptions = opts
                        }
                    ))
                    .font(.system(size: 14, weight: .medium))
                    .foregroundStyle(F33Color.ink)
                    .padding(.horizontal, 12)
                    .frame(height: 38)
                    .background(Color.white.opacity(0.92), in: Capsule())
                    if options.count > 2 {
                        Button { composer.removePollOption(at: i) } label: {
                            Image(systemName: "minus.circle.fill").foregroundStyle(.white.opacity(0.8))
                                .frame(width: 32, height: 38)
                        }
                        .buttonStyle(.plain)
                    }
                }
            }
            if options.count < 4 {
                Button { composer.addPollOption() } label: {
                    Label("Add option", systemImage: "plus").font(.system(size: 13, weight: .semibold)).foregroundStyle(.white)
                        .frame(height: 32)
                }
                .buttonStyle(.plain)
            }
        }
    }

    private func swatches(_ composer: VisionComposer) -> some View {
        HStack(spacing: 10) {
            ForEach(VisionBackground.names, id: \.0) { key, name in
                Button { composer.background = key } label: {
                    VisionBackground(preset: key)
                        .frame(width: 30, height: 30)
                        .clipShape(Circle())
                        .overlay(Circle().strokeBorder(.white, lineWidth: composer.background == key ? 3 : 1))
                        .frame(width: F33Layout.minTouchTarget, height: F33Layout.minTouchTarget)
                        .contentShape(Circle())
                }
                .buttonStyle(.plain)
                .accessibilityLabel(name)
                .accessibilityAddTraits(composer.background == key ? [.isSelected, .isButton] : .isButton)
            }
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 6)
        .background(Color.black.opacity(0.35), in: Capsule())
    }

    private func stageButton(_ icon: String, label: String, isOn: Bool = false, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            Image(systemName: icon)
                .font(.system(size: 15, weight: .semibold))
                .foregroundStyle(isOn ? .black : .white)
                .frame(width: 40, height: 40)
                .background(isOn ? Color.white : Color.black.opacity(0.35), in: Circle())
                .overlay(Circle().strokeBorder(.white.opacity(0.7), lineWidth: 1))
                .frame(minWidth: F33Layout.minTouchTarget, minHeight: F33Layout.minTouchTarget)
                .contentShape(Circle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel(label)
    }

    private func chip(_ text: String, tint: Color) -> some View {
        Text(text)
            .font(.system(size: 12, weight: .semibold))
            .foregroundStyle(tint)
            .lineLimit(1)
            .padding(.horizontal, 12)
            .frame(height: 32)
            .overlay(Capsule().strokeBorder(tint.opacity(0.8), lineWidth: 1))
            .frame(minHeight: F33Layout.minTouchTarget)
            .contentShape(Capsule())
    }

    private func alignIcon(_ align: String) -> String {
        switch align {
        case "left": return "text.alignleft"
        case "right": return "text.alignright"
        default: return "text.aligncenter"
        }
    }

    /// The composer previews at the scale the server will resolve `auto` to.
    private func scale(for body: String) -> String {
        switch body.count {
        case 0...40: return "xl"
        case 41...90: return "l"
        case 91...160: return "m"
        default: return "s"
        }
    }
}
