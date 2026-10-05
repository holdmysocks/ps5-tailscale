// Package relsig signs and verifies releases.
//
// A release's payload comes with a small signature file. It names the
// version and the SHA-256 of the payload and carries an Ed25519 signature
// over both, made with a key that never leaves the maintainer's machines.
// The daemon has the public half built in and installs an update only if the
// signature checks out, so a tampered release, or one put up by someone who
// got into the hosting account, is turned down.
//
// The version is part of what is signed, so a signature cannot be reused to
// pass an older payload off as a newer release.
package relsig

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// FileSuffix is appended to a payload's name for its signature file.
const FileSuffix = ".sig"

const domain = "ps5-tailscale-release-v1"

// Signature is the content of a signature file.
type Signature struct {
	Version string // without a leading "v"
	SHA256  string // lower-case hex of the payload's SHA-256
	Sig     []byte
}

// message is what gets signed.
func message(version, sum string) []byte {
	return []byte(domain + "\nversion=" + version + "\nsha256=" + sum + "\n")
}

func clean(version, sum string) (string, string, error) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	sum = strings.ToLower(strings.TrimSpace(sum))
	if version == "" || strings.ContainsAny(version, " \t\r\n=") {
		return "", "", errors.New("invalid version")
	}
	if b, err := hex.DecodeString(sum); err != nil || len(b) != 32 {
		return "", "", errors.New("invalid SHA-256")
	}
	return version, sum, nil
}

// Sign signs a payload's version and SHA-256.
func Sign(key ed25519.PrivateKey, version, sum string) (Signature, error) {
	version, sum, err := clean(version, sum)
	if err != nil {
		return Signature{}, err
	}
	return Signature{Version: version, SHA256: sum, Sig: ed25519.Sign(key, message(version, sum))}, nil
}

// Verify reports whether the signature was made with the private half of pub.
func (s Signature) Verify(pub ed25519.PublicKey) bool {
	return len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, message(s.Version, s.SHA256), s.Sig)
}

// Marshal renders the signature file.
func (s Signature) Marshal() []byte {
	return []byte(fmt.Sprintf("version: %s\nsha256: %s\nsignature: %s\n",
		s.Version, s.SHA256, base64.StdEncoding.EncodeToString(s.Sig)))
}

// Parse reads a signature file.
func Parse(b []byte) (Signature, error) {
	fields, err := ParseFields(b)
	if err != nil {
		return Signature{}, err
	}
	version, sum, err := clean(fields["version"], fields["sha256"])
	if err != nil {
		return Signature{}, err
	}
	sig, err := base64.StdEncoding.DecodeString(fields["signature"])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return Signature{}, errors.New("invalid signature field")
	}
	return Signature{Version: version, SHA256: sum, Sig: sig}, nil
}

// ParseFields reads "name: value" lines. Lines without a colon are ignored,
// which leaves room for a heading.
func ParseFields(b []byte) (map[string]string, error) {
	if len(b) > 16<<10 {
		return nil, errors.New("file too large")
	}
	fields := map[string]string{}
	for _, line := range bytes.Split(b, []byte("\n")) {
		name, value, ok := strings.Cut(string(line), ":")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	return fields, nil
}

// ParsePublicKey reads a base64 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("invalid public key")
	}
	return ed25519.PublicKey(b), nil
}
