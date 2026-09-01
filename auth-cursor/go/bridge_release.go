package main

import (
	"fmt"
	"runtime"
)

// bridgeVersion pins the cursor/sdk-bridge release this plugin drives. The release tag matches
// the @cursor/sdk version the bridge embeds, so bumping it changes both the sdk.v1 contract in
// proto/ and the checksums below; the three must move together.
const bridgeVersion = "1.0.30"

// bridgeReleaseBaseURL is the GitHub release download root for the pinned bridge. It is a
// variable so that the install tests can serve an archive locally instead of reaching GitHub.
var bridgeReleaseBaseURL = "https://github.com/cursor/sdk-bridge/releases/download"

// bridgeArchiveChecksums records the SHA256 of every archive in the pinned release, keyed by the
// bridge's own platform token.
//
// The sums are compiled in rather than read from the release's SHA256SUMS.txt: a checksum served
// from the same place as the archive it describes proves only that the two agree, so verifying
// against it would leave the download trusting whatever the endpoint returned.
var bridgeArchiveChecksums = map[string]string{
	"darwin-arm64": "7aa9aa579fe8c2307e20a3d5bd1d5dffe2b8cd27975d0e722a723a2ab1c1b498",
	"darwin-x64":   "eb197e2705cd0e5a8278db984832d1d3d2b1785448cf24aef5e5f08a32811384",
	"linux-arm64":  "c32ce544f4b83d82fbd9284e56be3d04f7b85885d9e451e47fd6cae79c5a016f",
	"linux-x64":    "765721e3a0cb334c3b590bd4998bbf79f3962999aa6ea3d8adf403558a4d4f3c",
	"win32-x64":    "77f98c104f9c176d0a8001bc48fc6e967ea606f5e01adaca99c640f6bda951ec",
}

// bridgePlatform renders the running platform in the bridge's own naming, which differs from
// Go's on both axes: the OS is Node's "win32" and the architecture is "x64".
func bridgePlatform(goos, goarch string) (string, error) {
	var os string
	switch goos {
	case "darwin", "linux":
		os = goos
	case "windows":
		os = "win32"
	default:
		return "", fmt.Errorf("cursor sdk bridge is not published for %s", goos)
	}
	var arch string
	switch goarch {
	case "amd64":
		arch = "x64"
	case "arm64":
		arch = "arm64"
	default:
		return "", fmt.Errorf("cursor sdk bridge is not published for %s/%s", goos, goarch)
	}
	platform := os + "-" + arch
	if _, published := bridgeArchiveChecksums[platform]; !published {
		return "", fmt.Errorf("cursor sdk bridge %s has no %s archive", bridgeVersion, platform)
	}
	return platform, nil
}

// bridgeArchiveURL is the download location of the archive for one platform.
func bridgeArchiveURL(platform string) string {
	return fmt.Sprintf("%s/v%s/cursor-sdk-bridge-standalone-%s.tar.gz",
		bridgeReleaseBaseURL, bridgeVersion, platform)
}

// bridgeExecutableName is the entrypoint inside the archive, relative to its root.
func bridgeExecutableName() string {
	if runtime.GOOS == "windows" {
		return "cursor-sdk-bridge.exe"
	}
	return "cursor-sdk-bridge"
}
