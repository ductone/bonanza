#!/usr/bin/env bash
# Build and verify distributable Bonanza CLI bundles.
#
# A bundle is a single versioned archive for one Linux platform holding
#   - the bonanza_bazel client binary built from an exact source revision,
#   - the Starlark built-in trees (bazel_tools, builtins_bzl, builtins_core)
#     that the client must be pointed at through the flag matrix in
#     tools/release/builtins-flags.txt,
#   - a launcher that resolves its own location and injects that matrix,
#   - VERSION / PROVENANCE.json / SHA256SUMS describing the archive itself.
#
# The revision is the bundle's immutable identity: the same revision and
# platform always produce the same archive path, the same top-level
# extraction directory and the same recorded contents. Nothing in the
# archive is derived from the build host, so a rebuild in a different
# checkout is byte-identical for a given revision, platform and toolchain.
#
# This tool never builds anything. The caller (CI) produces the client
# binary for a platform and hands it over with `assemble`.
#
# Subcommands:
#   assemble --binary PATH --platform linux_amd64|linux_arm64 \
#            --revision SHA1 --source-root DIR --out DIR \
#            [--source-date-epoch N] [--repo-slug OWNER/REPO] [--release-tag TAG]
#   verify ARCHIVE [--expect-revision SHA1] [--expect-platform PLATFORM]
#            [--checksum-file FILE] [--no-exec | --cross-exec]
#   attest ARCHIVE [--repo-slug OWNER/REPO] [--signer-workflow WORKFLOW]
#            [--allow-self-hosted]
#   tag --revision SHA1
#
# verify runs a client start-up probe by default, which requires the host to
# be able to execute the archive's binary. When the host architecture differs
# from --platform it refuses instead of failing with an exec error: pass
# --no-exec to verify structure only, or --cross-exec when the platform's
# binaries really can run here (for example under qemu binfmt). The summary
# always states whether the client was executed.
#
# assemble requires a clean checkout whose HEAD is exactly --revision, so
# an artifact can never mix a reviewed revision with unreviewed local
# changes. verify is the consumer-side offline check: it re-verifies the
# archive checksum, every file digest inside it, the recorded
# revision/platform, the ELF architecture of the client, the presence of
# all three Starlark trees, the absence of any credential material, and
# (unless --no-exec) that the launcher can start the bundled client.
#
# attest is the authenticity check, and it is deliberately separate from
# verify: a checksum file downloaded from the same release URL proves
# integrity, not who built the archive. attest runs `gh attestation verify`
# against the identity recorded inside the archive -- the exact workflow
# that signed it, the release tag, and the source revision -- instead of
# accepting any workflow in the repository.

set -euo pipefail
set -o pipefail

readonly REPO_ROOT="$(cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly BUNDLE_NAME="bonanza-cli"
readonly BUNDLE_FORMAT=1
readonly LAUNCHER_NAME="bonanza"
readonly CLIENT_NAME="bonanza_bazel"
readonly STARLARK_TREES=(bazel_tools builtins_bzl builtins_core)
readonly DEFAULT_REPO_SLUG="ductone/bonanza"
# The workflow that signs release attestations. Recorded in every bundle so
# a consumer pins signer identity from the artifact itself rather than from
# documentation, and so `attest` cannot drift from the workflow that exists.
readonly SIGNER_WORKFLOW_PATH=".github/workflows/release-cli.yaml"
readonly SIGNER_WORKFLOW_RE='^[^/]+/[^/]+/\.github/workflows/release-cli\.(yaml|yml)$'

# Scratch directory for a single assemble/verify invocation. Declared at
# top level so the EXIT trap can clean it up after the subcommand returns.
WORK_DIR=""
cleanup() {
  if [[ -n "$WORK_DIR" && -d "$WORK_DIR" ]]; then
    rm -rf "$WORK_DIR"
  fi
}
trap cleanup EXIT

die() {
  printf 'bonanza-cli-bundle: %s\n' "$*" >&2
  exit 1
}

usage() {
  awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"
}

json_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  s="${s//$'\n'/\\n}"
  s="${s//$'\r'/\\r}"
  s="${s//$'\t'/\\t}"
  printf '%s' "$s"
}

require_tools() {
  local tool
  for tool in tar gzip sha256sum od find sort; do
    command -v "$tool" >/dev/null 2>&1 || die "required tool not found: $tool"
  done
}

validate_revision() {
  [[ "$1" =~ ^[0-9a-f]{40}$ ]] ||
    die "revision must be a full 40-character lowercase git SHA-1, got: $1"
}

