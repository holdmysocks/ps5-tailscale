// Command signrelease manages the release signing key and signs payloads.
//
//	go run ./cmd/signrelease keygen                  create the key (once)
//	go run ./cmd/signrelease pubkey                  print the public key
//	go run ./cmd/signrelease sign -version 1.2.3 -file tailscale.elf
//	go run ./cmd/signrelease verify -file tailscale.elf
//	go run ./cmd/signrelease backup -out FILE        passphrase-protected copy
//	go run ./cmd/signrelease restore -in FILE        bring a backup onto this machine
//
// The key lives outside the repository, by default in .ps5-tailscale in the
// user's home directory; PS5TS_SIGNING_KEY names another file. It is kept
// unencrypted there so that releases can be made without typing anything.
// A backup is encrypted with a passphrase and is meant for somewhere else: a
// NAS, a USB stick, a password manager.
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/term"

	"ps5tailscale/relsig"
)

const (
	keyHeading    = "ps5-tailscale release signing key. Keep this file private."
	backupHeading = "ps5-tailscale release signing key, encrypted backup."
	kdfRounds     = 600_000
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "keygen":
		err = keygen(args)
	case "pubkey":
		err = pubkey(args)
	case "sign":
		err = sign(args)
	case "verify":
		err = verify(args)
	case "backup":
		err = backup(args)
	case "restore":
		err = restore(args)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "signrelease:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: signrelease keygen | pubkey | sign -version V -file F | verify -file F | backup -out F | restore -in F")
	os.Exit(2)
}

// keyPath is where the signing key is kept on this machine.
func keyPath() (string, error) {
	if p := os.Getenv("PS5TS_SIGNING_KEY"); p != "" {
		return p, nil
	}
	// The home directory itself, not the configuration directory: on Windows
	// that is AppData, which packaged apps see a private copy of, so a key
	// created from inside one would be invisible everywhere else.
	dir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ".ps5-tailscale", "release-signing.key"), nil
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func writeKey(path string, key ed25519.PrivateKey) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite a signing key", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	text := fmt.Sprintf("%s\nprivate: %s\npublic: %s\n", keyHeading, b64(key.Seed()), b64(key.Public().(ed25519.PublicKey)))
	return os.WriteFile(path, []byte(text), 0o600)
}

func loadKey() (ed25519.PrivateKey, string, error) {
	path, err := keyPath()
	if err != nil {
		return nil, "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, path, fmt.Errorf("no signing key (%w); run keygen, or restore a backup", err)
	}
	fields, err := relsig.ParseFields(b)
	if err != nil {
		return nil, path, err
	}
	seed, err := base64.StdEncoding.DecodeString(fields["private"])
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, path, fmt.Errorf("%s is not a signing key", path)
	}
	return ed25519.NewKeyFromSeed(seed), path, nil
}

func keygen(args []string) error {
	path, err := keyPath()
	if err != nil {
		return err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := writeKey(path, key); err != nil {
		return err
	}
	fmt.Printf("signing key written to %s\npublic key: %s\n", path, b64(key.Public().(ed25519.PublicKey)))
	fmt.Println("Make a backup now: signrelease backup -out <file>")
	return nil
}

func pubkey(args []string) error {
	key, _, err := loadKey()
	if err != nil {
		return err
	}
	fmt.Println(b64(key.Public().(ed25519.PublicKey)))
	return nil
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	version := fs.String("version", "", "the release's version, e.g. 1.2.3")
	file := fs.String("file", "", "the payload to sign")
	out := fs.String("out", "", "the signature file (default: the payload's name plus "+relsig.FileSuffix+")")
	fs.Parse(args)
	if *version == "" || *file == "" {
		return errors.New("sign needs -version and -file")
	}
	key, _, err := loadKey()
	if err != nil {
		return err
	}
	sum, err := fileSum(*file)
	if err != nil {
		return err
	}
	sig, err := relsig.Sign(key, *version, sum)
	if err != nil {
		return err
	}
	if *out == "" {
		*out = *file + relsig.FileSuffix
	}
	if err := os.WriteFile(*out, sig.Marshal(), 0o644); err != nil {
		return err
	}
	fmt.Printf("signed %s as version %s -> %s\n", *file, sig.Version, *out)
	return nil
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	file := fs.String("file", "", "the payload")
	sigFile := fs.String("sig", "", "the signature file (default: the payload's name plus "+relsig.FileSuffix+")")
	pub := fs.String("pubkey", "", "the public key to check against (default: this machine's signing key)")
	fs.Parse(args)
	if *file == "" {
		return errors.New("verify needs -file")
	}
	if *sigFile == "" {
		*sigFile = *file + relsig.FileSuffix
	}
	var public ed25519.PublicKey
	if *pub != "" {
		p, err := relsig.ParsePublicKey(*pub)
		if err != nil {
			return err
		}
		public = p
	} else {
		key, _, err := loadKey()
		if err != nil {
			return err
		}
		public = key.Public().(ed25519.PublicKey)
	}
	b, err := os.ReadFile(*sigFile)
	if err != nil {
		return err
	}
	sig, err := relsig.Parse(b)
	if err != nil {
		return err
	}
	sum, err := fileSum(*file)
	if err != nil {
		return err
	}
	if sum != sig.SHA256 {
		return errors.New("the payload does not match the signature file")
	}
	if !sig.Verify(public) {
		return errors.New("the signature is not valid for this key")
	}
	fmt.Printf("ok: version %s, sha256 %s\n", sig.Version, sig.SHA256)
	return nil
}

// readPassphrase asks for a passphrase without showing it.
func readPassphrase(prompt string) ([]byte, error) {
	fmt.Fprint(os.Stderr, prompt)
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, errors.New("a passphrase has to be typed at a terminal")
	}
	p, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	return p, err
}

