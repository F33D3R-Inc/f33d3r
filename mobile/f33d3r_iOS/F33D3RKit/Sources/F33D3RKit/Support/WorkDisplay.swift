import Foundation

/// What stands between a reader and a work's content, and what it costs to pass.
///
/// One type rather than two booleans on the card, because the two gates are
/// mutually exclusive and their chips differ in wording, colour and what tapping
/// them will eventually do.
public enum WorkGate: Hashable, Sendable {
    /// Included in the author's subscription.
    case subscriber
    /// Sold on its own, at this price in micro-AET.
    case priced(uAET: Int64)

    /// The chip's text. "gated" leads in both cases so the two read as one
    /// family at a glance and the differing half comes second.
    public var label: String {
        switch self {
        case .subscriber:
            return "gated · sub"
        case .priced(let uAET):
            return "gated · \(AET.label(uAET: uAET))"
        }
    }
}

public extension Work {

    /// The short word under the handle, after the timestamp: "track", "video",
    /// "photo", "poll".
    ///
    /// Derived from what is actually attached, never from `content_type`. That
    /// column defaults to `'text'` and the insert path never writes it, so every
    /// row in the database claims to be text and the field cannot be read for
    /// anything. What a work *has* is trustworthy: a `video` makes it a video, a
    /// `voice` makes it a track, images make it a photo.
    ///
    /// Choosing a display noun from attachments already in hand is presentation
    /// and belongs here. Deriving what to rank or what to filter from the same
    /// fields would not, and does not happen.
    ///
    /// `kind` leads for the relational kinds, because "reply" tells the reader
    /// more about a reply-with-a-photo than "photo" does.
    var kindLabel: String {
        switch kind {
        case "reply": return "reply"
        case "quote": return "quote"
        case "thread_post": return "thread"
        case "react_video": return "react"
        default: break
        }

        if video != nil { return "video" }
        if voice != nil { return "track" }
        if poll != nil { return "poll" }
        if !mediaURLs.isEmpty { return "photo" }
        return "post"
    }

    /// The gate in front of this work, if any. Price wins over subscription:
    /// a work that can be bought outright is telling the reader something more
    /// specific than one that says "subscribe".
    ///
    /// A priced work the viewer has bought has no gate: the server says they
    /// own it, and a chip that still quotes the price would be asking them to
    /// pay again.
    var gate: WorkGate? {
        if let priceUAET, priceUAET > 0 {
            return purchasedByViewer ? nil : .priced(uAET: priceUAET)
        }
        if subscriberOnly { return .subscriber }
        return nil
    }

    /// Whether this work is sold outright — priced, whoever is looking.
    var isForSale: Bool { (priceUAET ?? 0) > 0 }

    /// Whether the work is something a music player can play: a voice
    /// recording or a video. Derived from the attachments, like `kindLabel`,
    /// and for the same reason — `content_type` cannot be trusted.
    var isPlayable: Bool { voice != nil || video != nil }

    /// The server path of the audio (or video) the player should load, when
    /// there is one. Voice first: a work with both is a track with a clip.
    var playbackPath: String? { voice?.url ?? video?.masterURL }

    /// How long the track runs, in seconds, as the server reported it.
    var playbackDurationSecs: Double { voice?.durationSecs ?? video?.durationSecs ?? 0 }

    /// The title a track is listed under: the first line of the body, with
    /// hashtags stripped, or the kind when the body is empty. Works have no
    /// title field, and the first line is where authors put one.
    var trackTitle: String {
        let firstLine = body.split(whereSeparator: \.isNewline).first.map(String.init) ?? ""
        let words = firstLine.split(separator: " ").filter { !$0.hasPrefix("#") }
        let title = words.joined(separator: " ").trimmingCharacters(in: .whitespacesAndNewlines)
        return title.isEmpty ? kindLabel.capitalized : title
    }

    /// The artwork a track is drawn with: the first attached image, else the
    /// video poster. Nil when the work has neither and the tile falls back to
    /// the author's palette.
    var artworkPath: String? { mediaURLs.first ?? video?.posterURL }

    /// What the tip control shows beside its icon — "12 AET" — or nil when
    /// nothing has been tipped and the control is just an invitation.
    var tipLabel: String? {
        guard let tipTotalUAET, tipTotalUAET > 0 else { return nil }
        return AET.label(uAET: tipTotalUAET)
    }

    /// The provenance to draw, or nil when the server said nothing — which is
    /// every response until the field ships.
    var visibleProvenance: WorkProvenance? {
        guard let provenance, !provenance.isEmpty else { return nil }
        return provenance
    }
}