validate_platform() {
  case "$1" in
    linux_amd64 | linux_arm64) ;;
    *) die "unsupported platform: $1 (expected linux_amd64 or linux_arm64)" ;;
  esac
}

release_tag_for() {
  validate_revision "$1"
  printf 'cli-%s\n' "${1:0:16}"
}

# The Linux platform label of the machine we are running on, used to decide
# whether the archive's client can be executed here.
host_platform() {
  case "$(uname -m)" in
    x86_64 | amd64) printf 'linux_amd64\n' ;;
    aarch64 | arm64) printf 'linux_arm64\n' ;;
    *) printf 'unknown\n' ;;
  esac
}

bundle_root_for() {
  printf '%s-%s-%s\n' "$BUNDLE_NAME" "$1" "$2"
}

# Asserts that a binary is a 64-bit little-endian ELF for the requested
# Linux platform. A mismatched cross-compilation, or a byte-swapped ELF whose
# machine field happens to look valid in the other byte order, is the failure
# this catches.
check_client_arch() {
  local binary="$1" platform="$2" magic elf_data machine
  magic="$(od -An -tx1 -N4 "$binary" | tr -d ' \n')"
  [[ "$magic" == "7f454c46" ]] || die "client is not an ELF file: $binary"
  elf_data="$(od -An -tx1 -j5 -N1 "$binary" | tr -d ' \n')"
  [[ "$elf_data" == "01" ]] ||
    die "client ELF is not little-endian (EI_DATA=0x$elf_data): $binary"
  machine="$(od -An -tx1 -j18 -N2 "$binary" | tr -d ' \n')"
  case "$platform:$machine" in
    linux_amd64:3e00) ;;
    linux_arm64:b700) ;;
    *) die "client ELF machine 0x$machine does not match platform $platform" ;;
  esac
}

# Computes SHA256SUMS for every regular file below the staging root,
# using bundle-relative paths, in sorted order.
write_checksums() {
  local staging_root="$1"
  (
    cd "$staging_root"
    find . -type f ! -name SHA256SUMS -printf '%P\n' |
      LC_ALL=C sort |
      xargs -r sha256sum >SHA256SUMS
  )
}