func sealer(passphrase, salt []byte) (cipher.AEAD, error) {
	k, err := pbkdf2.Key(sha256.New, string(passphrase), salt, kdfRounds, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func backup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	out := fs.String("out", "", "where to write the encrypted backup")
	fs.Parse(args)
	if *out == "" {
		return errors.New("backup needs -out")
	}
	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("%s already exists", *out)
	}
	key, _, err := loadKey()
	if err != nil {
		return err
	}
	p1, err := readPassphrase("Passphrase for the backup: ")
	if err != nil {
		return err
	}
	if len(p1) < 8 {
		return errors.New("use at least 8 characters")
	}
	p2, err := readPassphrase("Again: ")
	if err != nil {
		return err
	}
	if string(p1) != string(p2) {
		return errors.New("the passphrases differ")
	}
	text, err := sealBackup(key, p1)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte(text), 0o600); err != nil {
		return err
	}
	fmt.Printf("encrypted backup written to %s\nWithout the passphrase it cannot be restored; keep the passphrase somewhere else.\n", *out)
	return nil
}

func restore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	in := fs.String("in", "", "the encrypted backup")
	fs.Parse(args)
	if *in == "" {
		return errors.New("restore needs -in")
	}
	path, err := keyPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite a signing key", path)
	}
	b, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	pass, err := readPassphrase("Passphrase of the backup: ")
	if err != nil {
		return err
	}
	key, err := openBackup(b, pass)
	if err != nil {
		return err
	}
	public := key.Public().(ed25519.PublicKey)
	if err := writeKey(path, key); err != nil {
		return err
	}
	fmt.Printf("signing key restored to %s\npublic key: %s\n", path, b64(public))
	return nil
}

// sealBackup encrypts the key with a passphrase.
func sealBackup(key ed25519.PrivateKey, passphrase []byte) (string, error) {
	salt, nonce := make([]byte, 16), make([]byte, 12)
	rand.Read(salt)
	rand.Read(nonce)
	aead, err := sealer(passphrase, salt)
	if err != nil {
		return "", err
	}
	public := key.Public().(ed25519.PublicKey)
	// The public key is bound to the ciphertext, so a backup cannot be
	// relabelled as another key's.
	data := aead.Seal(nil, nonce, key.Seed(), public)
	return fmt.Sprintf("%s\nRestore with: signrelease restore -in <this file>\nkdf: pbkdf2-sha256\nrounds: %d\nsalt: %s\nnonce: %s\ndata: %s\npublic: %s\n",
		backupHeading, kdfRounds, b64(salt), b64(nonce), b64(data), b64(public)), nil
}

// openBackup decrypts a backup.
func openBackup(b, passphrase []byte) (ed25519.PrivateKey, error) {
	fields, err := relsig.ParseFields(b)
	if err != nil {
		return nil, err
	}
	dec := func(name string) []byte {
		v, _ := base64.StdEncoding.DecodeString(fields[name])
		return v
	}
	rounds, _ := strconv.Atoi(fields["rounds"])
	salt, nonce, data, public := dec("salt"), dec("nonce"), dec("data"), dec("public")
	if fields["kdf"] != "pbkdf2-sha256" || rounds != kdfRounds || len(salt) == 0 || len(nonce) != 12 || len(public) != ed25519.PublicKeySize {
		return nil, errors.New("not a backup this version understands")
	}
	aead, err := sealer(passphrase, salt)
	if err != nil {
		return nil, err
	}
	seed, err := aead.Open(nil, nonce, data, public)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("wrong passphrase, or the backup is damaged")
	}
	key := ed25519.NewKeyFromSeed(seed)
	if !key.Public().(ed25519.PublicKey).Equal(ed25519.PublicKey(public)) {
		return nil, errors.New("the backup is inconsistent")
	}
	return key, nil
}
