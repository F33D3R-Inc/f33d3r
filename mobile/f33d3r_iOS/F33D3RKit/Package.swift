// swift-tools-version: 6.0
import PackageDescription

// F33D3RKit is the non-UI half of the iOS app: models, networking, session
// storage, crypto and design tokens. It is a separate package so this layer
// builds and tests headlessly with `swift build` / `swift test`, without a
// simulator — which is where the parts that must be exactly right (canonical
// JSON, content signing, DTO decoding) actually get verified.
let package = Package(
    name: "F33D3RKit",
    platforms: [.iOS(.v17), .macOS(.v14)],
    products: [
        .library(name: "F33D3RKit", targets: ["F33D3RKit"])
    ],
    targets: [
        .target(name: "F33D3RKit"),
        .testTarget(
            name: "F33D3RKitTests",
            dependencies: ["F33D3RKit"],
            // Fixtures are generated from the Go DTOs by
            // TestWriteAPIV1GoldenFixtures in feed-engine. They make the
            // client/server contract a checked-in artifact both sides test
            // against, so a rename on one side fails the other side's build.
            resources: [.copy("Fixtures")]
        ),
    ]
)
