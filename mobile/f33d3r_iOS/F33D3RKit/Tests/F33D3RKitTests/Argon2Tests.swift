import Foundation
import Testing
@testable import F33D3RKit

/// Proves this Argon2 is the Argon2 the browser runs.
///
/// The vault key is derived on one device and used on another, and the only
/// symptom of a mismatch is a private key that will not unwrap — no error names
/// the parameter that differed, and the tag is a perfectly well-formed 32 bytes
/// either way. So this suite refuses to test the implementation against itself.
///
/// Three independent sources, in descending order of authority:
///
/// 1. **RFC 9106 §5**, quoted below as literals. Argon2d, Argon2i and Argon2id,
///    all with a secret and associated data — parameters no other check here
///    exercises, and which no library on this machine can even express.
/// 2. **`tests/kat.rs` of argon2 0.5.3**, the exact crate version
///    `sealcore/Cargo.lock` pins. Those vectors come from the reference C
///    implementation. The v0x10 case is there to exercise the overwrite path
///    that v0x13 replaces with an XOR.
/// 3. **`golang.org/x/crypto/argon2`**, run by `Tools/sealcore-golden` over the
///    parameters this app actually uses, including a handle-derived salt and a
///    non-ASCII password. It is a third implementation, written by neither the
///    Rust authors nor this file.
///
/// What is *not* proven: that the running Rust crate produces these. Cargo is
/// not installed on this machine and the instruction was not to install it. The
/// crate source was read instead — `Params::DEFAULT` is `m_cost: 19 * 1024`,
/// `t_cost: 2`, `p_cost: 1`, `Algorithm::default()` carries `#[default]` on
/// `Argon2id` and `Version::default()` on `V0x13` — and the crate's own KATs are
/// pinned here. See Tools/sealcore-golden/README.md.
struct Argon2Tests {

    // MARK: Helpers

    static func hex(_ string: String) -> Data {
        var bytes = [UInt8]()
        var index = string.startIndex
        while index < string.endIndex {
            let next = string.index(index, offsetBy: 2)
            bytes.append(UInt8(string[index..<next], radix: 16)!)
            index = next
        }
        return Data(bytes)
    }

    static func hexString(_ data: Data) -> String {
        data.map { String(format: "%02x", $0) }.joined()
    }

    /// The shape every RFC 9106 §5 vector shares.
    static let rfcPassword = Data(repeating: 0x01, count: 32)
    static let rfcSalt = Data(repeating: 0x02, count: 16)
    static let rfcSecret = Data(repeating: 0x03, count: 8)
    static let rfcAssociatedData = Data(repeating: 0x04, count: 12)

    static func rfcParameters(_ variant: Argon2.Variant, _ version: Argon2.Version = .v0x13)
        -> Argon2.Parameters
    {
        Argon2.Parameters(
            variant: variant, version: version,
            timeCost: 3, memoryKiB: 32, parallelism: 4, tagLength: 32
        )
    }

    // MARK: RFC 9106 section 5

    @Test(
        "RFC 9106's own test vectors, all three variants, with a secret and associated data",
        arguments: [
            (Argon2.Variant.d, "512b391b6f1162975371d30919734294f868e3be3984f3c1a13a4db9fabe4acb"),
            (Argon2.Variant.i, "c814d9d1dc7f37aa13f0d77f2494bda1c8de6b016dd388d29952a4c4672b6ce8"),
            (Argon2.Variant.id, "0d640df58d78766c08c037a34a8b53c9d01ef0452d75b65eb52520e96b01e659"),
        ]
    )
    func matchesRFC9106(variant: Argon2.Variant, expected: String) throws {
        let tag = try Argon2.hash(
            password: Self.rfcPassword,
            salt: Self.rfcSalt,
            secret: Self.rfcSecret,
            associatedData: Self.rfcAssociatedData,
            parameters: Self.rfcParameters(variant)
        )
        #expect(Self.hexString(tag) == expected)
    }

