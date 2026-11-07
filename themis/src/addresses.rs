//! Checks that a payout address is really an address, checksum included. A typo here
//! would send a seller's money nowhere, and a malformed one must never reach the code
//! that reads the chains.

use sha2::{Digest, Sha256};

const BITCOIN_ALPHABET: &str = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";
const XRPL_ALPHABET: &str = "rpshnaf39wBUDNEGHJKLM4PQRST7VWXYZ2bcdeCg65jkm8oFqi1tuvAxyz";
const BECH32_CHARSET: &str = "qpzry9x8gf2tvdw0s3jn54khce6mua7l";
const BECH32_CONST: u32 = 1;
const BECH32M_CONST: u32 = 0x2bc8_30a3;

/// Base58 digits to bytes, keeping leading zero bytes.
fn base58_decode(s: &str, alphabet: &str) -> Option<Vec<u8>> {
    let mut bytes: Vec<u8> = Vec::new();
    for c in s.chars() {
        let mut carry = alphabet.find(c)? as u32;
        for b in bytes.iter_mut().rev() {
            carry += u32::from(*b) * 58;
            *b = (carry & 0xff) as u8;
            carry >>= 8;
        }
        while carry > 0 {
            bytes.insert(0, (carry & 0xff) as u8);
            carry >>= 8;
        }
    }
    let zero = alphabet.chars().next()?;
    let leading = s.chars().take_while(|c| *c == zero).count();
    let mut out = vec![0u8; leading];
    out.extend(bytes);
    Some(out)
}

/// A 21-byte payload with its 4-byte double-SHA-256 checksum, and its version byte.
fn checked_version(s: &str, alphabet: &str) -> Option<u8> {
    let decoded = base58_decode(s, alphabet)?;
    if decoded.len() != 25 {
        return None;
    }
    let (payload, check) = decoded.split_at(21);
    let digest = Sha256::digest(Sha256::digest(payload));
    (digest[..4] == *check).then_some(payload[0])
}

fn bech32_polymod(values: impl IntoIterator<Item = u8>) -> u32 {
    const GEN: [u32; 5] = [0x3b6a_57b2, 0x2650_8e6d, 0x1ea1_19fa, 0x3d42_33dd, 0x2a14_62b3];
    let mut chk: u32 = 1;
    for v in values {
        let top = chk >> 25;
        chk = ((chk & 0x01ff_ffff) << 5) ^ u32::from(v);
        for (i, g) in GEN.iter().enumerate() {
            if (top >> i) & 1 == 1 {
                chk ^= g;
            }
        }
    }
    chk
}

/// A mainnet SegWit address: v0 with bech32, v1 Taproot with bech32m.
fn valid_segwit(addr: &str) -> bool {
    let Some(data_part) = addr.strip_prefix("bc1") else {
        return false;
    };
    let Some(data) = data_part
        .chars()
        .map(|c| BECH32_CHARSET.find(c).map(|i| i as u8))
        .collect::<Option<Vec<u8>>>()
    else {
        return false;
    };
    if data.len() < 7 {
        return false;
    }
    let hrp = [3u8, 3, 0, 2, 3];
    let check = bech32_polymod(hrp.into_iter().chain(data.iter().copied()));
    match (data[0], addr.len()) {
        (0, 42 | 62) => check == BECH32_CONST,
        (1, 62) => check == BECH32M_CONST,
        _ => false,
    }
}

pub fn valid_btc(addr: &str) -> bool {
    if addr.starts_with("bc1") {
        return valid_segwit(addr);
    }
    // 0x00 is pay-to-key and 0x05 pay-to-script, on mainnet.
    matches!(checked_version(addr, BITCOIN_ALPHABET), Some(0x00 | 0x05))
}

pub fn valid_xrp(addr: &str) -> bool {
    addr.starts_with('r') && checked_version(addr, XRPL_ALPHABET) == Some(0x00)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn real_bitcoin_addresses_pass() {
        assert!(valid_btc("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"));
        assert!(valid_btc("3J98t1WpEZ73CNmQviecrnyiWrnqRhWNLy"));
        assert!(valid_btc("bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"));
        assert!(valid_btc("bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqzk5jj0"));
    }

    #[test]
    fn a_typo_fails_the_checksum() {
        assert!(!valid_btc("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNb"));
        assert!(!valid_btc("bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t5"));
        assert!(!valid_btc("bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqzk5jj1"));
        assert!(!valid_btc("not an address"));
    }

    #[test]
    fn xrp_addresses_use_their_own_alphabet() {
        assert!(valid_xrp("rrrrrrrrrrrrrrrrrrrrrhoLvTp"));
        assert!(valid_xrp("rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh"));
        assert!(!valid_xrp("rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTi"));
        assert!(!valid_xrp("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"));
    }
}
