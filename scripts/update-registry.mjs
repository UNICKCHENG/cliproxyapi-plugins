#!/usr/bin/env node
// Rewrites one plugin entry in registry.json from a directory of release archives.
//
// CLIProxyAPI installs plugins from this monorepo with the "direct" install type, which
// pins every artifact URL and sha256 in the registry itself. That is what lets each plugin
// in the repository release on its own tag: the "github-release" install type resolves the
// repository's single latest release instead, so one plugin's release would hide the others.
//
// Usage:
//   node scripts/update-registry.mjs --plugin auth-cursor --version 0.1.0 \
//     --tag auth-cursor-v0.1.0 --assets ./release --repository https://github.com/owner/repo

import { createHash } from "node:crypto";
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

function parseArgs(argv) {
  const args = {};
  for (let index = 0; index < argv.length; index += 2) {
    const key = argv[index];
    if (!key.startsWith("--")) {
      throw new Error(`unexpected argument: ${key}`);
    }
    const value = argv[index + 1];
    if (value === undefined) {
      throw new Error(`missing value for ${key}`);
    }
    args[key.slice(2)] = value;
  }
  for (const required of ["plugin", "version", "tag", "assets", "repository"]) {
    if (!args[required]) {
      throw new Error(`missing required argument --${required}`);
    }
  }
  return args;
}

// Archive names are produced by the release workflow as
// <id>_<version>_<goos>_<goarch>.zip, which is also the layout the host expects.
function parseArchiveName(name, pluginId, version) {
  const prefix = `${pluginId}_${version}_`;
  if (!name.startsWith(prefix) || !name.endsWith(".zip")) {
    return null;
  }
  const platform = name.slice(prefix.length, -".zip".length);
  const separator = platform.indexOf("_");
  if (separator <= 0) {
    return null;
  }
  return { goos: platform.slice(0, separator), goarch: platform.slice(separator + 1) };
}

function collectArtifacts(args) {
  const artifacts = [];
  for (const name of readdirSync(args.assets).sort()) {
    const platform = parseArchiveName(name, args.plugin, args.version);
    if (!platform) {
      continue;
    }
    const data = readFileSync(join(args.assets, name));
    artifacts.push({
      goos: platform.goos,
      goarch: platform.goarch,
      url: `${args.repository}/releases/download/${args.tag}/${name}`,
      sha256: createHash("sha256").update(data).digest("hex"),
      size: data.length,
    });
  }
  if (artifacts.length === 0) {
    throw new Error(`no ${args.plugin} archives for version ${args.version} in ${args.assets}`);
  }
  return artifacts;
}

const args = parseArgs(process.argv.slice(2));
const registryPath = args.registry ?? "registry.json";
const registry = JSON.parse(readFileSync(registryPath, "utf8"));
const metadata = JSON.parse(readFileSync(join(args.plugin, "plugin.json"), "utf8"));

const entry = {
  id: metadata.id,
  name: metadata.name,
  description: metadata.description,
  author: metadata.author,
  version: args.version,
  repository: args.repository,
  homepage: `${args.repository}/tree/main/${args.plugin}`,
  license: metadata.license,
  tags: metadata.tags ?? [],
  install: {
    type: "direct",
    artifacts: collectArtifacts(args),
  },
};

registry.schema_version = 2;
registry.plugins = Array.isArray(registry.plugins) ? registry.plugins : [];
const existing = registry.plugins.findIndex((plugin) => plugin.id === metadata.id);
if (existing >= 0) {
  registry.plugins[existing] = entry;
} else {
  registry.plugins.push(entry);
}
registry.plugins.sort((left, right) => left.id.localeCompare(right.id));

writeFileSync(registryPath, `${JSON.stringify(registry, null, 2)}\n`);
console.log(`updated ${metadata.id} ${args.version} with ${entry.install.artifacts.length} artifacts`);