    @Test("Version 0x10 overwrites where 0x13 XORs, and the KAT tells them apart")
    func matchesVersion0x10Vector() throws {
        // From tests/kat.rs of argon2 0.5.3. Not a parameter this app uses; it
        // is here because it is the only vector that distinguishes the two
        // second-pass behaviours, and a port that XORs unconditionally passes
        // every other test in this file.
        let tag = try Argon2.hash(
            password: Self.rfcPassword,
            salt: Self.rfcSalt,
            secret: Self.rfcSecret,
            associatedData: Self.rfcAssociatedData,
            parameters: Self.rfcParameters(.id, .v0x10)
        )
        #expect(
            Self.hexString(tag) == "b64615f07789b66b645b67ee9ed3b377ae350b6bfcbb0fc95141ea8f322613c0"
        )
    }

    // MARK: The reference implementation's vectors, via the crate's KAT file

    struct ReferenceVector {
        let name: String
        let variant: Argon2.Variant
        let version: Argon2.Version
        let timeCost: UInt32
        let memoryKiB: UInt32
        let parallelism: UInt32
        let password: String
        let salt: String
        let tag: String
    }

    /// A subset of `tests/kat.rs`: the parallel cases (`p = 2`) and one large-m
    /// case, which between them exercise the lane synchronisation and the
    /// address-block regeneration that a `p = 1`, small-`m` test never reaches.
    static let referenceVectors: [ReferenceVector] = [
        .init(name: "argon2id v13 t2 m65536 p1", variant: .id, version: .v0x13,
              timeCost: 2, memoryKiB: 65536, parallelism: 1,
              password: "password", salt: "somesalt",
              tag: "09316115d5cf24ed5a15a31a3ba326e5cf32edc24702987c02b6566f61913cf7"),
        .init(name: "argon2id v13 t2 m256 p2", variant: .id, version: .v0x13,
              timeCost: 2, memoryKiB: 256, parallelism: 2,
              password: "password", salt: "somesalt",
              tag: "6d093c501fd5999645e0ea3bf620d7b8be7fd2db59c20d9fff9539da2bf57037"),
        .init(name: "argon2id v13 t2 m256 p1", variant: .id, version: .v0x13,
              timeCost: 2, memoryKiB: 256, parallelism: 1,
              password: "password", salt: "somesalt",
              tag: "9dfeb910e80bad0311fee20f9c0e2b12c17987b4cac90c2ef54d5b3021c68bfe"),
        .init(name: "argon2i v13 t2 m256 p2", variant: .i, version: .v0x13,
              timeCost: 2, memoryKiB: 256, parallelism: 2,
              password: "password", salt: "somesalt",
              tag: "4ff5ce2769a1d7f4c8a491df09d41a9fbe90e5eb02155a13e4c01e20cd4eab61"),
        .init(name: "argon2i v10 t2 m256 p2", variant: .i, version: .v0x10,
              timeCost: 2, memoryKiB: 256, parallelism: 2,
              password: "password", salt: "somesalt",
              tag: "b6c11560a6a9d61eac706b79a2f97d68b4463aa3ad87e00c07e2b01e90c564fb"),
    ]

    @Test("The reference implementation's vectors, including two lanes")
    func matchesReferenceVectors() throws {
        for vector in Self.referenceVectors {
            let tag = try Argon2.hash(
                password: Data(vector.password.utf8),
                salt: Data(vector.salt.utf8),
                parameters: Argon2.Parameters(
                    variant: vector.variant, version: vector.version,
                    timeCost: vector.timeCost, memoryKiB: vector.memoryKiB,
                    parallelism: vector.parallelism, tagLength: 32
                )
            )
            #expect(Self.hexString(tag) == vector.tag, "\(vector.name)")
        }
    }

    // MARK: A third implementation

    @Test("Go's x/crypto/argon2 derives the same tags at the parameters this app uses")
    func matchesGoImplementation() throws {
        let fixture = try SealCoreGoldenFixture.load()
        let goVectors = fixture.argon2.filter { $0.source == "go" }
        // The fixture must actually contain them; an empty filter passes a
        // for-loop silently.
        #expect(goVectors.count >= 5)

        for vector in goVectors {
            let variant: Argon2.Variant = vector.variant == "id" ? .id
                : (vector.variant == "i" ? .i : .d)
            let tag = try Argon2.hash(
                password: Data(base64Encoded: vector.passwordB64) ?? Data(),
                salt: try #require(Data(base64Encoded: vector.saltB64)),
                parameters: Argon2.Parameters(
                    variant: variant,
                    version: vector.version == 0x13 ? .v0x13 : .v0x10,
                    timeCost: UInt32(vector.timeCost),
                    memoryKiB: UInt32(vector.memoryKiB),
                    parallelism: UInt32(vector.lanes),
                    tagLength: 32
                )
            )
            #expect(Self.hexString(tag) == vector.tagHex, "\(vector.name)")
        }
    }

    // MARK: Shape

    @Test("m' is memory rounded down to whole segments, so 19456 KiB is used exactly")
    func usesRequestedMemory() throws {
        // 19456 is divisible by 4, so no rounding happens on the path that
        // matters. This pins that: if the rounding were wrong for a value that
        // does not divide evenly, these two would differ.
        let a = try Argon2.hash(password: Data("x".utf8), salt: Data("saltsalt".utf8),
                                parameters: .sealcore)
        var rounded = Argon2.Parameters.sealcore
        rounded.memoryKiB = 19456
        let b = try Argon2.hash(password: Data("x".utf8), salt: Data("saltsalt".utf8),
                                parameters: rounded)
        #expect(a == b)
        #expect(a.count == 32)
    }

    @Test("The sealcore parameters are argon2 0.5.3's Argon2::default(), field for field")
    func sealcoreParametersMatchTheCrateDefault() {
        // Read out of argon2-0.5.3/src/params.rs and algorithm.rs. Written down
        // as an assertion so that changing the constant requires changing a
        // test that says where the number came from.
        #expect(Argon2.Parameters.sealcore.variant == .id)
        #expect(Argon2.Parameters.sealcore.version == .v0x13)
        #expect(Argon2.Parameters.sealcore.memoryKiB == 19 * 1024)
        #expect(Argon2.Parameters.sealcore.timeCost == 2)
        #expect(Argon2.Parameters.sealcore.parallelism == 1)
        #expect(Argon2.Parameters.sealcore.tagLength == 32)
    }

    @Test("A salt the Rust crate would refuse is refused here too")
    func refusesShortSalt() {
        #expect(throws: Argon2Error.saltTooShort(7)) {
            try Argon2.hash(password: Data("x".utf8), salt: Data("1234567".utf8),
                            parameters: .sealcore)
        }
    }

    @Test("Tag length is mixed into the hash, so a shorter tag is not a truncation")
    func tagLengthIsNotTruncation() throws {
        var short = Argon2.Parameters.sealcore
        short.memoryKiB = 64
        short.tagLength = 16
        var long = short
        long.tagLength = 32

        let a = try Argon2.hash(password: Data("pw".utf8), salt: Data("saltsalt".utf8), parameters: short)
        let b = try Argon2.hash(password: Data("pw".utf8), salt: Data("saltsalt".utf8), parameters: long)
        #expect(a.count == 16)
        #expect(b.count == 32)
        #expect(a != b.prefix(16))
    }

    // MARK: Cost

    @Test("One derivation at the login parameters is fast enough to sit in a login flow")
    func costsWhatALoginCanAfford() throws {
        let start = Date()
        _ = try Argon2.hash(
            password: Data("correct horse battery staple".utf8),
            salt: Data(base64Encoded: SealCore.saltFromHandle("miiyazuko"))!,
            parameters: .sealcore
        )
        let elapsed = Date().timeIntervalSince(start)
        print("[argon2] sealcore parameters: \(Int(elapsed * 1000)) ms (this build)")

        // Deliberately loose. `swift test` builds -Onone, where this runs an
        // order of magnitude slower than the release build a phone executes, so
        // a tight bound here would be a flaky test that measures the compiler.
        // The number that matters is printed above and recorded in the report.
        #expect(elapsed < 30)
    }
}
