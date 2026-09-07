// Cross-verification for Malkuth signatures.
//
// Steps 2-6 below are COPIED VERBATIM from `verifyMalkuthSig` in
// feed-engine/internal/handler/work_event.go. Step 1 (fetching the key from
// Elohim Veni) is the only thing replaced, by the key the vector carries —
// which is exactly the key the registration would have handed the authority.
//
// Two jobs:
//
//	verify   read vectors Swift produced and report whether Go accepts each one
//	generate produce vectors signed by Go, for Swift to verify
//
// Run:
//	go run ./verify verify   vectors_from_swift.json
//	go run ./verify generate vectors_from_go.json
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"runtime"
)

// verifyMalkuthSigBody is verifyMalkuthSig with its step 1 — the Elohim Veni
// fetch — replaced by the caller supplying the key. Everything from the SPKI
// decode onwards is unchanged.
func verifyMalkuthSigBody(pubKeyB64, cid, sigBase64URL string) bool {
	if pubKeyB64 == "" {
		return false
	}

	// 2. Decode SPKI bytes
	spkiBytes, err := base64.StdEncoding.DecodeString(pubKeyB64)
	if err != nil {
		spkiBytes, err = base64.RawStdEncoding.DecodeString(pubKeyB64)
		if err != nil {
			return false
		}
	}

	// 3. Parse ECDSA public key
	pub, err := x509.ParsePKIXPublicKey(spkiBytes)
	if err != nil {
		return false
	}
	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return false
	}

	// 4. Decode signature (base64url, no padding)
	sigBytes, err := base64.RawURLEncoding.DecodeString(sigBase64URL)
	if err != nil {
		return false
	}
	if len(sigBytes) != 64 {
		return false
	}

	// 5. Compute SHA-256 of CID string
	digest := sha256.Sum256([]byte(cid))

	// 6. Split P1363 sig into r and s (each 32 bytes)
	r := new(big.Int).SetBytes(sigBytes[:32])
	s := new(big.Int).SetBytes(sigBytes[32:])

	return ecdsa.Verify(ecPub, digest[:], r, s)
}

type swiftVector struct {
	Name            string `json:"name"`
	PrivateKeyRawB64 string `json:"private_key_raw_b64"`
	PublicKeySPKIB64 string `json:"public_key_spki_b64"`
	CID             string `json:"cid"`
	SignatureB64URL string `json:"signature_b64url"`
	ShouldVerify    bool   `json:"should_verify"`
	VerifiedByGo    bool   `json:"verified_by_go"`
}

type goVector struct {
	Name             string `json:"name"`
	PublicKeySPKIB64 string `json:"public_key_spki_b64"`
	CID              string `json:"cid"`
	SignatureB64URL  string `json:"signature_b64url"`
	ShouldVerify     bool   `json:"should_verify"`
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: verify <verify|generate> <file>")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "verify":
		doVerify(os.Args[2])
	case "generate":
		doGenerate(os.Args[2])
	default:
		fmt.Fprintln(os.Stderr, "unknown mode")
		os.Exit(2)
	}
}

func doVerify(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	var file struct {
		Vectors []swiftVector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		panic(err)
	}
	if len(file.Vectors) == 0 {
		panic("no vectors")
	}

	failures := 0
	for i := range file.Vectors {
		v := &file.Vectors[i]
		v.VerifiedByGo = verifyMalkuthSigBody(v.PublicKeySPKIB64, v.CID, v.SignatureB64URL)
		status := "ok"
		if v.VerifiedByGo != v.ShouldVerify {
			status = "MISMATCH"
			failures++
		}
		fmt.Printf("%-28s expected=%-5v go=%-5v %s\n", v.Name, v.ShouldVerify, v.VerifiedByGo, status)
	}

	out, _ := json.MarshalIndent(map[string]any{
		"go_version": runtime.Version(),
		"vectors":    file.Vectors,
	}, "", "  ")
	_ = os.WriteFile(path+".verified", out, 0o644)
	fmt.Printf("\n%d/%d as expected; wrote %s.verified\n", len(file.Vectors)-failures, len(file.Vectors), path)
	if failures > 0 {
		os.Exit(1)
	}
}

func doGenerate(path string) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		panic(err)
	}
	spkiB64 := base64.StdEncoding.EncodeToString(spki)

	// Sign a CID the way the browser does: ECDSA over SHA-256 of the CID's
	// bytes, emitted as P1363 r||s, base64url unpadded.
	sign := func(cid string) string {
		digest := sha256.Sum256([]byte(cid))
		r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
		if err != nil {
			panic(err)
		}
		sig := make([]byte, 64)
		r.FillBytes(sig[:32])
		s.FillBytes(sig[32:])
		return base64.RawURLEncoding.EncodeToString(sig)
	}

	cids := []string{
		"sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	}
	// Plus real CIDs out of the canonical fixtures, so the signed strings are
	// strings a real post produces.
	if canonical, err := os.ReadFile("golden_canonical.json"); err == nil {
		var g struct {
			Cases []struct {
				CID string `json:"cid"`
			} `json:"cases"`
		}
		if json.Unmarshal(canonical, &g) == nil {
			for i, c := range g.Cases {
				if i >= 4 {
					break
				}
				cids = append(cids, c.CID)
			}
		}
	}

	vectors := []goVector{}
	for i, cid := range cids {
		vectors = append(vectors, goVector{
			Name:             fmt.Sprintf("go_signed_%d", i),
			PublicKeySPKIB64: spkiB64,
			CID:              cid,
			SignatureB64URL:  sign(cid),
			ShouldVerify:     true,
		})
	}

	// Negative: a good signature against a CID it does not cover.
	vectors = append(vectors, goVector{
		Name:             "go_signed_wrong_cid",
		PublicKeySPKIB64: spkiB64,
		CID:              "sha256:" + fmt.Sprintf("%064x", 1),
		SignatureB64URL:  sign(cids[0]),
		ShouldVerify:     false,
	})

	// Negative: a good signature against a different key.
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	otherSPKI, _ := x509.MarshalPKIXPublicKey(&other.PublicKey)
	vectors = append(vectors, goVector{
		Name:             "go_signed_wrong_key",
		PublicKeySPKIB64: base64.StdEncoding.EncodeToString(otherSPKI),
		CID:              cids[0],
		SignatureB64URL:  sign(cids[0]),
		ShouldVerify:     false,
	})

	// Negative: one bit flipped in s.
	tampered := sign(cids[0])
	sigBytes, _ := base64.RawURLEncoding.DecodeString(tampered)
	sigBytes[63] ^= 0x01
	vectors = append(vectors, goVector{
		Name:             "go_signed_bit_flipped",
		PublicKeySPKIB64: spkiB64,
		CID:              cids[0],
		SignatureB64URL:  base64.RawURLEncoding.EncodeToString(sigBytes),
		ShouldVerify:     false,
	})

	// Self-check: Go must agree with its own expectations before Swift is asked.
	for _, v := range vectors {
		if got := verifyMalkuthSigBody(v.PublicKeySPKIB64, v.CID, v.SignatureB64URL); got != v.ShouldVerify {
			panic(fmt.Sprintf("%s: go verifier disagrees with the vector it just built", v.Name))
		}
	}

	out, _ := json.MarshalIndent(map[string]any{
		"go_version": runtime.Version(),
		"vectors":    vectors,
	}, "", "  ")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("wrote %d go-signed vectors to %s\n", len(vectors), path)
}
