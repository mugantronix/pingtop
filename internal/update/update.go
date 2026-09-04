// Package update checks GitHub Releases for a newer pingtop build,
// downloads it, and performs the rename-write-relaunch dance needed
// to replace a running Windows executable with itself.
//
// Why the rename dance: Windows won't let a process overwrite or
// delete its own running .exe file directly, but it WILL let you
// rename it — the OS keeps the open file mapping valid under the old
// inode/handle even after the directory entry is renamed. So the
// update flow is: rename the running exe aside (to "<name>.old"),
// write the new exe to the original path, spawn the new exe as a
// fresh process, then let the (now-stale) running process exit
// normally. The next launch cleans up the leftover .old file.
package update

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Repo is the GitHub "owner/name" this build checks for updates
// against. Hardcoded rather than configurable — a fork that wants a
// different update source should change this constant, which also
// makes it obvious in source control who's pointing where.
const Repo = "mugantronix/pingtop"

// Release describes the latest GitHub release relevant to updating:
// enough to compare versions and to locate the Windows asset and its
// checksum entry.
type Release struct {
	Tag          string // as GitHub reports it, e.g. "v1.2.0"
	Version      string // Tag with a leading "v" stripped, e.g. "1.2.0" — matches goreleaser's {{.Version}} used in asset filenames
	ZipURL       string // download URL for the windows_amd64 zip asset
	ChecksumsURL string // download URL for checksums.txt
	AssetName    string // filename of the zip asset, e.g. "pingtop_1.2.0_windows_amd64.zip" — used to find its line in checksums.txt
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

// FetchLatest queries the GitHub API for the latest release and
// returns the info needed to check/perform an update. Returns a nil
// Release (no error) if the release exists but doesn't have a
// windows_amd64 zip asset — this build only knows how to consume
// that shape, produced by this project's own goreleaser config; a
// hand-crafted release without it just means "nothing to offer".
func FetchLatest(ctx context.Context) (*Release, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github api: unexpected status %s", resp.Status)
	}

	var gr ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return nil, fmt.Errorf("decode github release: %w", err)
	}

	version := strings.TrimPrefix(gr.TagName, "v")
	wantZip := fmt.Sprintf("pingtop_%s_windows_amd64.zip", version)

	rel := &Release{Tag: gr.TagName, Version: version, AssetName: wantZip}
	for _, a := range gr.Assets {
		switch a.Name {
		case wantZip:
			rel.ZipURL = a.BrowserDownloadURL
		case "checksums.txt":
			rel.ChecksumsURL = a.BrowserDownloadURL
		}
	}
	if rel.ZipURL == "" {
		return nil, nil // release exists but has no matching Windows asset
	}
	return rel, nil
}

// IsNewer reports whether latest is a newer version than current.
// Both are dot-separated numeric version strings with an optional
// leading "v" (e.g. "v1.2.0", "1.10.0"); components are compared
// numerically (so "1.10.0" > "1.9.0", unlike a plain string
// comparison). A malformed or empty current version is treated as
// "always outdated" — most likely a dev build with no embedded
// version, where offering the latest release is the safe default.
func IsNewer(current, latest string) bool {
	cur := parseVersion(current)
	lat := parseVersion(latest)
	if cur == nil {
		return lat != nil
	}
	if lat == nil {
		return false
	}
	for i := 0; i < len(cur) || i < len(lat); i++ {
		var c, l int
		if i < len(cur) {
			c = cur[i]
		}
		if i < len(lat) {
			l = lat[i]
		}
		if l != c {
			return l > c
		}
	}
	return false
}

func parseVersion(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil // not a plain numeric version — treat as unparseable
		}
		out[i] = n
	}
	return out
}

