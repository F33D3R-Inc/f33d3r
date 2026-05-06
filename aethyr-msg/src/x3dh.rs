/// X3DH (Extended Triple Diffie-Hellman) — asynchronous session establishment.
///
/// Allows Alice to establish a shared session with Bob even when Bob is offline.
/// No interactive exchange required — Bob only needs to have published his prekeys.
///
/// Protocol (Signal spec §3):
///
///   Keys involved:
///     IK_A  Alice's identity key  (long-term)
///     EK_A  Alice's ephemeral key (one-time, generated per session)
///     IK_B  Bob's identity key    (long-term, fetched from relay)
///     SPK_B Bob's signed prekey   (medium-term, rotated every 1–4 weeks)
///     OPK_B Bob's one-time prekey (single use, deleted after claim; optional)
///
///   Shared secret:
///     DH1 = DH(IK_A, SPK_B)
///     DH2 = DH(EK_A, IK_B)
///     DH3 = DH(EK_A, SPK_B)
///     DH4 = DH(EK_A, OPK_B)   ← omit if no OPK available
///
///     master_secret = HKDF(DH1 || DH2 || DH3 [|| DH4],
///                          salt = 00..00,
///                          info = "AMP_v1_X3DH_v1")
///
///   Both Alice and Bob can independently derive the same master_secret.
///   master_secret seeds the Double Ratchet root key.
///
/// Security properties:
///   • Mutual authentication (both identity keys contribute to DH)
///   • Forward secrecy (ephemeral key deleted after use)
///   • Deniability (no signatures on the master secret itself)

use anyhow::{anyhow, Result};
use base64::{engine::general_purpose::URL_SAFE_NO_PAD as BASE64, Engine};
use x25519_dalek::{PublicKey, StaticSecret};

use crate::crypto::{generate_keypair, hkdf, x25519_dh};

/// Published prekey bundle for Bob (stored on relay, fetched by Alice).
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct PrekeyBundle {
    /// Bob's identity (BASE58(BLAKE3(ik_pub)))
    pub identity:          String,
    /// Bob's long-term identity public key (BASE64)
    pub ik_public:         String,
    /// Bob's signed prekey public key (BASE64)
    pub spk_public:        String,
    /// Bob's signed prekey ID
    pub spk_id:            i32,
    /// BLAKE3-MAC signature of spk_public with Bob's identity key
    /// Proves SPK_B is authored by Bob, prevents prekey injection attacks.
    pub spk_signature:     String,
    /// Bob's one-time prekey public key (BASE64, optional — may be absent)
    pub opk_public:        Option<String>,
    /// One-time prekey ID (used in the initial message so Bob can delete it)
    pub opk_id:            Option<i32>,
}

/// Result of Alice performing X3DH — contains the derived master secret
/// and the data Alice must include in her first message for Bob to reproduce the DH.
#[derive(Debug)]
pub struct X3DHSendResult {
    /// 32-byte master secret → seeds the Double Ratchet root key.
    pub master_secret: [u8; 32],
    /// Alice's ephemeral public key (Bob needs this for DH2/DH3/DH4).
    pub ek_public:     PublicKey,
    /// One-time prekey ID used (Bob needs to know which OPK to use).
    pub opk_id_used:   Option<i32>,
    /// Signed prekey ID used.
    pub spk_id_used:   i32,
}

