// sealcore-golden generates — and checks — the fixture in
// Tests/F33D3RKitTests/Fixtures/sealcore_golden.json.
//
// It exists because `SealCore.swift` cannot be proven against itself. Seal then
// open in one implementation passes with the tag in the wrong place, the HKDF
// salt wrong, and the vault wrapping base64 text instead of key bytes — all
// three self-consistent, all three unable to read a single message the browser
// wrote.
//
// Go is not the Rust the browser runs, so this is not a proof of agreement with
// sealcore itself; it is a second, independent implementation of the same
// documented scheme, written from `sealcore/src/lib.rs`. Where Go and Swift
// agree, both agree with the spec on that point. What remains outstanding is a
// run against the Rust crate; see the README.
//
//	go run . generate golden.json          # Go seals, Swift must open
//	go run . verify envelopes_from_swift.json verdicts.json   # Swift sealed, Go opens
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/crypto/argon2"
)

// Copied from sealcore/src/lib.rs:32. Not derived, not reconstructed.
const hkdfInfo = "sealcore-v1-x25519-wrap"

var b64 = base64.StdEncoding

// ── the scheme ──────────────────────────────────────────────────────────────

// aesSeal returns the Rust `aes-gcm` layout: ciphertext with the 16-byte tag
// appended, nonce separate. Go's gcm.Seal appends the tag the same way, which
// is exactly the convention Swift has to reproduce by hand.
func aesSeal(key, plaintext []byte) (ct, nonce []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return gcm.Seal(nil, nonce, plaintext, nil), nonce, nil
}

func aesOpen(key, nonce, ct []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ct, nil)
}

// wrapKey is ECDH then HKDF-SHA256 with an absent salt, which RFC 5869 defines
// as HashLen zero bytes — spelled out here rather than left to a library's
// idea of "no salt".
func wrapKey(priv *ecdh.PrivateKey, peer *ecdh.PublicKey) ([]byte, error) {
	shared, err := priv.ECDH(peer)
	if err != nil {
		return nil, err
	}
	return hkdf.Key(sha256.New, shared, make([]byte, 32), hkdfInfo, 32)
}

type sealedKey struct {
	RecipientAccount string `json:"recipient_account"`
	EphPubB64        string `json:"eph_pub_b64"`
	SealedB64        string `json:"sealed_b64"`
	SealedNonceB64   string `json:"sealed_nonce_b64"`
}

type envelope struct {
	BodyCtB64    string      `json:"body_ct_b64"`
	BodyNonceB64 string      `json:"body_nonce_b64"`
	Sealed       []sealedKey `json:"sealed"`
}

type recipient struct {
	Account string `json:"account"`
	PrivB64 string `json:"priv_b64"`
	PubB64  string `json:"pub_b64"`
}

func sealMessage(plaintext string, recipients []recipient) (envelope, error) {
	contentKey := make([]byte, 32)
	if _, err := rand.Read(contentKey); err != nil {
		return envelope{}, err
	}
	bodyCt, bodyNonce, err := aesSeal(contentKey, []byte(plaintext))
	if err != nil {
		return envelope{}, err
	}

	env := envelope{
		BodyCtB64:    b64.EncodeToString(bodyCt),
		BodyNonceB64: b64.EncodeToString(bodyNonce),
	}
	for _, r := range recipients {
		raw, err := b64.DecodeString(r.PubB64)
		if err != nil {
			return envelope{}, err
		}
		theirPub, err := ecdh.X25519().NewPublicKey(raw)
		if err != nil {
			return envelope{}, err
		}
		eph, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return envelope{}, err
		}
		wk, err := wrapKey(eph, theirPub)
		if err != nil {
			return envelope{}, err
		}
		sct, snonce, err := aesSeal(wk, contentKey)
		if err != nil {
			return envelope{}, err
		}
		env.Sealed = append(env.Sealed, sealedKey{
			RecipientAccount: r.Account,
			EphPubB64:        b64.EncodeToString(eph.PublicKey().Bytes()),
			SealedB64:        b64.EncodeToString(sct),
			SealedNonceB64:   b64.EncodeToString(snonce),
		})
	}
	return env, nil
}

