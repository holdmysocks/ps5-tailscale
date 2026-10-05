package main

import (
	"crypto/ed25519"
	"strings"
	"testing"
)

func TestBackupRoundTrip(t *testing.T) {
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	text, err := sealBackup(key, []byte("correct horse"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, b64(key.Seed())) {
		t.Fatal("the backup contains the key in the clear")
	}
	got, err := openBackup([]byte(text), []byte("correct horse"))
	if err != nil || !got.Equal(key) {
		t.Fatalf("restore: %v", err)
	}
	if _, err := openBackup([]byte(text), []byte("wrong")); err == nil {
		t.Error("a wrong passphrase was accepted")
	}
	if _, err := openBackup([]byte("nonsense"), []byte("x")); err == nil {
		t.Error("nonsense was accepted as a backup")
	}
}
