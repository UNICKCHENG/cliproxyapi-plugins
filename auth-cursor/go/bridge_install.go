package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// bridgeBinaryEnv lets an operator point the plugin at a bridge they installed themselves,
// which is also how the tests avoid the download entirely.
const bridgeBinaryEnv = "CURSOR_SDK_BRIDGE_BIN"

// bridgeDirName names the bridge tree under the user's cache directory. It follows the
// distribution id (auth-cursor), not the provider key (cursor).
const bridgeDirName = "auth-cursor-bridge"

// bridgeArchiveMaxBytes bounds what the extractor will write for one archive member. The pinned
// archives are tens of megabytes; anything past this is a malformed or hostile tar rather than a
// bridge, and the limit keeps a decompression bomb from filling the cache directory.
const bridgeArchiveMaxBytes = 512 << 20

// bridgeDownloadMaxBytes bounds the compressed archive fetch. The pinned archives are tens of
// megabytes; this is enough headroom without letting a hostile endpoint fill the disk.
const bridgeDownloadMaxBytes = 256 << 20

// bridgeDownloadTimeout bounds the archive fetch. A timeout is acceptable here because this is
// installation, not an established upstream call.
const bridgeDownloadTimeout = 15 * time.Minute

// resolveBridgeBinary locates the bridge executable, installing the pinned release if needed.
//
// Search order, most explicit first: the CURSOR_SDK_BRIDGE_BIN environment variable, the
// configured bridge-path, the copy this plugin already unpacked into the user's cache directory,
// and finally a fresh download of the pinned release.
func resolveBridgeBinary(cfg pluginConfig) (string, error) {
	if configured := strings.TrimSpace(os.Getenv(bridgeBinaryEnv)); configured != "" {
		return validateBridgeBinary(configured, bridgeBinaryEnv)
	}
	if cfg.BridgePath != "" {
		return validateBridgeBinary(cfg.BridgePath, "bridge-path")
	}
	root, errRoot := bridgeInstallRoot()
	if errRoot != nil {
		return "", errRoot
	}
	installed := filepath.Join(root, "bin", bridgeExecutableName())
	errVerify := error(nil)
	if fileExists(installed) {
		if errVerify = verifyInstalledBridge(root); errVerify == nil {
			return installed, nil
		}
	}
	if errInstall := installBridge(cfg, root); errInstall != nil {
		// An install that cannot run leaves the provider with nothing, so an existing tree is
		// still used rather than failing every request. It is reported at warn because the copy
		// on disk is the unverified one the reinstall was meant to replace.
		if fileExists(installed) {
			hostLog("warn", "cursor sdk bridge reinstall failed, using the existing unverified install", map[string]any{
				"path":   installed,
				"reason": errInstall.Error(),
				"verify": errorText(errVerify),
			})
			return installed, nil
		}
		return "", errInstall
	}
	if !fileExists(installed) {
		return "", fmt.Errorf("cursor sdk bridge missing at %s after install", installed)
	}
	return installed, nil
}

// validateBridgeBinary accepts either the executable itself or the directory the archive
// unpacks into, so an operator can point the setting at whichever they have on hand.
func validateBridgeBinary(path, source string) (string, error) {
	candidates := []string{path, filepath.Join(path, bridgeExecutableName()), filepath.Join(path, "bin", bridgeExecutableName())}
	for _, candidate := range candidates {
		if fileExists(candidate) {
			return filepath.Abs(candidate)
		}
	}
	return "", fmt.Errorf("cursor sdk bridge not found at %s %q (looked for %s, %s/%s and %s/bin/%s)",
		source, path, path, path, bridgeExecutableName(), path, bridgeExecutableName())
}

// bridgeInstallRoot keys the unpacked tree by bridge version so that upgrading the plugin to a
// different pinned release never reuses a binary built for another sdk.v1 revision.
//
// The cache directory rather than ~/.cli-proxy-api: the latter is the host's default auth-dir,
// where every file is scanned as a credential candidate.
func bridgeInstallRoot() (string, error) {
	cache, errCache := os.UserCacheDir()
	if errCache != nil {
		return "", fmt.Errorf("resolve cache directory for the cursor sdk bridge: %w", errCache)
	}
	return filepath.Join(cache, "cli-proxy-api", bridgeDirName, bridgeVersion), nil
}

// bridgeStateRoot is where the bridge keeps durable local agent state. Keying it by plugin
// version keeps an upgrade from inheriting a store written by a different protocol, and confines
// the SQLite files to one directory that can be removed wholesale.
func bridgeStateRoot() (string, error) {
	cache, errCache := os.UserCacheDir()
	if errCache != nil {
		return "", fmt.Errorf("resolve cache directory for the cursor sdk bridge state: %w", errCache)
	}
	return filepath.Join(cache, "cli-proxy-api", bridgeDirName, pluginVersion, "state"), nil
}

