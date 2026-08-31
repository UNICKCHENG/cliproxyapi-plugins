package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSyncSidecarAssetsWritesEmbeddedFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if errSync := syncSidecarAssets(dir); errSync != nil {
		t.Fatalf("syncSidecarAssets() error = %v", errSync)
	}
	for _, name := range sidecarAssetNames {
		want, errEmbedded := sidecarAssets.ReadFile("sidecar/" + name)
		if errEmbedded != nil {
			t.Fatalf("read embedded %s: %v", name, errEmbedded)
		}
		got, errRead := os.ReadFile(filepath.Join(dir, name))
		if errRead != nil {
			t.Fatalf("read released %s: %v", name, errRead)
		}
		if string(got) != string(want) {
			t.Fatalf("%s content mismatch after release", name)
		}
	}
}

// A library upgrade can change the sidecar protocol, so a stale file on disk must be
// replaced rather than reused.
func TestSyncSidecarAssetsReplacesStaleFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stale := filepath.Join(dir, "index.mjs")
	if errWrite := os.WriteFile(stale, []byte("// stale"), 0o600); errWrite != nil {
		t.Fatalf("seed stale file: %v", errWrite)
	}
	if errSync := syncSidecarAssets(dir); errSync != nil {
		t.Fatalf("syncSidecarAssets() error = %v", errSync)
	}
	got, errRead := os.ReadFile(stale)
	if errRead != nil {
		t.Fatalf("read released index.mjs: %v", errRead)
	}
	if string(got) == "// stale" {
		t.Fatal("stale index.mjs was not replaced")
	}
}

func TestEnsureSidecarDependenciesSkipsInstalledTree(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if errMkdir := os.MkdirAll(filepath.Join(dir, "node_modules", "@cursor", "sdk"), 0o700); errMkdir != nil {
		t.Fatalf("seed node_modules: %v", errMkdir)
	}
	// An unusable npm path proves the installed tree short-circuits before npm runs.
	cfg := pluginConfig{NodePath: filepath.Join(dir, "missing-node")}
	if errEnsure := ensureSidecarDependencies(cfg, dir); errEnsure != nil {
		t.Fatalf("ensureSidecarDependencies() error = %v", errEnsure)
	}
}

func TestSidecarBootstrapDirIsVersioned(t *testing.T) {
	t.Parallel()

	dir, errDir := sidecarBootstrapDir()
	if errDir != nil {
		t.Fatalf("sidecarBootstrapDir() error = %v", errDir)
	}
	if filepath.Base(dir) != pluginVersion {
		t.Fatalf("bootstrap dir = %q, want it keyed by version %q", dir, pluginVersion)
	}
	if filepath.Base(filepath.Dir(dir)) != sidecarDirName {
		t.Fatalf("bootstrap dir = %q, want parent %q", dir, sidecarDirName)
	}
}

// The bootstrap tree must stay out of the host's default auth-dir: everything under it is
// scanned as a credential candidate, and an npm tree there floods the auth file list with
// node_modules manifests that cannot be deleted through the management API.
func TestSidecarBootstrapDirIsOutsideAuthDir(t *testing.T) {
	dir, errDir := sidecarBootstrapDir()
	if errDir != nil {
		t.Fatalf("sidecarBootstrapDir() error = %v", errDir)
	}
	home, errHome := os.UserHomeDir()
	if errHome != nil {
		t.Skipf("home directory is required: %v", errHome)
	}
	authDir := filepath.Join(home, ".cli-proxy-api")
	relative, errRelative := filepath.Rel(authDir, dir)
	if errRelative == nil && !strings.HasPrefix(relative, "..") {
		t.Fatalf("bootstrap dir = %q, want it outside the default auth-dir %q", dir, authDir)
	}
}

func TestRemoveLegacySidecarRootDeletesTreeInAuthDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	legacy := filepath.Join(home, ".cli-proxy-api", sidecarDirName, "0.0.0", "node_modules", "@cursor", "sdk")
	if errMkdir := os.MkdirAll(legacy, 0o700); errMkdir != nil {
		t.Fatalf("seed legacy sidecar tree: %v", errMkdir)
	}
	manifest := filepath.Join(legacy, "package.json")
	if errWrite := os.WriteFile(manifest, []byte(`{"name":"@cursor/sdk"}`), 0o600); errWrite != nil {
		t.Fatalf("seed legacy manifest: %v", errWrite)
	}

	removeLegacySidecarRoot()

	if directoryExists(filepath.Join(home, ".cli-proxy-api", sidecarDirName)) {
		t.Fatal("legacy sidecar root survived the cleanup")
	}
	// Only the plugin's own directory may be removed; the auth directory holds credentials.
	if !directoryExists(filepath.Join(home, ".cli-proxy-api")) {
		t.Fatal("cleanup removed the auth directory itself")
	}
}

