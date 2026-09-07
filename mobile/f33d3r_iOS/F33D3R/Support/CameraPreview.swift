import SwiftUI
import AVFoundation
import Observation

/// The device camera for the vision composer and the go-live screen.
///
/// One session, started on a background queue, shown through a preview layer.
/// On a Simulator — which has no camera — `isAvailable` is false and the
/// screens that use this say so and offer the photo library instead of
/// drawing a black box that pretends to be a viewfinder.
@MainActor
@Observable
final class CameraController: NSObject {
    let session = AVCaptureSession()

    private(set) var isAvailable = false
    private(set) var isRunning = false
    private(set) var isAuthorized = false
    private(set) var position: AVCaptureDevice.Position = .back
    private(set) var failure: String?

    private let queue = DispatchQueue(label: "com.f33d3r.ios.camera")
    private let photoOutput = AVCapturePhotoOutput()
    private var input: AVCaptureDeviceInput?
    private var captureContinuation: CheckedContinuation<Data?, Never>?

    override init() {
        super.init()
        isAvailable = AVCaptureDevice.default(.builtInWideAngleCamera, for: .video, position: .back) != nil
            || AVCaptureDevice.default(.builtInWideAngleCamera, for: .video, position: .front) != nil
    }

    /// Asks for permission if needed and starts the session.
    ///
    /// Configuration happens here on the main actor — adding inputs and
    /// outputs is quick — and only `startRunning`, which blocks, moves to the
    /// capture queue. That split is also what keeps the non-Sendable capture
    /// objects on one side of the isolation boundary.
    func start() async {
        guard isAvailable, !isRunning else { return }
        switch AVCaptureDevice.authorizationStatus(for: .video) {
        case .authorized:
            isAuthorized = true
        case .notDetermined:
            isAuthorized = await AVCaptureDevice.requestAccess(for: .video)
        default:
            isAuthorized = false
        }
        guard isAuthorized else {
            failure = "Camera access is off. Allow it in Settings to shoot a vision."
            return
        }
        session.beginConfiguration()
        session.sessionPreset = .photo
        if input == nil {
            if let device = AVCaptureDevice.default(.builtInWideAngleCamera, for: .video, position: position),
               let made = try? AVCaptureDeviceInput(device: device), session.canAddInput(made) {
                session.addInput(made)
                input = made
            }
            if session.canAddOutput(photoOutput) {
                session.addOutput(photoOutput)
            }
        }
        session.commitConfiguration()
        guard input != nil else {
            failure = "The camera could not be started."
            return
        }
        let session = self.session
        await withCheckedContinuation { (continuation: CheckedContinuation<Void, Never>) in
            queue.async {
                session.startRunning()
                continuation.resume()
            }
        }
        isRunning = session.isRunning
    }

    func stop() {
        guard isRunning else { return }
        let session = self.session
        queue.async { session.stopRunning() }
        isRunning = false
    }

    /// Switches between the front and back cameras.
    func flip() async {
        guard isRunning else { return }
        let next: AVCaptureDevice.Position = position == .back ? .front : .back
        guard let device = AVCaptureDevice.default(.builtInWideAngleCamera, for: .video, position: next),
              let made = try? AVCaptureDeviceInput(device: device) else { return }
        session.beginConfiguration()
        if let input { session.removeInput(input) }
        if session.canAddInput(made) {
            session.addInput(made)
            input = made
            position = next
        } else if let input, session.canAddInput(input) {
            session.addInput(input)
        }
        session.commitConfiguration()
    }

    /// Takes a photo and returns JPEG bytes.
    func capture() async -> Data? {
        guard isRunning else { return nil }
        return await withCheckedContinuation { continuation in
            captureContinuation = continuation
            let settings = AVCapturePhotoSettings(format: [AVVideoCodecKey: AVVideoCodecType.jpeg])
            photoOutput.capturePhoto(with: settings, delegate: self)
        }
    }
}

extension CameraController: AVCapturePhotoCaptureDelegate {
    nonisolated func photoOutput(_ output: AVCapturePhotoOutput, didFinishProcessingPhoto photo: AVCapturePhoto, error: Error?) {
        let data = error == nil ? photo.fileDataRepresentation() : nil
        Task { @MainActor in
            captureContinuation?.resume(returning: data)
            captureContinuation = nil
        }
    }
}

/// The preview layer as a view.
struct CameraPreview: UIViewRepresentable {
    let session: AVCaptureSession

    func makeUIView(context: Context) -> PreviewView {
        let view = PreviewView()
        view.previewLayer.session = session
        view.previewLayer.videoGravity = .resizeAspectFill
        return view
    }

    func updateUIView(_ uiView: PreviewView, context: Context) {}

    final class PreviewView: UIView {
        override class var layerClass: AnyClass { AVCaptureVideoPreviewLayer.self }
        var previewLayer: AVCaptureVideoPreviewLayer { layer as! AVCaptureVideoPreviewLayer }
    }
}