write_readme() {
  local path="$1" revision="$2" platform="$3" tag="$4" slug="$5" root="$6" source_root="$7"
  local archive flags template
  archive="$(archive_name "$revision" "$platform")"
  flags="$(sed "s|@BUNDLE_ROOT@|<extraction-root>|g" "$source_root/tools/release/builtins-flags.txt")"

  # The template is a quoted heredoc, so every Markdown backtick and
  # ${...} sequence in it is inert; bundle-specific values are substituted
  # by parameter expansion afterwards, which treats them literally.
  template="$(cat <<'TEMPLATE'
# Bonanza CLI bundle

Immutable version: @@REVISION@@
Platform: @@PLATFORM@@
Extraction root: `@@ROOT@@/`

This archive is a complete, self-contained Bonanza client. It contains the
`@@CLIENT@@` binary plus the Starlark built-in trees the client must be
pointed at, and a launcher that wires them together. It contains no cluster
endpoints, certificates, keys or encryption settings: those stay with the
operator and are supplied per environment.

## Layout

```text
@@ROOT@@/
  bin/bonanza                        launcher: entrypoint, use this
  bin/bonanza_bazel                  the client binary, no builtins applied
  share/bonanza/client-flags.txt     the builtins flag matrix, verbatim
  share/bonanza/starlark/bazel_tools/
  share/bonanza/starlark/builtins_bzl/
  share/bonanza/starlark/builtins_core/
  VERSION                            the source revision, bare
  PROVENANCE.json                    revision, platform, flags, file identity
  SHA256SUMS                         digest of every file listed above
  LICENSE, AUTHORS
```

## Consumer interface

Use `bin/@@LAUNCHER@@` as the client, exactly like Bazel:

```sh
/tmp/@@ROOT@@/bin/@@LAUNCHER@@ build //foo:bar
```

The launcher resolves its own location, so the directory may be relocated
after extraction. It injects these flags, derived from
`share/bonanza/client-flags.txt`, ahead of any caller arguments:

```text
@@FLAGS@@
```

If the caller supplies any `--builtins_module`, `--override_module`,
`--rule_implementation_wrapper_identifier` or
`--subrule_implementation_wrapper_identifier` argument of its own, the
launcher passes the invocation through untouched and says so on stderr, so
the bundle's builtins tree and a caller-provided one never mix.
`BONANZA_CLI_NO_BUILTINS=1` suppresses the injection explicitly.
`BONANZA_CLI_PRINT_ARGV=1` prints the argv the launcher would execute and
exits without launching, for diagnosing exactly which flags are applied.

Callers are still responsible for module resolution (`--registry`,
`--override_module` for ordinary modules, `--vendor_dir`), for the
environment's rc/identity values, and for all executor, cache, scheduling
and credential flags.

A workspace, home or system rc file that sets the same four builtins flags
would be applied on top of the launcher's arguments. A build environment
that previously bootstrapped the client from source must therefore drop its
own `--override_module` entries for `bazel_tools`, `builtins_bzl` and
`builtins_core`, and its `--builtins_module` entries, when it switches to a
bundle; otherwise two competing builtins trees are configured.

## Release pin contract

Tag: `@@TAG@@`
Archive: `https://github.com/@@SLUG@@/releases/download/@@TAG@@/@@ARCHIVE@@`
Checksums: `https://github.com/@@SLUG@@/releases/download/@@TAG@@/SHA256SUMS`

The tag names the revision (`@@TAG@@` is the first 16 characters of
`@@REVISION@@`); the archive name carries the full revision. A release is
published only from a tag that already exists on the reviewed revision, and
is never overwritten: re-running the release workflow for a published
version fails instead of replacing it.

## Authenticity

`SHA256SUMS` and `<archive>.sha256` are fetched from the same release URL as
the archive: they prove the bytes did not change in transit, not who produced
them. Authenticity comes from the signed build attestation, pinned to the
exact signer workflow, release tag and source revision that are recorded in
`PROVENANCE.json`:

```sh
gh attestation verify @@ARCHIVE@@ \
  --repo @@SLUG@@ \
  --signer-workflow @@SIGNER@@ \
  --source-ref refs/tags/@@TAG@@ \
  --source-digest @@REVISION@@ \
  --deny-self-hosted-runners
```

`--repo` on its own would accept an attestation signed by any workflow in the
repository, so the signer workflow, tag and revision are pinned together.
`tools/release/bonanza-cli-bundle.sh attest` runs exactly this command, taking
every value from the archive itself rather than from documentation:

```sh
tools/release/bonanza-cli-bundle.sh attest @@ARCHIVE@@
```

Then check the archive offline (`bonanza-cli-bundle.sh` comes from a Bonanza
checkout of the same revision):

```sh
sha256sum -c @@ARCHIVE@@.sha256
tools/release/bonanza-cli-bundle.sh verify @@ARCHIVE@@ --expect-revision @@REVISION@@ --expect-platform @@PLATFORM@@
```

`verify` needs no network and no attestation. Besides the digests, revision,
platform and signer identity it checks the ELF architecture and that all three
Starlark trees shipped, and it rejects anything that is not a regular file or
directory. A bundle therefore contains no symlinks whose targets could point
outside the extraction root.

The last `verify` step is a client start-up probe, so it needs a host that can
execute this archive's binary. On a different architecture `verify` refuses
rather than failing with an exec error: pass `--no-exec` to check structure
only, or `--cross-exec` when @@PLATFORM@@ binaries can run here (for example
under qemu binfmt). The summary line `client execution:` always states which of
the two happened, so a skipped probe is never mistaken for a passing one.
Release CI executes the x86-64 archive natively and the arm64 archive under
emulation; native arm64 execution on real hardware is an operator check, not a
claim made by the release.
TEMPLATE
)"

  template="${template//@@REVISION@@/$revision}"
  template="${template//@@PLATFORM@@/$platform}"
  template="${template//@@ROOT@@/$root}"
  template="${template//@@LAUNCHER@@/$LAUNCHER_NAME}"
  template="${template//@@CLIENT@@/$CLIENT_NAME}"
  template="${template//@@ARCHIVE@@/$archive}"
  template="${template//@@TAG@@/$tag}"
  template="${template//@@SLUG@@/$slug}"
  template="${template//@@SIGNER@@/$slug/$SIGNER_WORKFLOW_PATH}"
  template="${template//@@FLAGS@@/$flags}"
  printf '%s\n' "$template" >"$path"
}

