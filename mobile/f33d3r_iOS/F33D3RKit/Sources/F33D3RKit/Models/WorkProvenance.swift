import Foundation

/// Why a work reached this reader.
///
/// The ranking engine already knows the answer — it chose the row — and this is
/// the sanitised half of that answer. `text` is a sentence the server composed;
/// the app prints it verbatim and never assembles one of its own, because a
/// sentence written on device would be the client inventing an account of a
/// decision it did not make. `kind` exists so an icon can be chosen without
/// parsing prose.
///
/// The server field is designed but not built. It is optional everywhere it
/// appears, and a card with no provenance draws no chip at all.
public struct WorkProvenance: Codable, Hashable, Sendable {
    /// reposted | you_follow | circle | surface | popular. A closed vocabulary
    /// the app may branch on; an unknown value draws the neutral icon rather
    /// than nothing, so a token added server-side needs no client release.
    public let kind: String

    /// The whole reason, as the server wrote it: "You follow @dev". Drawn as
    /// given, in the chip and in the sheet behind it.
    public let text: String

    /// The person the reason is about, when there is one — so the sheet can
    /// offer their profile without picking a handle back out of `text`.
    public let handle: String?

    public init(kind: String, text: String, handle: String? = nil) {
        self.kind = kind
        self.text = text
        self.handle = handle
    }

    public init(from decoder: any Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        kind = try c.decodeIfPresent(String.self, forKey: .kind) ?? ""
        text = try c.decodeIfPresent(String.self, forKey: .text) ?? ""
        handle = try c.decodeIfPresent(String.self, forKey: .handle)
    }

    /// A provenance with nothing to say draws no chip.
    public var isEmpty: Bool { text.isEmpty }
}
