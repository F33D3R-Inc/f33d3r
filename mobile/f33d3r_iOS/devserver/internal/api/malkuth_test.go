package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// fixturesDir is the Kit's golden fixtures — the same files `swift test`
// reads. Both sides are tested against one artifact.
func fixturesDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "F33D3RKit", "Tests", "F33D3RKitTests", "Fixtures")
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("fixtures not found at %s: %v", dir, err)
	}
	return dir
}

func readFixture(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixturesDir(t), name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// TestCanonicalVectors: every canonical-JSON case the Swift encoder was tested
// against must produce byte-identical output and the same CID here, or the
// server would reject every work the app signs.
func TestCanonicalVectors(t *testing.T) {
	var doc struct {
		Cases []struct {
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			Canonical string          `json:"canonical"`
			CID       string          `json:"cid"`
		} `json:"cases"`
	}
	readFixture(t, "malkuth_canonical.json", &doc)
	if len(doc.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var p workCanonicalPayload
			if err := json.Unmarshal(c.Input, &p); err != nil {
				t.Fatal(err)
			}
			got, err := canonicalBytes(p)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.Canonical {
				t.Errorf("canonical bytes differ\n got: %s\nwant: %s", got, c.Canonical)
			}
			if !verifyCID(c.CID, p) {
				t.Errorf("cid %s did not verify", c.CID)
			}
		})
	}
}

// TestSignatureVectors: keys and signatures made by Go's crypto and by
// CryptoKit must verify (or fail) exactly as the fixture recorded.
func TestSignatureVectors(t *testing.T) {
	type vector struct {
		Name         string `json:"name"`
		PublicKey    string `json:"public_key_spki_b64"`
		CID          string `json:"cid"`
		Signature    string `json:"signature_b64url"`
		ShouldVerify bool   `json:"should_verify"`
		VerifiedByGo *bool  `json:"verified_by_go"`
	}
	var doc struct {
		FromGo    []vector `json:"from_go"`
		FromSwift []vector `json:"from_swift"`
	}
	readFixture(t, "malkuth_signatures.json", &doc)
	for _, v := range append(doc.FromGo, doc.FromSwift...) {
		t.Run(v.Name, func(t *testing.T) {
			want := v.ShouldVerify
			if v.VerifiedByGo != nil {
				want = *v.VerifiedByGo
			}
			if got := verifySignature(v.PublicKey, v.CID, v.Signature); got != want {
				t.Errorf("verify = %v, want %v", got, want)
			}
		})
	}
}

func TestDeriveTags(t *testing.T) {
	got := deriveTags([]string{"Film", "35mm"}, "four frames #film #Roll #35MM #roll")
	want := []string{"Film", "35mm", "Roll"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}