write_provenance() {
  local path="$1" revision="$2" platform="$3" tag="$4" root="$5" epoch="$6" binary_hash="$7" source_root="$8"
  local tree flags flag
  {
    printf '{\n'
    printf '  "bundle_format": %d,\n' "$BUNDLE_FORMAT"
    printf '  "bundle_name": "%s",\n' "$(json_escape "$BUNDLE_NAME-$platform")"
    printf '  "bundle_root": "%s",\n' "$(json_escape "$root")"
    printf '  "client_binary": "%s",\n' "$(json_escape "bin/$CLIENT_NAME")"
    printf '  "client_binary_sha256": "%s",\n' "$binary_hash"
    printf '  "client_flags": [\n'
    local first=1
    while IFS= read -r flag; do
      [[ -n "$flag" ]] || continue
      if ((first == 0)); then printf ',\n'; fi
      first=0
      printf '    "%s"' "$(json_escape "$flag")"
    done <"$source_root/tools/release/builtins-flags.txt"
    printf '\n  ],\n'
    printf '  "client_flags_file": "%s",\n' "$(json_escape "share/bonanza/client-flags.txt")"
    printf '  "linux_platform": "%s",\n' "$(json_escape "$platform")"
    printf '  "release_tag": "%s",\n' "$(json_escape "$tag")"
    printf '  "signer_workflow": "%s",\n' "$(json_escape "$slug/$SIGNER_WORKFLOW_PATH")"
    printf '  "source_date_epoch": %d,\n' "$epoch"
    printf '  "source_revision": "%s",\n' "$revision"
    printf '  "starlark_trees": [\n'
    first=1
    for tree in "${STARLARK_TREES[@]}"; do
      if ((first == 0)); then printf ',\n'; fi
      first=0
      printf '    "%s"' "$(json_escape "share/bonanza/starlark/$tree")"
    done
    printf '\n  ],\n'
    printf '  "version": "%s"\n' "$revision"
    printf '}\n'
  } >"$path"
}

archive_name() {
  printf '%s-%s-%s.tar.gz\n' "$BUNDLE_NAME" "$1" "$2"
}

