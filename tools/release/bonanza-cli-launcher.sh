#!/usr/bin/env bash
# Bonanza CLI launcher shipped inside a versioned CLI bundle.
#
# The bundle carries a platform-specific bonanza_bazel client together
# with the Starlark built-in trees (bazel_tools, builtins_bzl,
# builtins_core) that the client must be told about on every
# invocation. This launcher is the bundle's public entrypoint: it
# resolves its own location so the archive can be extracted anywhere,
# applies the versioned builtins flag matrix recorded in
# share/bonanza/client-flags.txt, and then execs the real client.
#
# It deliberately sets no cluster, executor, cache, certificate or
# encryption flags: credentials and endpoints stay with the operator and
# never live in a released artifact.
#
# Environment variables:
#   BONANZA_CLI_NO_BUILTINS=1   exec the client without the bundle's
#                               builtins flags (caller owns them).
#   BONANZA_CLI_PRINT_ARGV=1    print the argv the launcher would exec,
#                               one element per line prefixed with
#                               "argv: ", then exit 0 without launching.
#
# Any of --builtins_module, --override_module,
# --rule_implementation_wrapper_identifier or
# --subrule_implementation_wrapper_identifier in the caller's arguments
# makes the launcher pass the invocation through untouched, so a caller
# that already selects its own builtins tree cannot silently end up with
# two competing ones.

set -euo pipefail

resolve_self() {
  local src="${BASH_SOURCE[0]}" dir
  while [[ -L "$src" ]]; do
    dir="$(cd -P -- "$(dirname -- "$src")" && pwd)"
    src="$(readlink -- "$src")"
    [[ "$src" == /* ]] || src="$dir/$src"
  done
  cd -P -- "$(dirname -- "$src")" && pwd
}

die() {
  printf 'bonanza: %s\n' "$*" >&2
  exit 1
}

self_dir="$(resolve_self)"
bundle_root="$(cd -P -- "$self_dir/.." && pwd)"
real_client="$self_dir/bonanza_bazel"
flags_file="$bundle_root/share/bonanza/client-flags.txt"

run_or_print() {
  if [[ -n "${BONANZA_CLI_PRINT_ARGV:-}" ]]; then
    local arg
    for arg in "$@"; do
      printf 'argv: %s\n' "$arg"
    done
    exit 0
  fi
  exec "$@"
}

[[ -x "$real_client" ]] ||
  die "bundled client is missing or not executable: $real_client"

if [[ "${BONANZA_CLI_NO_BUILTINS:-0}" == "1" ]]; then
  run_or_print "$real_client" "$@"
fi

# Startup flags must stay ahead of the command name; everything from the
# command name onwards is passed through unchanged. Only the startup
# flags the client itself defines are recognized here.
declare -a startup=() tail=()
index=1
while ((index <= $#)); do
  arg="${!index}"
  case "$arg" in
    --bazelrc)
      startup+=("--bazelrc")
      index=$((index + 1))
      ((index <= $#)) || die "--bazelrc expects a value"
      startup+=("${!index}")
      ;;
    --bazelrc=* | --home_rc* | --nohome_rc* | --system_rc* | --nosystem_rc* \
    | --workspace_rc* | --noworkspace_rc* | --ignore_all_rc_files* | --noignore_all_rc_files*)
      startup+=("$arg")
      ;;
    *)
      tail=("${@:index}")
      break
      ;;
  esac
  index=$((index + 1))
done

if (( ${#tail[@]} == 0 )); then
  run_or_print "$real_client" ${startup[@]+"${startup[@]}"}
fi

for arg in "${tail[@]}"; do
  case "$arg" in
    --builtins_module | --builtins_module=* | --override_module | --override_module=* \
    | --rule_implementation_wrapper_identifier | --rule_implementation_wrapper_identifier=* \
    | --subrule_implementation_wrapper_identifier | --subrule_implementation_wrapper_identifier=*)
      printf 'bonanza: caller supplied %s; not injecting the bundle builtins flags\n' "$arg" >&2
      run_or_print "$real_client" ${startup[@]+"${startup[@]}"} "${tail[@]}"
      ;;
  esac
done

[[ -r "$flags_file" ]] ||
  die "bundle is incomplete: $flags_file is missing"

declare -a bundle_flags=()
while IFS= read -r line; do
  [[ -n "$line" ]] || continue
  bundle_flags+=("${line//@BUNDLE_ROOT@/$bundle_root}")
done <"$flags_file"
(( ${#bundle_flags[@]} > 0 )) ||
  die "bundle is incomplete: $flags_file lists no flags"

command_name="${tail[0]}"
run_or_print "$real_client" ${startup[@]+"${startup[@]}"} \
  "$command_name" "${bundle_flags[@]}" ${tail[@]+"${tail[@]:1}"}