func TestRemoveLegacySidecarRootIgnoresMissingTree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	removeLegacySidecarRoot()
}

func TestResolveSidecarScriptPrefersConfiguredPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := filepath.Join(dir, "index.mjs")
	if errWrite := os.WriteFile(script, []byte("// sidecar"), 0o600); errWrite != nil {
		t.Fatalf("write sidecar: %v", errWrite)
	}

	// A directory is accepted and resolved to its entrypoint.
	resolved, errResolve := resolveSidecarScript(pluginConfig{SidecarPath: dir})
	if errResolve != nil {
		t.Fatalf("resolveSidecarScript(dir) error = %v", errResolve)
	}
	if resolved != script {
		t.Fatalf("resolved = %q, want %q", resolved, script)
	}

	if _, errMissing := resolveSidecarScript(pluginConfig{SidecarPath: filepath.Join(dir, "absent")}); errMissing == nil {
		t.Fatal("resolveSidecarScript() error = nil for a missing sidecar-path")
	}
}

func TestNodeDirectoryResolvesExplicitPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	node := filepath.Join(dir, "node")
	if errWrite := os.WriteFile(node, []byte("#!/bin/sh\n"), 0o700); errWrite != nil {
		t.Fatalf("write node: %v", errWrite)
	}
	if got := nodeDirectory(node); got != dir {
		t.Fatalf("nodeDirectory(%q) = %q, want %q", node, got, dir)
	}
	if got := nodeDirectory(""); got != "" {
		t.Fatalf("nodeDirectory(\"\") = %q, want empty", got)
	}
}

// TestBootstrapSidecarScriptInstallsDependencies exercises the full bootstrap, including
// the npm install a store-installed plugin performs on its first request. It needs network
// access and takes tens of seconds, so it is opt-in.
func TestBootstrapSidecarScriptInstallsDependencies(t *testing.T) {
	if os.Getenv("CURSOR_PLUGIN_BOOTSTRAP_TEST") != "1" {
		t.Skip("set CURSOR_PLUGIN_BOOTSTRAP_TEST=1 to run the npm bootstrap")
	}
	node, errLook := exec.LookPath("node")
	if errLook != nil {
		t.Skipf("node is required: %v", errLook)
	}
	// bootstrapSidecarScript resolves the cache and home directories, which t.Setenv redirects.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	legacyRoot := filepath.Join(home, ".cli-proxy-api", sidecarDirName)
	if errMkdir := os.MkdirAll(filepath.Join(legacyRoot, "0.0.0"), 0o700); errMkdir != nil {
		t.Fatalf("seed legacy sidecar tree: %v", errMkdir)
	}

	script, errBootstrap := bootstrapSidecarScript(pluginConfig{NodePath: node})
	if errBootstrap != nil {
		t.Fatalf("bootstrapSidecarScript() error = %v", errBootstrap)
	}
	if !fileExists(script) {
		t.Fatalf("bootstrapped script %q does not exist", script)
	}
	if strings.HasPrefix(script, filepath.Join(home, ".cli-proxy-api")+string(filepath.Separator)) {
		t.Fatalf("bootstrapped script %q landed in the default auth-dir", script)
	}
	if directoryExists(legacyRoot) {
		t.Fatalf("bootstrap left the legacy tree %q inside the auth-dir", legacyRoot)
	}
	if !directoryExists(filepath.Join(filepath.Dir(script), "node_modules", "@cursor", "sdk")) {
		t.Fatal("bootstrap did not install @cursor/sdk")
	}
	// The sidecar must load and exit cleanly once stdin closes, which also proves the
	// embedded scripts and the installed SDK are mutually resolvable.
	cmd := exec.Command(node, script)
	cmd.Dir = filepath.Dir(script)
	cmd.Stdin = strings.NewReader("")
	if output, errRun := cmd.CombinedOutput(); errRun != nil {
		t.Fatalf("bootstrapped sidecar failed: %v: %s", errRun, output)
	}
}

// npm is resolved next to the configured Node first so that a version-manager toolchain
// works even when the service PATH does not expose npm.
func TestNPMExecutablePrefersNodeDirectory(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("npm shim names differ on windows")
	}
	dir := t.TempDir()
	node := filepath.Join(dir, "node")
	npm := filepath.Join(dir, "npm")
	for _, path := range []string{node, npm} {
		if errWrite := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); errWrite != nil {
			t.Fatalf("write %s: %v", path, errWrite)
		}
	}
	resolved, errResolve := npmExecutable(pluginConfig{NodePath: node})
	if errResolve != nil {
		t.Fatalf("npmExecutable() error = %v", errResolve)
	}
	if resolved != npm {
		t.Fatalf("npmExecutable() = %q, want %q", resolved, npm)
	}
}