func openMessage(privB64 string, s sealedKey, env envelope) (string, error) {
	rawPriv, err := b64.DecodeString(privB64)
	if err != nil {
		return "", err
	}
	priv, err := ecdh.X25519().NewPrivateKey(rawPriv)
	if err != nil {
		return "", err
	}
	rawEph, err := b64.DecodeString(s.EphPubB64)
	if err != nil {
		return "", err
	}
	eph, err := ecdh.X25519().NewPublicKey(rawEph)
	if err != nil {
		return "", err
	}
	wk, err := wrapKey(priv, eph)
	if err != nil {
		return "", err
	}

	sNonce, err := b64.DecodeString(s.SealedNonceB64)
	if err != nil {
		return "", err
	}
	sCt, err := b64.DecodeString(s.SealedB64)
	if err != nil {
		return "", err
	}
	contentKey, err := aesOpen(wk, sNonce, sCt)
	if err != nil {
		return "", fmt.Errorf("unwrap content key: %w", err)
	}
	if len(contentKey) != 32 {
		return "", fmt.Errorf("content key is %d bytes", len(contentKey))
	}

	bodyNonce, err := b64.DecodeString(env.BodyNonceB64)
	if err != nil {
		return "", err
	}
	bodyCt, err := b64.DecodeString(env.BodyCtB64)
	if err != nil {
		return "", err
	}
	body, err := aesOpen(contentKey, bodyNonce, bodyCt)
	if err != nil {
		return "", fmt.Errorf("open body: %w", err)
	}
	return string(body), nil
}

// saltFromHandle mirrors gnosis-seal.js: base64 of SHA-256 of the lower-cased
// handle. derive_backup_key then base64-DECODES it, so what Argon2 sees is the
// 32 raw digest bytes.
func saltFromHandle(handle string) []byte {
	sum := sha256.Sum256([]byte(lower(handle)))
	return sum[:]
}

func lower(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'A' && c <= 'Z' {
			out[i] = c + 32
		}
	}
	return string(out)
}

// ── fixture ─────────────────────────────────────────────────────────────────

type argon2Vector struct {
	Name        string `json:"name"`
	Source      string `json:"source"`
	Variant     string `json:"variant"`
	Version     int    `json:"version"`
	TimeCost    int    `json:"t"`
	MemoryKiB   int    `json:"m_kib"`
	Lanes       int    `json:"p"`
	PasswordB64 string `json:"password_b64"`
	SaltB64     string `json:"salt_b64"`
	SecretB64   string `json:"secret_b64"`
	ADB64       string `json:"ad_b64"`
	TagHex      string `json:"tag_hex"`
}

type hkdfVector struct {
	Name    string `json:"name"`
	IKMB64  string `json:"ikm_b64"`
	SaltB64 string `json:"salt_b64"`
	Info    string `json:"info"`
	OKMB64  string `json:"okm_b64"`
}

type agreementVector struct {
	Name       string `json:"name"`
	PrivB64    string `json:"priv_b64"`
	PubB64     string `json:"pub_b64"`
	PeerPubB64 string `json:"peer_pub_b64"`
	SharedB64  string `json:"shared_b64"`
	WrapKeyB64 string `json:"wrap_key_b64"`
}

type sealedCase struct {
	Name       string      `json:"name"`
	Plaintext  string      `json:"plaintext"`
	Recipients []recipient `json:"recipients"`
	Envelope   envelope    `json:"envelope"`
}

type vaultCase struct {
	Note            string `json:"note"`
	Handle          string `json:"handle"`
	Password        string `json:"password"`
	SaltB64         string `json:"salt_b64"`
	DerivedKeyB64   string `json:"derived_key_b64"`
	IdentityPrivB64 string `json:"identity_priv_b64"`
	IdentityPubB64  string `json:"identity_pub_b64"`
	WrappedCtB64    string `json:"wrapped_ct_b64"`
	WrappedNonceB64 string `json:"wrapped_nonce_b64"`
}

type fixture struct {
	Note        string            `json:"note"`
	GeneratedBy string            `json:"generated_by"`
	Argon2      []argon2Vector    `json:"argon2"`
	HKDF        []hkdfVector      `json:"hkdf"`
	Agreement   []agreementVector `json:"agreement"`
	Sealed      []sealedCase      `json:"sealed"`
	Vault       vaultCase         `json:"vault"`
	FromSwift   json.RawMessage   `json:"from_swift,omitempty"`
}

