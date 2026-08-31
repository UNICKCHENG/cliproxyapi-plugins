#!/usr/bin/env node
// Checks registry.json against the constraints CLIProxyAPI enforces when it loads a plugin
// store source, so a malformed entry fails in CI instead of breaking every client that has
// this repository configured as a source.

import { readFileSync } from "node:fs";

const registryPath = process.argv[2] ?? "registry.json";
const registry = JSON.parse(readFileSync(registryPath, "utf8"));
const problems = [];

if (registry.schema_version !== 2) {
  problems.push(`schema_version is ${registry.schema_version}, want 2`);
}
if (!Array.isArray(registry.plugins)) {
  problems.push("plugins must be an array");
}

for (const plugin of registry.plugins ?? []) {
  const label = plugin.id ?? "<missing id>";
  for (const field of ["id", "name", "version", "repository"]) {
    if (!plugin[field]) {
      problems.push(`${label} is missing ${field}`);
    }
  }
  // The host only accepts "direct" and "github-release"; this repository must use direct so
  // that plugins can release independently of each other.
  if (plugin.install?.type !== "direct") {
    problems.push(`${label} install.type is ${plugin.install?.type}, want direct`);
    continue;
  }
  const artifacts = plugin.install.artifacts ?? [];
  if (artifacts.length === 0) {
    problems.push(`${label} has no artifacts, which the host rejects`);
  }
  const seen = new Set();
  for (const artifact of artifacts) {
    const platform = `${artifact.goos}/${artifact.goarch}`;
    if (seen.has(platform)) {
      problems.push(`${label} lists ${platform} twice`);
    }
    seen.add(platform);
    if (!/^https:\/\//.test(artifact.url ?? "")) {
      problems.push(`${label} ${platform} url must be https`);
    }
    if (!/^[0-9a-f]{64}$/.test(artifact.sha256 ?? "")) {
      problems.push(`${label} ${platform} sha256 is not 64 lowercase hex characters`);
    }
    if (!artifact.url?.includes(`_${plugin.version}_`)) {
      problems.push(`${label} ${platform} url does not carry version ${plugin.version}`);
    }
  }
}

if (problems.length > 0) {
  for (const problem of problems) {
    console.error(`registry: ${problem}`);
  }
  process.exit(1);
}
console.log(`${registryPath} ok (${(registry.plugins ?? []).length} plugins)`);