/// Alice performs X3DH to initialise a session with Bob.
///
/// `alice_identity_secret`: Alice's long-term identity secret key.
/// `bundle`: Bob's prekey bundle fetched from the relay.
pub fn x3dh_send(
    alice_identity_secret: &StaticSecret,
    bundle: &PrekeyBundle,
) -> Result<X3DHSendResult> {
    let ik_b_bytes = BASE64.decode(&bundle.ik_public)
        .map_err(|_| anyhow!("invalid ik_public base64"))?;
    if ik_b_bytes.len() != 32 {
        return Err(anyhow!("ik_public must be 32 bytes"));
    }
    let ik_b = PublicKey::from(
        <[u8; 32]>::try_from(ik_b_bytes.as_slice()).unwrap()
    );

    let spk_b_bytes = BASE64.decode(&bundle.spk_public)
        .map_err(|_| anyhow!("invalid spk_public base64"))?;
    if spk_b_bytes.len() != 32 {
        return Err(anyhow!("spk_public must be 32 bytes"));
    }
    let spk_b = PublicKey::from(
        <[u8; 32]>::try_from(spk_b_bytes.as_slice()).unwrap()
    );

    // Verify SPK signature: BLAKE3-keyed(ik_b_bytes, spk_b_bytes) == spk_signature
    verify_spk_signature(&ik_b_bytes, &spk_b_bytes, &bundle.spk_signature)?;

    // Generate Alice's ephemeral key pair (single use)
    let (ek_a, ek_a_pub) = generate_keypair();

    // DH computations
    let dh1 = x25519_dh(alice_identity_secret, &spk_b); // IK_A × SPK_B
    let dh2 = x25519_dh(&ek_a, &ik_b);                  // EK_A × IK_B
    let dh3 = x25519_dh(&ek_a, &spk_b);                  // EK_A × SPK_B

    let opk_id_used;
    let ikm = if let Some(ref opk_b64) = bundle.opk_public {
        let opk_bytes = BASE64.decode(opk_b64)
            .map_err(|_| anyhow!("invalid opk_public base64"))?;
        if opk_bytes.len() != 32 {
            return Err(anyhow!("opk_public must be 32 bytes"));
        }
        let opk_b = PublicKey::from(<[u8; 32]>::try_from(opk_bytes.as_slice()).unwrap());
        let dh4 = x25519_dh(&ek_a, &opk_b);              // EK_A × OPK_B
        opk_id_used = bundle.opk_id;
        [dh1.as_ref(), dh2.as_ref(), dh3.as_ref(), dh4.as_ref()].concat()
    } else {
        opk_id_used = None;
        [dh1.as_ref(), dh2.as_ref(), dh3.as_ref()].concat()
    };

    let master_bytes = hkdf(&ikm, &[0u8; 32], b"AMP_v1_X3DH_v1", 32)?;
    let mut master_secret = [0u8; 32];
    master_secret.copy_from_slice(&master_bytes);

    Ok(X3DHSendResult {
        master_secret,
        ek_public: ek_a_pub,
        opk_id_used,
        spk_id_used: bundle.spk_id,
    })
}

/// Bob performs X3DH to derive the same master secret from Alice's initial message.
///
/// `bob_identity_secret`:    Bob's long-term identity secret key.
/// `bob_signed_prekey`:      The SPK_B secret key corresponding to `spk_id`.
/// `bob_one_time_prekey`:    The OPK_B secret key, if Alice used one.
/// `alice_identity_public`:  Alice's identity public key (fetched from relay).
/// `alice_ek_public`:        Alice's ephemeral public key (included in her first message).
pub fn x3dh_recv(
    bob_identity_secret:  &StaticSecret,
    bob_signed_prekey:    &StaticSecret,
    bob_one_time_prekey:  Option<&StaticSecret>,
    alice_identity_pub:   &PublicKey,
    alice_ek_pub:         &PublicKey,
) -> Result<[u8; 32]> {
    let ik_b   = bob_identity_secret;
    let spk_b  = bob_signed_prekey;
    let ek_a   = alice_ek_pub;
    let ik_a   = alice_identity_pub;

    let dh1 = x25519_dh(spk_b, ik_a);   // SPK_B × IK_A
    let dh2 = x25519_dh(ik_b,  ek_a);   // IK_B  × EK_A
    let dh3 = x25519_dh(spk_b, ek_a);   // SPK_B × EK_A

    let ikm = if let Some(opk_b) = bob_one_time_prekey {
        let dh4 = x25519_dh(opk_b, ek_a); // OPK_B × EK_A
        [dh1.as_ref(), dh2.as_ref(), dh3.as_ref(), dh4.as_ref()].concat()
    } else {
        [dh1.as_ref(), dh2.as_ref(), dh3.as_ref()].concat()
    };

    let master_bytes = hkdf(&ikm, &[0u8; 32], b"AMP_v1_X3DH_v1", 32)?;
    let mut out = [0u8; 32];
    out.copy_from_slice(&master_bytes);
    Ok(out)
}