func generate(path string) error {
	f := fixture{
		Note: "Vectors for SealCore.swift. The Argon2 rows marked source=go are computed here " +
			"by golang.org/x/crypto/argon2, an implementation independent of both the Rust crate " +
			"and the Swift port. The rows marked source=argon2-0.5.3-kat and source=rfc9106 are " +
			"transcribed from tests/kat.rs of the exact crate version sealcore/Cargo.lock pins; " +
			"they carry a secret and associated data, which x/crypto cannot express.",
		GeneratedBy: fmt.Sprintf("Tools/sealcore-golden (%s)", runtime.Version()),
	}

	// Argon2, computed here. The first row is the one that matters: it is
	// Argon2::default() in argon2 0.5.3 — Argon2id, v0x13, m=19456, t=2, p=1,
	// 32-byte tag — over a handle-derived salt, which is the whole vault path.
	goVectors := []struct {
		name           string
		t, m, p        uint32
		password, salt []byte
	}{
		{"sealcore_default_params", 2, 19456, 1, []byte("correct horse battery staple"), saltFromHandle("MiiYazuko")},
		{"sealcore_empty_password", 2, 19456, 1, []byte(""), saltFromHandle("dev")},
		{"sealcore_unicode_password", 2, 19456, 1, []byte("пароль🔒パスワード"), saltFromHandle("tehanibentley")},
		{"cheap_t1_m8_p1", 1, 8, 1, []byte("password"), []byte("somesalt")},
		{"cheap_t3_m64_p4", 3, 64, 4, []byte("password"), []byte("somesalt")},
		{"reference_t2_m256_p2", 2, 256, 2, []byte("password"), []byte("somesalt")},
	}
	for _, v := range goVectors {
		tag := argon2.IDKey(v.password, v.salt, v.t, v.m, uint8(v.p), 32)
		f.Argon2 = append(f.Argon2, argon2Vector{
			Name: v.name, Source: "go", Variant: "id", Version: 0x13,
			TimeCost: int(v.t), MemoryKiB: int(v.m), Lanes: int(v.p),
			PasswordB64: b64.EncodeToString(v.password),
			SaltB64:     b64.EncodeToString(v.salt),
			SecretB64:   "", ADB64: "",
			TagHex: hex.EncodeToString(tag),
		})
	}

	// The vectors x/crypto cannot compute, because it has no secret or
	// associated-data parameter. Transcribed from the crate's own KAT file.
	kat := []argon2Vector{
		{Name: "rfc9106_argon2d_v13", Source: "rfc9106", Variant: "d", Version: 0x13, TimeCost: 3, MemoryKiB: 32, Lanes: 4,
			TagHex: "512b391b6f1162975371d30919734294f868e3be3984f3c1a13a4db9fabe4acb"},
		{Name: "rfc9106_argon2i_v13", Source: "rfc9106", Variant: "i", Version: 0x13, TimeCost: 3, MemoryKiB: 32, Lanes: 4,
			TagHex: "c814d9d1dc7f37aa13f0d77f2494bda1c8de6b016dd388d29952a4c4672b6ce8"},
		{Name: "rfc9106_argon2id_v13", Source: "rfc9106", Variant: "id", Version: 0x13, TimeCost: 3, MemoryKiB: 32, Lanes: 4,
			TagHex: "0d640df58d78766c08c037a34a8b53c9d01ef0452d75b65eb52520e96b01e659"},
		{Name: "kat_argon2id_v10", Source: "argon2-0.5.3-kat", Variant: "id", Version: 0x10, TimeCost: 3, MemoryKiB: 32, Lanes: 4,
			TagHex: "b64615f07789b66b645b67ee9ed3b377ae350b6bfcbb0fc95141ea8f322613c0"},
	}
	for i := range kat {
		kat[i].PasswordB64 = b64.EncodeToString(repeat(0x01, 32))
		kat[i].SaltB64 = b64.EncodeToString(repeat(0x02, 16))
		kat[i].SecretB64 = b64.EncodeToString(repeat(0x03, 8))
		kat[i].ADB64 = b64.EncodeToString(repeat(0x04, 12))
	}
	f.Argon2 = append(f.Argon2, kat...)

	// HKDF. The second row is the same derivation with an *empty* salt rather
	// than 32 zero bytes: HMAC pads a short key with zeros to its 64-byte block,
	// so the two must come out identical, and a Swift that picks the wrong
	// overload is still correct. The test asserts they match.
	ikm := []byte("shared secret bytes, exactly 32.")
	okmZero, err := hkdf.Key(sha256.New, ikm, make([]byte, 32), hkdfInfo, 32)
	if err != nil {
		return err
	}
	okmEmpty, err := hkdf.Key(sha256.New, ikm, nil, hkdfInfo, 32)
	if err != nil {
		return err
	}
	f.HKDF = []hkdfVector{
		{Name: "absent_salt_as_32_zero_bytes", IKMB64: b64.EncodeToString(ikm),
			SaltB64: b64.EncodeToString(make([]byte, 32)), Info: hkdfInfo,
			OKMB64: b64.EncodeToString(okmZero)},
		{Name: "empty_salt", IKMB64: b64.EncodeToString(ikm), SaltB64: "", Info: hkdfInfo,
			OKMB64: b64.EncodeToString(okmEmpty)},
	}

	// X25519 agreement plus the wrap key it feeds, so a Swift failure can be
	// localised to ECDH or to HKDF rather than "the message did not open".
	for i := 0; i < 2; i++ {
		a, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		b, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		shared, err := a.ECDH(b.PublicKey())
		if err != nil {
			return err
		}
		wk, err := wrapKey(a, b.PublicKey())
		if err != nil {
			return err
		}
		f.Agreement = append(f.Agreement, agreementVector{
			Name:       fmt.Sprintf("pair_%d", i),
			PrivB64:    b64.EncodeToString(a.Bytes()),
			PubB64:     b64.EncodeToString(a.PublicKey().Bytes()),
			PeerPubB64: b64.EncodeToString(b.PublicKey().Bytes()),
			SharedB64:  b64.EncodeToString(shared),
			WrapKeyB64: b64.EncodeToString(wk),
		})
	}

	// Whole envelopes, one per recipient count.
	for _, spec := range []struct {
		name      string
		plaintext string
		count     int
	}{
		{"single_recipient", "hello sealed", 1},
		{"group_of_three", "group msg — 三人 🔐", 3},
		{"empty_body", "", 1},
	} {
		var recips []recipient
		for i := 0; i < spec.count; i++ {
			k, err := ecdh.X25519().GenerateKey(rand.Reader)
			if err != nil {
				return err
			}
			recips = append(recips, recipient{
				Account: fmt.Sprintf("acct-%s-%d", spec.name, i),
				PrivB64: b64.EncodeToString(k.Bytes()),
				PubB64:  b64.EncodeToString(k.PublicKey().Bytes()),
			})
		}
		env, err := sealMessage(spec.plaintext, recips)
		if err != nil {
			return err
		}
		// Never emit a vector without opening it here first: a fixture that
		// only one side has ever read proves whatever bug produced it.
		for i, r := range recips {
			got, err := openMessage(r.PrivB64, env.Sealed[i], env)
			if err != nil || got != spec.plaintext {
				return fmt.Errorf("%s: self-open failed: %v (%q)", spec.name, err, got)
			}
		}
		f.Sealed = append(f.Sealed, sealedCase{
			Name: spec.name, Plaintext: spec.plaintext, Recipients: recips, Envelope: env,
		})
	}

	// The vault: password -> Argon2id -> wrap the raw identity key. This one
	// fixture pins Argon2, AES-GCM and both base64 conventions at once.
	handle := "MiiYazuko"
	password := "f33d3rdev"
	salt := saltFromHandle(handle)
	derived := argon2.IDKey([]byte(password), salt, 2, 19456, 1, 32)
	identity, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	// wrap_with_key base64-decodes its argument, so the sealed plaintext is the
	// 32 raw scalar bytes and not the 44 characters that spell them.
	wct, wnonce, err := aesSeal(derived, identity.Bytes())
	if err != nil {
		return err
	}
	f.Vault = vaultCase{
		Note:            "salt_b64 is SHA-256 of the lower-cased handle; Argon2 sees its 32 decoded bytes.",
		Handle:          handle,
		Password:        password,
		SaltB64:         b64.EncodeToString(salt),
		DerivedKeyB64:   b64.EncodeToString(derived),
		IdentityPrivB64: b64.EncodeToString(identity.Bytes()),
		IdentityPubB64:  b64.EncodeToString(identity.PublicKey().Bytes()),
		WrappedCtB64:    b64.EncodeToString(wct),
		WrappedNonceB64: b64.EncodeToString(wnonce),
	}

	out, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s: %d argon2, %d hkdf, %d agreement, %d sealed\n",
		path, len(f.Argon2), len(f.HKDF), len(f.Agreement), len(f.Sealed))
	return nil
}

func repeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

// ── the other direction: Swift sealed it, Go opens it ───────────────────────

type swiftExport struct {
	Cases []struct {
		Name          string    `json:"name"`
		Plaintext     string    `json:"plaintext"`
		ShouldOpen    bool      `json:"should_open"`
		RecipientPriv string    `json:"recipient_priv_b64"`
		Sealed        sealedKey `json:"sealed"`
		Envelope      envelope  `json:"envelope"`
	} `json:"cases"`
	Vault struct {
		Handle          string `json:"handle"`
		Password        string `json:"password"`
		DerivedKeyB64   string `json:"derived_key_b64"`
		IdentityPrivB64 string `json:"identity_priv_b64"`
		WrappedCtB64    string `json:"wrapped_ct_b64"`
		WrappedNonceB64 string `json:"wrapped_nonce_b64"`
	} `json:"vault"`
}

func verify(inPath, outPath string) error {
	raw, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}
	var export swiftExport
	if err := json.Unmarshal(raw, &export); err != nil {
		return err
	}

	type verdict struct {
		Name       string `json:"name"`
		ShouldOpen bool   `json:"should_open"`
		OpenedByGo bool   `json:"opened_by_go"`
		Plaintext  string `json:"plaintext_go_read"`
		Error      string `json:"error,omitempty"`
	}
	var verdicts []verdict
	pass := 0
	for _, c := range export.Cases {
		got, err := openMessage(c.RecipientPriv, c.Sealed, c.Envelope)
		v := verdict{Name: c.Name, ShouldOpen: c.ShouldOpen, OpenedByGo: err == nil && got == c.Plaintext, Plaintext: got}
		if err != nil {
			v.Error = err.Error()
		}
		if v.OpenedByGo == c.ShouldOpen {
			pass++
		}
		verdicts = append(verdicts, v)
	}

	// The vault, in the direction that matters: Swift derived the key from the
	// password, so Go re-derives it and unwraps what Swift wrapped.
	vaultOK := false
	vaultNote := ""
	if export.Vault.Password != "" {
		derived := argon2.IDKey([]byte(export.Vault.Password), saltFromHandle(export.Vault.Handle), 2, 19456, 1, 32)
		if b64.EncodeToString(derived) != export.Vault.DerivedKeyB64 {
			vaultNote = "argon2 key derived by Swift differs from the one derived here"
		} else {
			ct, _ := b64.DecodeString(export.Vault.WrappedCtB64)
			nonce, _ := b64.DecodeString(export.Vault.WrappedNonceB64)
			pt, err := aesOpen(derived, nonce, ct)
			if err != nil {
				vaultNote = "unwrap failed: " + err.Error()
			} else if b64.EncodeToString(pt) != export.Vault.IdentityPrivB64 {
				vaultNote = "unwrapped bytes are not the identity key Swift said it wrapped"
			} else {
				vaultOK = true
				vaultNote = "derived the same key and unwrapped the raw 32-byte identity scalar"
			}
		}
	}

	result := map[string]any{
		"go_version":     runtime.Version(),
		"verdicts":       verdicts,
		"vault_verified": vaultOK,
		"vault_note":     vaultNote,
	}
	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(outPath, append(out, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("%d/%d envelopes as expected; vault verified: %v %s\n",
		pass, len(export.Cases), vaultOK, vaultNote)
	if pass != len(export.Cases) || !vaultOK {
		return fmt.Errorf("cross-verification failed")
	}
	return nil
}

// merge folds a verify run's verdicts into the fixture's from_swift, so the
// checked-in artifact is produced by a command rather than by hand-editing JSON.
func merge(fixturePath, verdictsPath string) error {
	rawFixture, err := os.ReadFile(fixturePath)
	if err != nil {
		return err
	}
	var f fixture
	if err := json.Unmarshal(rawFixture, &f); err != nil {
		return err
	}
	verdicts, err := os.ReadFile(verdictsPath)
	if err != nil {
		return err
	}
	f.FromSwift = json.RawMessage(verdicts)
	out, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(fixturePath, append(out, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("merged %s into %s\n", verdictsPath, fixturePath)
	return nil
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: sealcore-golden generate <out.json> | verify <in.json> <out.json> | merge <fixture.json> <verdicts.json>")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = generate(os.Args[2])
	case "verify":
		if len(os.Args) < 4 {
			err = fmt.Errorf("verify needs an input and an output path")
		} else {
			err = verify(os.Args[2], os.Args[3])
		}
	case "merge":
		if len(os.Args) < 4 {
			err = fmt.Errorf("merge needs a fixture and a verdicts path")
		} else {
			err = merge(os.Args[2], os.Args[3])
		}
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
