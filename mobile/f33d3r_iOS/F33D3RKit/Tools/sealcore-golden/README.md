# sealcore golden vectors

Generates and checks `Tests/F33D3RKitTests/Fixtures/sealcore_golden.json`, the
fixture `SealCoreTests` and `Argon2Tests` are proven against.

It exists because `SealCore.swift` cannot be proven against itself. Sealed mode
is end-to-end encrypted: the server stores blobs it cannot read and cannot
repair, so a Swift port that disagrees with the browser produces messages one
side can read and the other cannot, with no error anywhere naming the byte that
differed. And every likely mistake is self-consistent — put the GCM tag in the
wrong place, salt the HKDF wrong, wrap the base64 *text* of the private key
instead of its 32 bytes, and seal-then-open still passes in Swift while nothing
the web client wrote will open.

So the vectors are produced by a second implementation, written here in Go from
`sealcore/src/lib.rs`, that had never seen the Swift.

## What is proven, and what is not

| | |
|---|---|
| Argon2id at the parameters this app uses | `golang.org/x/crypto/argon2` — an implementation independent of both the Rust crate and the Swift port |
| Argon2 with a secret and associated data | RFC 9106 §5, and `tests/kat.rs` of argon2 **0.5.3** — the exact version `sealcore/Cargo.lock` pins. Transcribed as literals in `Argon2Tests`; `x/crypto` has no secret or AD parameter and cannot compute them |
| HKDF-SHA256 with an absent salt | Go's `crypto/hkdf`, and a Swift-side assertion that CryptoKit's no-salt overload equals an explicit 32 zero bytes |
| X25519, the whole seal, the vault | Go seals, Swift opens (fixture `sealed`, `vault`); Swift seals, Go opens (fixture `from_swift`) |

**Not proven: agreement with the Rust crate itself.** Cargo is not installed on
this machine and installing it was out of scope, so `cargo test` in
`sealcore/` has not been run against these vectors. What was done instead:
the crate source for argon2 0.5.3 was read directly — `Params::DEFAULT` is
`m_cost: 19 * 1024`, `t_cost: 2`, `p_cost: 1`; `Algorithm::default()` carries
`#[default]` on `Argon2id`; `Version::default()` is `V0x13`;
`hash_password_into` takes the tag length from the caller's buffer, which
`derive_backup_key_impl` gives as `[0u8; 32]` — and the crate's own KATs are
pinned in `Argon2Tests`. Closing the gap properly means one command on a machine
with Rust:

```bash
cd sealcore && cargo test
# then add a test that prints an envelope for a fixed keypair and paste it in
# as another `sealed` case, marked source=rust.
```

## Regenerating

Needs Go, the Xcode beta toolchain, and `golang.org/x/crypto` in the module
cache.

```bash
cd F33D3RKit/Tools/sealcore-golden
FIXTURES=../../Tests/F33D3RKitTests/Fixtures

# 1. Go seals; every envelope is opened here before it is written out.
go run . generate "$FIXTURES/sealcore_golden.json"

# 2. Swift seals, Go opens, and the verdict is folded back in.
cd ../..
SEALCORE_EXPORT_DIR=$PWD/Tools/sealcore-golden swift test --filter exportEnvelopesForGo
cd Tools/sealcore-golden
go run . verify envelopes_from_swift.json verdicts_from_go.json   # must report 6/6 + vault
go run . merge "$FIXTURES/sealcore_golden.json" verdicts_from_go.json
rm envelopes_from_swift.json verdicts_from_go.json

cd ../.. && swift test
```

A regeneration that makes a previously passing Swift test fail is not a fixture
to be updated — it is the two implementations having drifted apart, and one of
them is now unable to read the other's messages.

This directory is outside `Sources/` and `Tests/`, so SwiftPM does not build it
and `swift test` does not need Go.
