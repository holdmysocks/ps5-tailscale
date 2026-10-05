package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"ps5tailscale/relsig"
)

// Installing a newer release from the status page.
//
// Nothing here runs by itself: an update is only ever installed when someone
// presses the button. The payload is downloaded from the release, checked
// against the release's signature with the key built in below, and handed to
// the ELF loader on this console, which is the same as sending the payload by
// hand: the new copy stops this one and takes over.

// updatePublicKey is the public half of the release signing key (base64
// Ed25519). Only releases signed with the private half can be installed from
// the status page. A fork that makes its own releases needs its own key
// (tsd/cmd/signrelease); test builds set another one with -ldflags -X.
var updatePublicKey = "HUcHCXGe3vNEeyt4frmoKZUqYDamdA1dzsr+Hsybda0="

const (
	payloadAsset  = "tailscale.elf"
	maxPayload    = 128 << 20
	loaderAddr    = "127.0.0.1:9021"
	updateDirName = "update"
)

// updateProgress is what the status page shows about an update in progress.
type updateProgress struct {
	// State is "" (nothing going on), "downloading", "verifying",
	// "installing" or "failed".
	State   string `json:"state"`
	Version string `json:"version,omitempty"`
	Percent int    `json:"percent,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (d *daemon) setUpdate(p updateProgress) {
	d.mu.Lock()
	d.update = p
	d.mu.Unlock()
}

// canInstall reports whether rel can be installed from the status page.
func canInstall(rel releaseInfo) bool {
	return newerVersion(version, rel.Version) && rel.Assets[payloadAsset] != "" && rel.Assets[payloadAsset+relsig.FileSuffix] != ""
}

// startUpdate begins installing the newest known release. It returns at
// once; progress is reported through the status.
func (d *daemon) startUpdate() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch d.update.State {
	case "downloading", "verifying", "installing":
		return errors.New("an update is already in progress")
	}
	rel := d.latest
	if !newerVersion(version, rel.Version) {
		return errors.New("no newer release is known")
	}
	if !canInstall(rel) {
		return errors.New("that release has no signed payload; install it by hand")
	}
	d.update = updateProgress{State: "downloading", Version: rel.Version}
	go func() {
		if err := d.runUpdate(rel); err != nil {
			d.logf("update to %s failed: %v", rel.Version, err)
			d.setUpdate(updateProgress{State: "failed", Version: rel.Version, Error: err.Error()})
		}
	}()
	return nil
}

func (d *daemon) runUpdate(rel releaseInfo) error {
	pub, err := relsig.ParsePublicKey(updatePublicKey)
	if err != nil {
		return errors.New("this build has no release key")
	}
	d.logf("update: downloading %s", rel.Version)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// The signature first: it is small, and says what the payload must be.
	sigBytes, err := httpGetSmall(ctx, rel.Assets[payloadAsset+relsig.FileSuffix])
	if err != nil {
		return fmt.Errorf("downloading the signature: %w", err)
	}
	sig, err := checkSignature(sigBytes, pub, rel.Version)
	if err != nil {
		return err
	}

	dir := filepath.Join(dataDir, updateDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file := filepath.Join(dir, payloadAsset)
	sum, err := d.download(ctx, rel.Assets[payloadAsset], file, rel.Version)
	if err != nil {
		os.Remove(file)
		return fmt.Errorf("downloading the payload: %w", err)
	}
	d.setUpdate(updateProgress{State: "verifying", Version: rel.Version})
	if sum != sig.SHA256 {
		os.Remove(file)
		return errors.New("the downloaded payload does not match the release's signature; not installing it")
	}
	d.logf("update: %s verified (sha256 %s)", rel.Version, sum)

	d.setUpdate(updateProgress{State: "installing", Version: rel.Version})
	d.mu.Lock()
	keep := d.cfg.PayloadPath
	d.mu.Unlock()
	if keep != "" {
		// The copy that is started at boot. A failure here is reported but
		// does not stop the update of the running instance.
		if err := replaceFile(file, keep); err != nil {
			d.logf("update: could not replace %s: %v", keep, err)
			notify("Tailscale: could not update the copy at\n%s", keep)
		} else {
			d.logf("update: replaced %s", keep)
		}
	}

	if err := sendToLoader(file); err != nil {
		return fmt.Errorf("the update is downloaded and verified (%s), but could not be started: %w", file, err)
	}
	d.logf("update: handed %s to the ELF loader", rel.Version)

	// The new instance stops this one within seconds. If it does not, its
	// start failed.
	select {
	case <-d.quit:
		return nil
	case <-time.After(90 * time.Second):
		return errors.New("the new version did not start; see /data/tailscale/launcher.log")
	}
}

// checkSignature parses a release's signature file and checks it against the
// release key and the version the release claims to be.
func checkSignature(b []byte, pub []byte, wantVersion string) (relsig.Signature, error) {
	sig, err := relsig.Parse(b)
	if err != nil {
		return sig, fmt.Errorf("the release's signature file is not valid: %w", err)
	}
	if !sig.Verify(pub) {
		return sig, errors.New("the release is not signed with this project's release key; not installing it")
	}
	if sig.Version != wantVersion {
		return sig, fmt.Errorf("the signature is for version %s, not %s; not installing it", sig.Version, wantVersion)
	}
	if !newerVersion(version, sig.Version) {
		return sig, fmt.Errorf("version %s is not newer than this one", sig.Version)
	}
	return sig, nil
}

func httpGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ps5-tailscale/"+version)
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, &httpStatusError{resp.Status}
	}
	return resp, nil
}

func httpGetSmall(ctx context.Context, url string) ([]byte, error) {
	resp, err := httpGet(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 16<<10))
}

// download saves url to file and returns the SHA-256 of what was written.
func (d *daemon) download(ctx context.Context, url, file, ver string) (string, error) {
	resp, err := httpGet(ctx, url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.ContentLength > maxPayload {
		return "", errors.New("the payload is implausibly large")
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	buf := make([]byte, 256<<10)
	var done int64
	lastPercent := -1
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if done += int64(n); done > maxPayload {
				f.Close()
				return "", errors.New("the payload is implausibly large")
			}
			h.Write(buf[:n])
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				return "", err
			}
			if resp.ContentLength > 0 {
				if p := int(done * 100 / resp.ContentLength); p != lastPercent {
					lastPercent = p
					d.setUpdate(updateProgress{State: "downloading", Version: ver, Percent: p})
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return "", rerr
		}
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if resp.ContentLength > 0 && done != resp.ContentLength {
		return "", errors.New("the download was cut short")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// replaceFile puts a copy of src at dst, by way of a temporary file next to
// dst so that dst is never left half written.
func replaceFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// sendToLoader hands a payload to the ELF loader on this console.
func sendToLoader(file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	c, err := net.DialTimeout("tcp", loaderAddr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("no ELF loader on port 9021 (%w); send the file by hand", err)
	}
	c.SetWriteDeadline(time.Now().Add(2 * time.Minute))
	// Not io.Copy(c, f) with the bare file: Go would use sendfile, which the
	// PS5 kernel refuses for sockets ("socket is not connected").
	if _, err := io.Copy(c, onlyReader{f}); err != nil {
		c.Close()
		return err
	}
	// The half-close tells the loader that the payload is complete.
	if tc, ok := c.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
	// The connection is the new payload's standard output. Keep reading it
	// for as long as this process lives, so that nothing the payload prints
	// while starting hits a closed socket.
	go func() {
		io.Copy(io.Discard, c)
		c.Close()
	}()
	return nil
}

// onlyReader hides everything about a reader but Read, so that copying from
// it takes the plain path.
type onlyReader struct{ io.Reader }

// validPayloadPath checks the setting that names the copy started at boot.
func validPayloadPath(p string) error {
	if p == "" {
		return nil
	}
	if !strings.HasPrefix(p, "/") {
		return errors.New("it must be a full path, such as /data/pldmgr/payloads/Tailscale/tailscale.elf")
	}
	if path.Clean(p) != p {
		return errors.New("it must not contain .. or doubled slashes")
	}
	if path.Ext(p) != ".elf" {
		return errors.New("it must name an .elf file")
	}
	return nil
}