// installBridge downloads, verifies and unpacks the pinned release into root.
//
// The tree is staged beside its destination and moved into place only after the whole archive
// has been written, so an interrupted install can never leave a half-unpacked directory that the
// next start would mistake for a usable bridge.
func installBridge(cfg pluginConfig, root string) error {
	platform, errPlatform := bridgePlatform(runtime.GOOS, runtime.GOARCH)
	if errPlatform != nil {
		return errPlatform
	}
	archiveURL := bridgeArchiveURL(platform)
	hostLog("info", "downloading cursor sdk bridge", map[string]any{
		"version": bridgeVersion,
		"url":     archiveURL,
	})

	parent := filepath.Dir(root)
	if errMkdir := os.MkdirAll(parent, 0o700); errMkdir != nil {
		return fmt.Errorf("create cursor sdk bridge directory %s: %w", parent, errMkdir)
	}
	staging, errStaging := os.MkdirTemp(parent, ".staging-*")
	if errStaging != nil {
		return fmt.Errorf("create cursor sdk bridge staging directory: %w", errStaging)
	}
	defer os.RemoveAll(staging)

	archive, errDownload := downloadBridgeArchive(cfg, archiveURL, staging)
	if errDownload != nil {
		return errDownload
	}
	if errVerify := verifyFileSHA256(archive, bridgeArchiveChecksums[platform]); errVerify != nil {
		return fmt.Errorf("verify %s: %w", archiveURL, errVerify)
	}
	unpacked := filepath.Join(staging, "unpacked")
	if errExtract := extractTarGz(archive, unpacked); errExtract != nil {
		return fmt.Errorf("unpack %s: %w", archiveURL, errExtract)
	}
	// A pre-existing tree has to be moved out of the way first: renaming onto a non-empty
	// directory fails, and the old binary must not inherit the stamp written for this download.
	replaced := ""
	if _, errStat := os.Stat(root); errStat == nil {
		replaced = root + ".replaced-" + fmt.Sprint(os.Getpid())
		if errAside := os.Rename(root, replaced); errAside != nil {
			return fmt.Errorf("replace cursor sdk bridge at %s: %w", root, errAside)
		}
	}
	if errRename := os.Rename(unpacked, root); errRename != nil {
		if replaced != "" {
			_ = os.Rename(replaced, root)
		}
		// A concurrent install of the same version is the benign case: the destination now
		// holds a tree another process verified, which is all the caller needs.
		if fileExists(filepath.Join(root, "bin", bridgeExecutableName())) {
			return nil
		}
		return fmt.Errorf("move cursor sdk bridge into %s: %w", root, errRename)
	}
	if replaced != "" {
		_ = os.RemoveAll(replaced)
	}
	if errStamp := stampInstalledBridge(root, platform); errStamp != nil {
		return errStamp
	}
	hostLog("info", "cursor sdk bridge installed", map[string]any{"version": bridgeVersion, "path": root})
	return nil
}

