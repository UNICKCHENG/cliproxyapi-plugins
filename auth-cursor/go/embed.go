package main

import "embed"

// sidecarAssets carries the Node sidecar sources inside the shared library.
//
// The plugin store only extracts the dynamic library from a release archive, so a
// store-installed plugin has no sidecar on disk. Embedding the sources lets the plugin
// materialise them on first use instead of asking the user to download a second artifact.
//
//go:embed sidecar/index.mjs sidecar/http-bridge.mjs sidecar/package.json
var sidecarAssets embed.FS

// sidecarAssetNames lists the files released into the bootstrap directory. package.json is
// last so that a partially written directory never looks complete to npm.
var sidecarAssetNames = []string{"index.mjs", "http-bridge.mjs", "package.json"}
