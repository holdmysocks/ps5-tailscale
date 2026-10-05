package relsig

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestSignVerify(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("payload"))
	hexSum := hex.EncodeToString(sum[:])

	sig, err := Sign(priv, "v1.2.3", hexSum)
	if err != nil {
		t.Fatal(err)
	}
	if sig.Version != "1.2.3" {
		t.Errorf("version = %q", sig.Version)
	}
	parsed, err := Parse(sig.Marshal())
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Verify(pub) {
		t.Fatal("a good signature did not verify")
	}

	// The same signature must not pass for another version or payload.
	other := parsed
	other.Version = "1.2.4"
	if other.Verify(pub) {
		t.Error("the signature passed for another version")
	}
	other = parsed
	otherSum := sha256.Sum256([]byte("another payload"))
	other.SHA256 = hex.EncodeToString(otherSum[:])
	if other.Verify(pub) {
		t.Error("the signature passed for another payload")
	}

	// Nor with another key.
	pub2, _, _ := ed25519.GenerateKey(nil)
	if parsed.Verify(pub2) {
		t.Error("the signature passed with another key")
	}
	if parsed.Verify(nil) {
		t.Error("the signature passed with no key")
	}

	if got, err := ParsePublicKey(base64.StdEncoding.EncodeToString(pub)); err != nil || !got.Equal(pub) {
		t.Errorf("ParsePublicKey: %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	for name, text := range map[string]string{
		"empty":         "",
		"no signature":  "version: 1.0.0\nsha256: " + hex.EncodeToString(make([]byte, 32)) + "\n",
		"short sum":     "version: 1.0.0\nsha256: abcd\nsignature: " + base64.StdEncoding.EncodeToString(make([]byte, 64)) + "\n",
		"bad signature": "version: 1.0.0\nsha256: " + hex.EncodeToString(make([]byte, 32)) + "\nsignature: AAAA\n",
		"no version":    "sha256: " + hex.EncodeToString(make([]byte, 32)) + "\nsignature: " + base64.StdEncoding.EncodeToString(make([]byte, 64)) + "\n",
	} {
		if _, err := Parse([]byte(text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Sign(nil, "1.0.0", "nothex"); err == nil {
		t.Error("Sign accepted a bad sum")
	}
}
