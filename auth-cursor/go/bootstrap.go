package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// sidecarDirName names the sidecar directory both under the home bootstrap root and next to
// the plugin library, so a development install and a store install stay discoverable by the
// same name. It follows the distribution id (auth-cursor), not the provider key (cursor).
const sidecarDirName = "auth-cursor-sidecar"

// bootstrapSidecarScript materialises the embedded sidecar under the user's home directory
// and returns the path to its entrypoint.
//
// A home-relative location is deliberate: the plugin store installs only the dynamic
// library, and the host process working directory is not stable (a service manager may
// start it anywhere), so neither can be used to locate the sidecar.
func bootstrapSidecarScript(cfg pluginConfig) (string, error) {
	dir, errDir := sidecarBootstrapDir()
	if errDir != nil {
		return "", errDir
	}
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return "", fmt.Errorf("create cursor sidecar directory %s: %w", dir, errMkdir)
	}
	if errSync := syncSidecarAssets(dir); errSync != nil {
		return "", errSync
	}
	if errDeps := ensureSidecarDependencies(cfg, dir); errDeps != nil {
		return "", errDeps
	}
	return filepath.Join(dir, "index.mjs"), nil
}

// sidecarBootstrapDir keys the directory by plugin version so that upgrading the library
// never reuses a sidecar built for a different protocol.
func sidecarBootstrapDir() (string, error) {
	home, errHome := os.UserHomeDir()
	if errHome != nil {
		return "", fmt.Errorf("resolve home directory for the cursor sidecar: %w", errHome)
	}
	return filepath.Join(home, ".cli-proxy-api", sidecarDirName, pluginVersion), nil
}

// syncSidecarAssets writes any embedded file that is missing or differs on disk, so a
// library upgrade that changes the sidecar protocol also refreshes the scripts.
func syncSidecarAssets(dir string) error {
	for _, name := range sidecarAssetNames {
		want, errRead := sidecarAssets.ReadFile("sidecar/" + name)
		if errRead != nil {
			return fmt.Errorf("read embedded sidecar %s: %w", name, errRead)
		}
		target := filepath.Join(dir, name)
		if current, errCurrent := os.ReadFile(target); errCurrent == nil && bytes.Equal(current, want) {
			continue
		}
		if errWrite := os.WriteFile(target, want, 0o600); errWrite != nil {
			return fmt.Errorf("write cursor sidecar %s: %w", target, errWrite)
		}
		hostLog("info", "cursor sidecar asset written", map[string]any{"path": target})
	}
	return nil
}

// ensureSidecarDependencies installs @cursor/sdk on first use. Installing from the plugin
// keeps the store install to a single artifact at the cost of one npm run per version.
func ensureSidecarDependencies(cfg pluginConfig, dir string) error {
	if directoryExists(filepath.Join(dir, "node_modules", "@cursor", "sdk")) {
		return nil
	}
	npmPath, errNPM := npmExecutable(cfg)
	if errNPM != nil {
		return errNPM
	}
	hostLog("info", "installing cursor sidecar dependencies", map[string]any{
		"npm": npmPath,
		"dir": dir,
	})
	// No timeout: npm applies its own network timeouts, and a bootstrap that is still
	// downloading must not be killed mid-install and leave a partial node_modules tree.
	cmd := exec.Command(npmPath, "install", "--omit=dev", "--no-audit", "--no-fund")
	cmd.Dir = dir
	cmd.Env = os.Environ()
	output, errRun := cmd.CombinedOutput()
	if errRun != nil {
		return fmt.Errorf("install cursor sidecar dependencies in %s: %w: %s",
			dir, errRun, strings.TrimSpace(tail(string(output), sidecarStderrTail)))
	}
	if !directoryExists(filepath.Join(dir, "node_modules", "@cursor", "sdk")) {
		return fmt.Errorf("cursor sidecar dependencies missing after npm install in %s", dir)
	}
	hostLog("info", "cursor sidecar dependencies installed", map[string]any{"dir": dir})
	return nil
}

// npmExecutable prefers the npm shipped alongside the configured Node so that a version
// manager install (mise, nvm, asdf) keeps both binaries from the same toolchain, which the
// host PATH may not expose at all.
func npmExecutable(cfg pluginConfig) (string, error) {
	names := []string{"npm"}
	if runtime.GOOS == "windows" {
		names = []string{"npm.cmd", "npm.exe", "npm"}
	}
	if nodeDir := nodeDirectory(cfg.NodePath); nodeDir != "" {
		for _, name := range names {
			candidate := filepath.Join(nodeDir, name)
			if fileExists(candidate) {
				return candidate, nil
			}
		}
	}
	for _, name := range names {
		if resolved, errLook := exec.LookPath(name); errLook == nil {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("npm not found next to %q or on PATH; install Node.js 22.13+ or set plugins.configs.cursor.sidecar-path to a prepared sidecar", cfg.NodePath)
}

// nodeDirectory resolves the directory holding the configured Node executable, following a
// bare command name through PATH.
func nodeDirectory(nodePath string) string {
	nodePath = strings.TrimSpace(nodePath)
	if nodePath == "" {
		return ""
	}
	if !strings.ContainsRune(nodePath, filepath.Separator) {
		resolved, errLook := exec.LookPath(nodePath)
		if errLook != nil {
			return ""
		}
		nodePath = resolved
	}
	absolute, errAbs := filepath.Abs(nodePath)
	if errAbs != nil {
		return ""
	}
	return filepath.Dir(absolute)
}

func directoryExists(path string) bool {
	info, errStat := os.Stat(path)
	return errStat == nil && info.IsDir()
}

// tail keeps the last limit bytes of s so that a long npm log stays readable in an error.
func tail(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[len(s)-limit:]
}
