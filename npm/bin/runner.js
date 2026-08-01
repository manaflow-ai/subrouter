#!/usr/bin/env node
const childProcess = require("node:child_process");
const crypto = require("node:crypto");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const packageJSON = require("../../package.json");

function fail(message) {
  console.error(message);
  process.exit(1);
}

function releasePlatform() {
  const platform = {
    darwin: "darwin",
    linux: "linux",
    win32: "windows"
  }[process.platform];
  if (!platform) fail(`Unsupported platform: ${process.platform}`);
  return platform;
}

function releaseArch() {
  if (process.arch === "x64") return "amd64";
  if (process.arch === "arm64") return "arm64";
  fail(`Unsupported architecture: ${process.arch}`);
}

function download(url) {
  const curl = childProcess.spawnSync("curl", ["-fsSL", url], {
    encoding: "buffer",
    maxBuffer: 200 * 1024 * 1024
  });
  if (curl.status !== 0) {
    const stderr = curl.stderr ? curl.stderr.toString("utf8") : "";
    fail(`Failed to download ${url}\n${stderr}`);
  }
  return curl.stdout;
}

function verifyChecksum(binary, assetName, checksumBody) {
  const expected = checksumBody
    .toString("utf8")
    .split(/\r?\n/)
    .map((line) => line.trim().split(/\s+/))
    .find((parts) => parts.length >= 2 && parts[1] === assetName);
  if (!expected) fail(`Missing checksum for ${assetName}`);
  const actual = crypto.createHash("sha256").update(fs.readFileSync(binary)).digest("hex");
  if (actual !== expected[0]) fail(`Checksum mismatch for ${assetName}`);
}

function ensureBinary() {
  if (process.env.SUBROUTER_BIN) return process.env.SUBROUTER_BIN;

  const version = packageJSON.version;
  const platform = releasePlatform();
  const arch = releaseArch();
  const exe = platform === "windows" ? ".exe" : "";
  const assetName = `subrouter_${version}_${platform}_${arch}${exe}`;
  const cacheRoot = process.env.SUBROUTER_INSTALL_DIR || path.join(os.homedir(), ".cache", "subrouter");
  const binary = path.join(cacheRoot, version, assetName);
  if (fs.existsSync(binary)) return binary;

  fs.mkdirSync(path.dirname(binary), { recursive: true });
  const baseURL = process.env.SUBROUTER_DOWNLOAD_BASE || `https://github.com/manaflow-ai/subrouter/releases/download/v${version}`;
  const tmp = `${binary}.${process.pid}.tmp`;
  fs.writeFileSync(tmp, download(`${baseURL}/${assetName}`), { mode: 0o755 });
  try {
    verifyChecksum(tmp, assetName, download(`${baseURL}/SHA256SUMS`));
  } catch (error) {
    fs.rmSync(tmp, { force: true });
    throw error;
  }
  fs.chmodSync(tmp, 0o755);
  fs.renameSync(tmp, binary);
  return binary;
}

function run(commandName) {
  const binary = ensureBinary();
  const result = childProcess.spawnSync(binary, process.argv.slice(2), {
    stdio: "inherit",
    argv0: commandName
  });
  if (result.error) fail(result.error.message);
  process.exit(result.status === null ? 1 : result.status);
}

module.exports = { run };