// Download fetches rel's zip asset, verifies it against
// checksums.txt (skipped, not failed, if ChecksumsURL is empty — an
// older release built before checksums were wired up shouldn't block
// updating past it), extracts pingtop.exe into a fresh temp
// directory, and returns its path.
func Download(ctx context.Context, rel *Release) (exePath string, err error) {
	zipBytes, err := fetchBytes(ctx, rel.ZipURL)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", rel.AssetName, err)
	}

	if rel.ChecksumsURL != "" {
		sums, err := fetchBytes(ctx, rel.ChecksumsURL)
		if err != nil {
			return "", fmt.Errorf("download checksums.txt: %w", err)
		}
		if err := verifyChecksum(zipBytes, rel.AssetName, sums); err != nil {
			return "", err
		}
	}

	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return "", fmt.Errorf("open downloaded zip: %w", err)
	}

	var exeFile *zip.File
	for _, f := range zr.File {
		if strings.EqualFold(filepath.Base(f.Name), "pingtop.exe") {
			exeFile = f
			break
		}
	}
	if exeFile == nil {
		return "", fmt.Errorf("pingtop.exe not found inside %s", rel.AssetName)
	}

	dir, err := os.MkdirTemp("", "pingtop-update-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	dest := filepath.Join(dir, "pingtop.exe")

	rc, err := exeFile.Open()
	if err != nil {
		return "", fmt.Errorf("open pingtop.exe in zip: %w", err)
	}
	defer rc.Close()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", dest, err)
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return "", fmt.Errorf("write %s: %w", dest, err)
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("close %s: %w", dest, err)
	}

	return dest, nil
}

func fetchBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// verifyChecksum finds assetName's line in checksums.txt (goreleaser's
// default format: "<sha256 hex>  <filename>", one per line) and
// confirms it matches the sha256 of data.
func verifyChecksum(data []byte, assetName string, checksumsTxt []byte) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])

	for _, line := range strings.Split(string(checksumsTxt), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if fields[1] == assetName {
			if fields[0] != got {
				return fmt.Errorf("checksum mismatch for %s: got %s, want %s", assetName, got, fields[0])
			}
			return nil
		}
	}
	return fmt.Errorf("no checksum entry found for %s", assetName)
}

// ReplaceAndRelaunch installs newExePath over the currently running
// executable and starts it as a new, detached process. On success the
// caller should exit promptly — there is nothing left for the current
// process to do, and the new process is already running independently.
// On failure the original executable is left in place (best effort:
// this function tries to restore it before returning an error).
func ReplaceAndRelaunch(newExePath string) error {
	curExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("os.Executable: %w", err)
	}
	curExe, err = filepath.EvalSymlinks(curExe)
	if err != nil {
		return fmt.Errorf("resolve current exe path: %w", err)
	}

	oldPath := curExe + ".old"
	_ = os.Remove(oldPath) // best-effort: clear out a leftover from a previous update

	if err := os.Rename(curExe, oldPath); err != nil {
		return fmt.Errorf("rename running exe aside: %w", err)
	}

	if err := copyFile(newExePath, curExe); err != nil {
		if restoreErr := os.Rename(oldPath, curExe); restoreErr != nil {
			return fmt.Errorf("install new exe: %w (and failed to restore original: %v)", err, restoreErr)
		}
		return fmt.Errorf("install new exe: %w", err)
	}

	cmd := exec.Command(curExe, os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Start(); err != nil {
		// Best effort: put the original back so the user isn't left
		// with a half-updated install if the relaunch itself failed.
		_ = os.Remove(curExe)
		_ = os.Rename(oldPath, curExe)
		return fmt.Errorf("relaunch: %w", err)
	}

	return nil
}

// CleanupOldExe removes a "<current exe>.old" left over from a
// previous update, if present. Best-effort and silent on failure
// (e.g. the file doesn't exist, or — rarely — is still momentarily
// locked by the just-exited previous process): this is opportunistic
// housekeeping, not something the program should ever fail to start
// over.
func CleanupOldExe() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return
	}
	_ = os.Remove(exe + ".old")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