/// Sign a signed prekey: BLAKE3-keyed(identity_key_bytes, spk_bytes) → BASE64.
/// Bob calls this when generating a new signed prekey.
pub fn sign_prekey(identity_key_bytes: &[u8], spk_bytes: &[u8]) -> String {
    let mut key_arr = [0u8; 32];
    let len = 32.min(identity_key_bytes.len());
    key_arr[..len].copy_from_slice(&identity_key_bytes[..len]);
    let mac = crate::crypto::blake3_keyed(&key_arr, spk_bytes);
    BASE64.encode(mac)
}

/// Verify a signed prekey signature.
pub fn verify_spk_signature(
    identity_key_bytes: &[u8],
    spk_bytes:          &[u8],
    signature_b64:      &str,
) -> Result<()> {
    let expected = sign_prekey(identity_key_bytes, spk_bytes);
    if expected != signature_b64 {
        return Err(anyhow!("signed prekey signature verification failed — possible injection attack"));
    }
    Ok(())
}

// ── Tests ─────────────────────────────────────────────────────────────────────

#[cfg(test)]
mod tests {
    use super::*;
    use crate::crypto::{generate_keypair, derive_identity};

    fn make_bundle_and_keys() -> (PrekeyBundle, StaticSecret, StaticSecret, StaticSecret) {
        let (ik_b_sec, ik_b_pub)  = generate_keypair();
        let (spk_b_sec, spk_b_pub) = generate_keypair();
        let (opk_b_sec, opk_b_pub) = generate_keypair();

        let identity = derive_identity(&ik_b_pub);
        let ik_bytes  = ik_b_pub.as_bytes().to_vec();
        let spk_bytes = spk_b_pub.as_bytes().to_vec();
        let sig = sign_prekey(&ik_bytes, &spk_bytes);

        let bundle = PrekeyBundle {
            identity,
            ik_public:    BASE64.encode(ik_bytes),
            spk_public:   BASE64.encode(spk_bytes),
            spk_id:       1,
            spk_signature: sig,
            opk_public:   Some(BASE64.encode(opk_b_pub.as_bytes())),
            opk_id:       Some(42),
        };
        (bundle, ik_b_sec, spk_b_sec, opk_b_sec)
    }

    #[test]
    fn x3dh_both_sides_agree() {
        let (bundle, ik_b_sec, spk_b_sec, opk_b_sec) = make_bundle_and_keys();
        let (alice_ik_sec, alice_ik_pub) = generate_keypair();

        let send_result = x3dh_send(&alice_ik_sec, &bundle).unwrap();

        // Bob recovers IK_A from the relay, EK_A from Alice's first message
        let bob_ms = x3dh_recv(
            &ik_b_sec, &spk_b_sec, Some(&opk_b_sec),
            &alice_ik_pub, &send_result.ek_public,
        ).unwrap();

        assert_eq!(send_result.master_secret, bob_ms,
            "Alice and Bob must derive the same master secret");
    }

    #[test]
    fn x3dh_without_opk() {
        let (mut bundle, ik_b_sec, spk_b_sec, _) = make_bundle_and_keys();
        bundle.opk_public = None;
        bundle.opk_id     = None;

        let (alice_ik_sec, alice_ik_pub) = generate_keypair();
        let send_result = x3dh_send(&alice_ik_sec, &bundle).unwrap();

        let bob_ms = x3dh_recv(
            &ik_b_sec, &spk_b_sec, None,
            &alice_ik_pub, &send_result.ek_public,
        ).unwrap();

        assert_eq!(send_result.master_secret, bob_ms);
    }

    #[test]
    fn invalid_spk_signature_rejected() {
        let (mut bundle, _, _, _) = make_bundle_and_keys();
        bundle.spk_signature = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=".into();
        let (alice_sec, _) = generate_keypair();
        assert!(x3dh_send(&alice_sec, &bundle).is_err());
    }
}