// downloadBridgeArchive fetches the archive into dir and returns its path.
//
// The fetch goes out over net/http rather than the host's HTTP bridge because host.http.do
// returns the whole body in one envelope, and these archives are tens of megabytes.
func downloadBridgeArchive(cfg pluginConfig, archiveURL, dir string) (string, error) {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if proxy := strings.TrimSpace(cfg.ProxyURL); proxy != "" {
		parsed, errParse := url.Parse(proxy)
		if errParse != nil {
			return "", fmt.Errorf("parse proxy-url %q: %w", proxy, errParse)
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	client := &http.Client{Transport: transport, Timeout: bridgeDownloadTimeout}

	resp, errGet := client.Get(archiveURL)
	if errGet != nil {
		return "", fmt.Errorf("download %s: %w", archiveURL, errGet)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: unexpected status %d", archiveURL, resp.StatusCode)
	}

	path := filepath.Join(dir, "bridge.tar.gz")
	file, errCreate := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if errCreate != nil {
		return "", fmt.Errorf("create %s: %w", path, errCreate)
	}
	defer file.Close()
	written, errCopy := io.Copy(file, io.LimitReader(resp.Body, bridgeDownloadMaxBytes+1))
	if errCopy != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("write %s: %w", path, errCopy)
	}
	if written > bridgeDownloadMaxBytes {
		_ = os.Remove(path)
		return "", fmt.Errorf("download %s exceeds %d bytes", archiveURL, bridgeDownloadMaxBytes)
	}
	return path, nil
}

func verifyFileSHA256(path, want string) error {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return errors.New("no checksum is recorded for this platform")
	}
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return errOpen
	}
	defer file.Close()
	digest := sha256.New()
	if _, errCopy := io.Copy(digest, file); errCopy != nil {
		return errCopy
	}
	got := hex.EncodeToString(digest.Sum(nil))
	if got != want {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, want)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return "", errOpen
	}
	defer file.Close()
	digest := sha256.New()
	if _, errCopy := io.Copy(digest, file); errCopy != nil {
		return "", errCopy
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func stampInstalledBridge(root, platform string) error {
	archiveSum := strings.ToLower(strings.TrimSpace(bridgeArchiveChecksums[platform]))
	if archiveSum == "" {
		return errors.New("no checksum is recorded for this platform")
	}
	binary := filepath.Join(root, "bin", bridgeExecutableName())
	binarySum, errHash := fileSHA256(binary)
	if errHash != nil {
		return errHash
	}
	if errWrite := os.WriteFile(filepath.Join(root, "archive.sha256"), []byte(archiveSum+"\n"), 0o600); errWrite != nil {
		return errWrite
	}
	if errWrite := os.WriteFile(filepath.Join(root, "bin.sha256"), []byte(binarySum+"\n"), 0o600); errWrite != nil {
		return errWrite
	}
	return nil
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func verifyInstalledBridge(root string) error {
	platform, errPlatform := bridgePlatform(runtime.GOOS, runtime.GOARCH)
	if errPlatform != nil {
		return errPlatform
	}
	wantArchive := strings.ToLower(strings.TrimSpace(bridgeArchiveChecksums[platform]))
	rawArchive, errRead := os.ReadFile(filepath.Join(root, "archive.sha256"))
	if errRead != nil {
		return errRead
	}
	if strings.TrimSpace(string(rawArchive)) != wantArchive {
		return fmt.Errorf("installed bridge archive stamp does not match the pinned checksum")
	}
	rawBinary, errRead := os.ReadFile(filepath.Join(root, "bin.sha256"))
	if errRead != nil {
		return errRead
	}
	return verifyFileSHA256(filepath.Join(root, "bin", bridgeExecutableName()), strings.TrimSpace(string(rawBinary)))
}

// extractTarGz unpacks archive into dir. The bridge archives hold regular files, directories and
// symlinks only; any other member type, and any path that would escape dir, aborts the install.
func extractTarGz(archive, dir string) error {
	file, errOpen := os.Open(archive)
	if errOpen != nil {
		return errOpen
	}
	defer file.Close()
	decompressed, errGzip := gzip.NewReader(file)
	if errGzip != nil {
		return errGzip
	}
	defer decompressed.Close()

	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return errMkdir
	}
	reader := tar.NewReader(decompressed)
	for {
		header, errNext := reader.Next()
		if errNext == io.EOF {
			return nil
		}
		if errNext != nil {
			return errNext
		}
		target, errTarget := safeJoin(dir, header.Name)
		if errTarget != nil {
			return errTarget
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if errMkdir := os.MkdirAll(target, 0o700); errMkdir != nil {
				return errMkdir
			}
		case tar.TypeReg:
			if errWrite := writeArchiveFile(reader, target, header.FileInfo().Mode()); errWrite != nil {
				return errWrite
			}
		case tar.TypeSymlink:
			// The bridge archives use relative symlinks inside their own tree; an absolute or
			// escaping target would let the archive write outside the cache directory.
			if _, errLink := safeJoin(filepath.Dir(target), header.Linkname); errLink != nil {
				return errLink
			}
			if errMkdir := os.MkdirAll(filepath.Dir(target), 0o700); errMkdir != nil {
				return errMkdir
			}
			_ = os.Remove(target)
			if errLink := os.Symlink(header.Linkname, target); errLink != nil {
				return errLink
			}
		default:
			return fmt.Errorf("unsupported archive member %q (type %d)", header.Name, header.Typeflag)
		}
	}
}

func writeArchiveFile(reader io.Reader, target string, mode os.FileMode) error {
	if errMkdir := os.MkdirAll(filepath.Dir(target), 0o700); errMkdir != nil {
		return errMkdir
	}
	// The archive's own permissions are honoured for the execute bit only: the bridge
	// entrypoint has to stay runnable, but nothing in the tree should be group or world
	// readable inside the user's cache directory.
	perm := os.FileMode(0o600)
	if mode&0o100 != 0 {
		perm = 0o700
	}
	file, errCreate := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if errCreate != nil {
		return errCreate
	}
	defer file.Close()
	written, errCopy := io.Copy(file, io.LimitReader(reader, bridgeArchiveMaxBytes+1))
	if errCopy != nil {
		return errCopy
	}
	if written > bridgeArchiveMaxBytes {
		return fmt.Errorf("archive member %s exceeds %d bytes", target, bridgeArchiveMaxBytes)
	}
	return nil
}

// safeJoin resolves name inside dir, rejecting anything that would land outside it.
func safeJoin(dir, name string) (string, error) {
	if name == "" {
		return "", errors.New("archive member has an empty name")
	}
	// Tar paths are slash-separated regardless of the platform that reads them.
	cleaned := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || cleaned == ".." {
		return "", fmt.Errorf("archive member %q escapes the extraction directory", name)
	}
	return filepath.Join(dir, cleaned), nil
}