# Asserts that the archive contains only plain files and directories, that
# no member name is absolute or escapes the extraction root, and (after
# extraction) that nothing extracted is a symlink. This must run BEFORE
# extraction: a link member is not covered by SHA256SUMS, so a bundle whose
# safe-bundle contract is "regular files and directories only" has to reject
# link members outright rather than trust the extractor's own hardening.
assert_safe_members() {
  local archive="$1" names types name line type
  # Separate long options, so `-f` can never swallow a following option as
  # the archive name, and fail closed if the archive cannot be listed at all.
  names="$(tar --list --gzip --file "$archive" --quoting-style=escape)" ||
    die "cannot list archive: $archive"
  types="$(tar --list --verbose --gzip --file "$archive" --quoting-style=escape)" ||
    die "cannot list archive: $archive"
  while IFS= read -r name; do
    [[ -n "$name" ]] || continue
    [[ "$name" != /* ]] || die "archive member has an absolute path: $name"
    case "/$name/" in
      */../*) die "archive member escapes the extraction root: $name" ;;
    esac
  done <<<"$names"
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    type="${line:0:1}"
    case "$type" in
      - | d) ;;
      *) die "archive member is not a regular file or directory: ${line#* }" ;;
    esac
  done <<<"$types"
  [[ -n "$names" ]] || die "archive is empty: $archive"
}

# Asserts that a staged or extracted tree carries no symlinks and no hard
# linked files, so every shipped byte is covered by SHA256SUMS.
assert_plain_tree() {
  local root="$1" offender
  offender="$(find "$root" -type l -print -quit)"
  [[ -z "$offender" ]] || die "tree contains a symlink: $offender"
  offender="$(find "$root" -type f -links +1 -print -quit)"
  [[ -z "$offender" ]] || die "tree contains a hard linked file: $offender"
}

cmd_assemble() {
  local binary="" platform="" revision="" source_root="" out="" epoch="" slug="$DEFAULT_REPO_SLUG" tag=""
  while (($# > 0)); do
    case "$1" in
      --binary) binary="$2"; shift 2 ;;
      --platform) platform="$2"; shift 2 ;;
      --revision) revision="$2"; shift 2 ;;
      --source-root) source_root="$2"; shift 2 ;;
      --out) out="$2"; shift 2 ;;
      --source-date-epoch) epoch="$2"; shift 2 ;;
      --repo-slug) slug="$2"; shift 2 ;;
      --release-tag) tag="$2"; shift 2 ;;
      -h | --help) usage; return 0 ;;
      *) die "unknown assemble argument: $1" ;;
    esac
  done

  [[ -n "$binary" ]] || die "assemble requires --binary"
  [[ -n "$platform" ]] || die "assemble requires --platform"
  [[ -n "$revision" ]] || die "assemble requires --revision"
  [[ -n "$out" ]] || die "assemble requires --out"
  source_root="${source_root:-$REPO_ROOT}"
  source_root="$(cd -P -- "$source_root" && pwd)"
  validate_platform "$platform"
  validate_revision "$revision"
  require_tools

  [[ -f "$binary" && ! -L "$binary" ]] ||
    die "--binary must be a regular file: $binary"
  command -v git >/dev/null 2>&1 || die "required tool not found: git"

  local head
  head="$(git -C "$source_root" rev-parse HEAD)"
  [[ "$head" == "$revision" ]] ||
    die "checkout HEAD ($head) is not the requested revision ($revision)"
  [[ -z "$(git -C "$source_root" status --porcelain)" ]] ||
    die "checkout at $source_root has local changes; refusing to package them"

  if [[ -z "$epoch" ]]; then
    epoch="$(git -C "$source_root" show -s --format=%ct "$revision")"
  fi
  [[ "$epoch" =~ ^[0-9]+$ ]] || die "--source-date-epoch must be a Unix timestamp"
  [[ -n "$tag" ]] || tag="$(release_tag_for "$revision")"

  check_client_arch "$binary" "$platform"

  local root staging archive
  root="$(bundle_root_for "$revision" "$platform")"
  archive="$(archive_name "$revision" "$platform")"
  WORK_DIR="$(mktemp -d)"
  staging="$WORK_DIR"

  local top="$staging/$root"
  mkdir -p "$top/bin" "$top/share/bonanza/starlark"
  install -m 0755 "$binary" "$top/bin/$CLIENT_NAME"
  install -m 0755 "$source_root/tools/release/bonanza-cli-launcher.sh" "$top/bin/$LAUNCHER_NAME"
  install -m 0644 "$source_root/tools/release/builtins-flags.txt" "$top/share/bonanza/client-flags.txt"

  local tree
  for tree in "${STARLARK_TREES[@]}"; do
    [[ -d "$source_root/starlark/$tree" ]] ||
      die "missing Starlark tree in checkout: starlark/$tree"
  done
  # A shipped link is not covered by SHA256SUMS and could point outside the
  # extraction root, so a bundle is regular files and directories only.
  assert_plain_tree "$source_root/starlark"
  for tree in "${STARLARK_TREES[@]}"; do
    cp -a "$source_root/starlark/$tree" "$top/share/bonanza/starlark/$tree"
  done

  # Normalize modes and drop anything that must not ship.
  find "$top/share" -type d -exec chmod 0755 {} +
  find "$top/share" -type f -exec chmod 0644 {} +
  find "$top/share" \( -name '*.pem' -o -name '*.key' -o -name '*.crt' -o -name '*.p12' \) \
    -print -quit | grep -q . && die "Starlark trees contain credential files"

  install -m 0644 "$source_root/LICENSE" "$top/LICENSE"
  [[ -f "$source_root/AUTHORS" ]] &&
    install -m 0644 "$source_root/AUTHORS" "$top/AUTHORS"
  printf '%s\n' "$revision" >"$top/VERSION"
  chmod 0644 "$top/VERSION"

  local binary_hash
  binary_hash="$(sha256sum "$top/bin/$CLIENT_NAME" | cut -d' ' -f1)"
  write_provenance "$top/PROVENANCE.json" "$revision" "$platform" "$tag" "$root" "$epoch" "$binary_hash" "$source_root"
  chmod 0644 "$top/PROVENANCE.json"
  write_readme "$top/BUNDLE-README.md" "$revision" "$platform" "$tag" "$slug" "$root" "$source_root"
  chmod 0644 "$top/BUNDLE-README.md"
  write_checksums "$top"
  assert_plain_tree "$top"

  mkdir -p "$out"
  out="$(cd -P -- "$out" && pwd)"
  (cd "$staging" && tar \
    --create \
    --file - \
    --sort=name \
    --format=gnu \
    --owner=0 \
    --group=0 \
    --numeric-owner \
    --no-acls \
    --no-xattrs \
    --mtime="@$epoch" \
    "$root" | gzip -9n >"$out/$archive")
  (cd "$out" && sha256sum "$archive" >"$archive.sha256")

  printf 'assembled %s\n' "$out/$archive"
  printf 'revision %s\n' "$revision"
  printf 'platform %s\n' "$platform"
  printf 'release tag %s\n' "$tag"
  printf 'sha256 %s\n' "$(cut -d' ' -f1 <"$out/$archive.sha256")"
}

json_string_field() {
  sed -n "s/^ *\"$1\": *\"\([^\"]*\)\".*/\1/p" "$2"
}

json_number_field() {
  sed -n "s/^ *\"$1\": *\([0-9]*\).*/\1/p" "$2"
}

# Rejects credential material and cluster wiring. Three separate classes, so a
# compiled client stays acceptable while real secrets never do:
#   1. secret-named files anywhere (including binaries and links);
#   2. PEM/DER key material in ANY file, binaries included -- the client's flag
#      names cannot produce a `-----BEGIN ... PRIVATE KEY-----` marker, so a
#      hit here means bytes were actually embedded;
#   3. credential-wiring identifiers in TEXT assets only. The client binary
#      legitimately contains `--remote_executor_client_private_key` and friends
#      as flag-name string literals, so scanning it for those identifiers
#      reports a false positive; grep's `-I` skips binary files, which is
#      exactly the distinction wanted here.
assert_no_secrets() {
  local root="$1" hits
  hits="$(find "$root" \( -name '*.pem' -o -name '*.key' -o -name '*.crt' \
    -o -name '*.p12' -o -name '*.p8' -o -name '*.jks' -o -name '*.keystore' \) -print -quit)"
  [[ -z "$hits" ]] || die "bundle contains a secret-named file: $hits"
  hits="$(grep -rl --binary-files=text -E -- '-----BEGIN [A-Z0-9 ]+-----' "$root" || true)"
  [[ -z "$hits" ]] || die "bundle contains embedded key material: $hits"
  hits="$(grep -rlI -E -- 'client_private_key|client_certificate_chain|remote_encryption_key|remote_executor_client|remote_executor=' "$root" || true)"
  [[ -z "$hits" ]] || die "bundle text assets reference credential flags: $hits"
}

cmd_verify() {
  local archive="" expect_revision="" expect_platform="" checksum_file="" exec_mode="auto"
  local -a positional=()
  while (($# > 0)); do
    case "$1" in
      --expect-revision) expect_revision="$2"; shift 2 ;;
      --expect-platform) expect_platform="$2"; shift 2 ;;
      --checksum-file) checksum_file="$2"; shift 2 ;;
      --no-exec) exec_mode="skip"; shift ;;
      --cross-exec) exec_mode="cross"; shift ;;
      -h | --help) usage; return 0 ;;
      -*) die "unknown verify argument: $1" ;;
      *) positional+=("$1"); shift ;;
    esac
  done
  ((${#positional[@]} == 1)) || die "verify requires exactly one archive path"
  archive="${positional[0]}"
  [[ -f "$archive" ]] || die "archive not found: $archive"
  require_tools

  local archive_dir archive_base
  archive_dir="$(cd -P -- "$(dirname -- "$archive")" && pwd)"
  archive_base="$(basename -- "$archive")"
  archive="$archive_dir/$archive_base"
  if [[ -n "$expect_revision" ]]; then validate_revision "$expect_revision"; fi
  if [[ -n "$expect_platform" ]]; then validate_platform "$expect_platform"; fi

  if [[ -z "$checksum_file" && -f "$archive.sha256" ]]; then
    checksum_file="$archive.sha256"
  fi
  if [[ -n "$checksum_file" ]]; then
    [[ -f "$checksum_file" ]] || die "checksum file not found: $checksum_file"
    (cd "$archive_dir" && sha256sum -c --status "$checksum_file") ||
      die "archive digest does not match $checksum_file"
  fi

  local tmp
  tmp="$(mktemp -d)"
  WORK_DIR="$tmp"
  assert_safe_members "$archive"
  tar -xzf "$archive" -C "$tmp"

  local entries=()
  mapfile -t entries < <(find "$tmp" -mindepth 1 -maxdepth 1 -printf '%f\n' | LC_ALL=C sort)
  ((${#entries[@]} == 1)) || die "archive must contain a single top-level directory"
  local root="${entries[0]}"
  local root_dir="$tmp/$root"
  assert_plain_tree "$tmp"

  local version
  version="$(cat "$root_dir/VERSION")"
  validate_revision "$version"
  local manifest
  manifest="$(json_string_field version "$root_dir/PROVENANCE.json")"
  [[ "$manifest" == "$version" ]] ||
    die "PROVENANCE.json version ($manifest) disagrees with VERSION ($version)"
  local platform
  platform="$(json_string_field linux_platform "$root_dir/PROVENANCE.json")"
  validate_platform "$platform"
  local epoch
  epoch="$(json_number_field source_date_epoch "$root_dir/PROVENANCE.json")"
  [[ -n "$epoch" ]] || die "PROVENANCE.json is missing source_date_epoch"

  if [[ -n "$expect_revision" && "$version" != "$expect_revision" ]]; then
    die "bundle revision $version does not match expected $expect_revision"
  fi
  if [[ -n "$expect_platform" && "$platform" != "$expect_platform" ]]; then
    die "bundle platform $platform does not match expected $expect_platform"
  fi
  local recorded_tag recorded_signer
  recorded_tag="$(json_string_field release_tag "$root_dir/PROVENANCE.json")"
  [[ "$recorded_tag" == "$(release_tag_for "$version")" ]] ||
    die "recorded release tag $recorded_tag does not match revision $version"
  recorded_signer="$(json_string_field signer_workflow "$root_dir/PROVENANCE.json")"
  [[ "$recorded_signer" =~ $SIGNER_WORKFLOW_RE ]] ||
    die "PROVENANCE.json records an unexpected signer workflow: ${recorded_signer:-<missing>}"
  [[ "$root" == "$(bundle_root_for "$version" "$platform")" ]] ||
    die "extraction root $root does not match revision and platform"
  [[ "$archive_base" == "$(archive_name "$version" "$platform")" ]] ||
    die "archive name $archive_base does not match revision and platform"

  (cd "$root_dir" && sha256sum -c --status SHA256SUMS) ||
    die "bundle contents do not match SHA256SUMS"

  local binary_hash
  binary_hash="$(json_string_field client_binary_sha256 "$root_dir/PROVENANCE.json")"
  [[ "$binary_hash" == "$(sha256sum "$root_dir/bin/$CLIENT_NAME" | cut -d' ' -f1)" ]] ||
    die "PROVENANCE.json client_binary_sha256 does not match bin/$CLIENT_NAME"

  [[ -x "$root_dir/bin/$LAUNCHER_NAME" ]] || die "bin/$LAUNCHER_NAME is missing or not executable"
  [[ -x "$root_dir/bin/$CLIENT_NAME" ]] || die "bin/$CLIENT_NAME is missing or not executable"

  local flags_file="$root_dir/share/bonanza/client-flags.txt"
  [[ -f "$flags_file" ]] || die "share/bonanza/client-flags.txt is missing"
  local required flag
  for required in builtins_module override_module \
    rule_implementation_wrapper_identifier subrule_implementation_wrapper_identifier; do
    grep -q -- "--$required" "$flags_file" ||
      die "flag matrix does not set --$required"
  done
  while IFS= read -r flag; do
    [[ -n "$flag" ]] || continue
    grep -qF -- "$flag" "$root_dir/PROVENANCE.json" ||
      die "PROVENANCE.json does not record flag matrix entry: $flag"
    case "$flag" in
      --override_module=*=*)
        local module_dir="${flag#--override_module=}"
        module_dir="${module_dir#*=}"
        module_dir="${module_dir//@BUNDLE_ROOT@/$root_dir}"
        [[ -f "$module_dir/MODULE.bazel" ]] ||
          die "override_module target is not a module: $module_dir"
        ;;
    esac
  done <"$flags_file"

  local tree bzl_count
  for tree in "${STARLARK_TREES[@]}"; do
    module_dir="$root_dir/share/bonanza/starlark/$tree"
    [[ -f "$module_dir/MODULE.bazel" ]] || die "missing module for Starlark tree $tree"
    bzl_count="$(find "$module_dir" -type f -name '*.bzl' | wc -l)"
    ((bzl_count > 0)) || die "Starlark tree $tree contains no .bzl files"
  done

  assert_no_secrets "$root_dir"
  check_client_arch "$root_dir/bin/$CLIENT_NAME" "$platform"

  # The start-up probe needs the host to execute the archive's client. Say so
  # explicitly rather than letting a cross-architecture run fail with an
  # inscrutable exec error, and never let a skipped probe look like a pass.
  local host=""
  case "$exec_mode" in
    skip) exec_note="skipped (--no-exec)" ;;
    cross) exec_note="executed (--cross-exec)" ;;
    auto)
      host="$(host_platform)"
      if [[ "$host" == "$platform" ]]; then
        exec_note="executed"
      else
        die "refusing to execute the $platform client on a $host host: pass --no-exec to verify structure only, or --cross-exec when $platform binaries can run here (e.g. under qemu binfmt)"
      fi
      ;;
  esac

  if [[ "$exec_note" == executed* ]]; then
    local probe_home="$tmp/probe-home"
    mkdir -p "$probe_home"
    HOME="$probe_home" "$root_dir/bin/$LAUNCHER_NAME" \
      --home_rc=0 --system_rc=0 --workspace_rc=0 version >/dev/null ||
      die "bundled launcher could not run the bundled client"
  fi

  printf 'verified %s\n' "$archive"
  printf 'revision %s\n' "$version"
  printf 'platform %s\n' "$platform"
  printf 'client execution: %s\n' "$exec_note"
  printf 'sha256 %s\n' "$(sha256sum "$archive" | cut -d' ' -f1)"
}

# Authenticity check. A checksum file fetched from the same release URL only
# establishes that the bytes did not change in transit; it says nothing about
# who produced them. This pins the exact signer workflow, the release tag and
# the source revision recorded inside the archive, so an attestation signed by
# any other workflow in the repository does not satisfy it.
cmd_attest() {
  local archive="" slug="$DEFAULT_REPO_SLUG" signer="" deny_self_hosted=1
  local -a positional=()
  while (($# > 0)); do
    case "$1" in
      --repo-slug) slug="$2"; shift 2 ;;
      --signer-workflow) signer="$2"; shift 2 ;;
      --allow-self-hosted) deny_self_hosted=0; shift ;;
      -h | --help) usage; return 0 ;;
      -*) die "unknown attest argument: $1" ;;
      *) positional+=("$1"); shift ;;
    esac
  done
  ((${#positional[@]} == 1)) || die "attest requires exactly one archive path"
  archive="${positional[0]}"
  [[ -f "$archive" ]] || die "archive not found: $archive"
  require_tools
  command -v gh >/dev/null 2>&1 ||
    die "required tool not found: gh (needed to fetch and verify attestations)"

  WORK_DIR="$(mktemp -d)"
  assert_safe_members "$archive"
  tar -xzf "$archive" -C "$WORK_DIR"
  local entries=()
  mapfile -t entries < <(find "$WORK_DIR" -mindepth 1 -maxdepth 1 -printf '%f\n' | LC_ALL=C sort)
  ((${#entries[@]} == 1)) || die "archive must contain a single top-level directory"
  local manifest="$WORK_DIR/${entries[0]}/PROVENANCE.json"
  [[ -f "$manifest" ]] || die "PROVENANCE.json is missing from the archive"

  local revision tag recorded_signer
  revision="$(json_string_field version "$manifest")"
  validate_revision "$revision"
  tag="$(json_string_field release_tag "$manifest")"
  [[ "$tag" == "$(release_tag_for "$revision")" ]] ||
    die "recorded release tag $tag does not match revision $revision"
  recorded_signer="$(json_string_field signer_workflow "$manifest")"
  if [[ -z "$signer" ]]; then
    signer="$recorded_signer"
  fi
  [[ "$signer" =~ $SIGNER_WORKFLOW_RE ]] ||
    die "refusing to verify against an unexpected signer workflow: ${signer:-<empty>}"
  if [[ "$recorded_signer" != "$signer" ]]; then
    printf 'attest: overriding recorded signer %s with %s\n' "$recorded_signer" "$signer" >&2
  fi

  local -a command_argv=(
    gh attestation verify "$archive"
    --repo "$slug"
    --signer-workflow "$signer"
    --source-ref "refs/tags/$tag"
    --source-digest "$revision"
  )
  if ((deny_self_hosted == 1)); then
    command_argv+=(--deny-self-hosted-runners)
  fi
  printf 'attest: %s\n' "${command_argv[*]}" >&2
  "${command_argv[@]}"
}

cmd_tag() {
  local revision=""
  while (($# > 0)); do
    case "$1" in
      --revision) revision="$2"; shift 2 ;;
      -h | --help) usage; return 0 ;;
      *) die "unknown tag argument: $1" ;;
    esac
  done
  [[ -n "$revision" ]] || die "tag requires --revision"
  release_tag_for "$revision"
}

main() {
  (($# > 0)) || { usage; exit 1; }
  local subcommand="$1"
  shift
  case "$subcommand" in
    assemble) cmd_assemble "$@" ;;
    verify) cmd_verify "$@" ;;
    attest) cmd_attest "$@" ;;
    tag) cmd_tag "$@" ;;
    -h | --help | help) usage ;;
    *) die "unknown subcommand: $subcommand" ;;
  esac
}

main "$@"
