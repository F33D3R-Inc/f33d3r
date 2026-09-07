import SwiftUI
import AVKit

/// The system video player, over the app.
///
/// `AVPlayerViewController` presented on its own rather than embedded in a
/// cover of ours, because that is the only way it draws as the system player:
/// the Done button, the pull-down to dismiss, Picture in Picture, AirPlay and
/// the volume route all in its own chrome, with no close button of ours laid
/// over the top of them. AVKit's full-screen delegate callbacks fire only for
/// a controller presented this way, which is how the tile learns the reader
/// has closed it.
///
/// It is handed the player the tile is already running, so the picture carries
/// on from where it was rather than starting again, and the tile keeps that
/// player when the screen goes away. This view draws nothing itself; it sits
/// behind the tile as the place the presentation is made from.
struct SystemVideoPlayer: UIViewControllerRepresentable {
    let player: AVPlayer?
    /// True while the player is on screen. The reader's Done sets it false;
    /// Picture in Picture returning to the app sets it true again.
    @Binding var isPresented: Bool
    /// True while the picture is in the system's floating window.
    @Binding var isInPictureInPicture: Bool

    func makeUIViewController(context: Context) -> Presenter {
        Presenter()
    }

    func updateUIViewController(_ presenter: Presenter, context: Context) {
        presenter.onPresentationChange = { isPresented = $0 }
        presenter.onPictureInPictureChange = { isInPictureInPicture = $0 }
        presenter.setPresented(isPresented, player: player)
    }

    /// An empty controller that owns the presentation and hears back from it.
    ///
    /// The delegate conformance is main-actor isolated: AVKit calls back on
    /// the main thread, and the presenter's job is UI work.
    final class Presenter: UIViewController, @MainActor AVPlayerViewControllerDelegate {
        var onPresentationChange: ((Bool) -> Void)?
        var onPictureInPictureChange: ((Bool) -> Void)?

        /// Kept while presented and, after an automatic dismissal into Picture
        /// in Picture, until the picture comes back or stops — it is the same
        /// controller that is put back on screen.
        private var playerController: AVPlayerViewController?
        /// The player waiting to be shown when the view is not yet in a window
        /// to present from; `viewDidAppear` finishes the job.
        private var pendingPlayer: AVPlayer?
        /// Set from the will-start callback, which precedes the automatic
        /// dismissal, so that dismissal can tell itself apart from Done.
        private var isPictureInPictureActive = false

        override func viewDidLoad() {
            super.viewDidLoad()
            view.backgroundColor = .clear
            view.isUserInteractionEnabled = false
        }

        override func viewDidAppear(_ animated: Bool) {
            super.viewDidAppear(animated)
            if let pendingPlayer { present(playing: pendingPlayer) }
        }

        func setPresented(_ presented: Bool, player: AVPlayer?) {
            if presented {
                guard playerController?.presentingViewController == nil, let player else { return }
                present(playing: player)
            } else {
                pendingPlayer = nil
                guard let controller = playerController, controller.presentingViewController != nil else { return }
                controller.dismiss(animated: true)
            }
        }

        private func present(playing player: AVPlayer) {
            guard view.window != nil else {
                pendingPlayer = player
                return
            }
            pendingPlayer = nil
            let controller = playerController ?? makeController()
            controller.player = player
            playerController = controller
            present(controller, animated: true)
        }

        private func makeController() -> AVPlayerViewController {
            let controller = AVPlayerViewController()
            controller.delegate = self
            controller.showsPlaybackControls = true
            controller.allowsPictureInPicturePlayback = true
            controller.canStartPictureInPictureAutomaticallyFromInline = true
            controller.entersFullScreenWhenPlaybackBegins = false
            controller.modalPresentationStyle = .fullScreen
            return controller
        }

        // MARK: AVPlayerViewControllerDelegate

        func playerViewController(
            _ playerViewController: AVPlayerViewController,
            willEndFullScreenPresentationWithAnimationCoordinator coordinator: any UIViewControllerTransitionCoordinator
        ) {
            onPresentationChange?(false)
            // Dismissed into Picture in Picture, the controller is kept for
            // the way back; dismissed by Done, it has nothing more to do.
            if !isPictureInPictureActive {
                playerController = nil
            }
        }

        func playerViewControllerWillStartPictureInPicture(_ playerViewController: AVPlayerViewController) {
            isPictureInPictureActive = true
            onPictureInPictureChange?(true)
        }

        func playerViewControllerDidStopPictureInPicture(_ playerViewController: AVPlayerViewController) {
            isPictureInPictureActive = false
            onPictureInPictureChange?(false)
            if playerViewController.presentingViewController == nil {
                playerController = nil
            }
        }

        func playerViewControllerShouldAutomaticallyDismissAtPictureInPictureStart(_ playerViewController: AVPlayerViewController) -> Bool {
            true
        }

        func playerViewController(
            _ playerViewController: AVPlayerViewController,
            restoreUserInterfaceForPictureInPictureStopWithCompletionHandler completionHandler: @escaping (Bool) -> Void
        ) {
            // The floating picture was tapped back into the app: the same
            // controller goes back up, full screen, and the tile is told.
            guard playerViewController.presentingViewController == nil, view.window != nil else {
                completionHandler(false)
                return
            }
            onPresentationChange?(true)
            present(playerViewController, animated: true) { completionHandler(true) }
        }
    }
}
