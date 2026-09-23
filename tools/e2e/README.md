# End-to-end smoke test

`run.sh` builds the demo cluster and `bonanza_bazel` from the current
checkout, launches the cluster with isolated state in a temporary
directory, and builds `testproject/` against it:

```sh
tools/e2e/run.sh
```

The test project is self-verifying: its `:verify` genrule runs on the
worker and greps the artifacts produced by the other targets, so a
successful build proves that analysis, remote action execution (via the
worker's virtual file system and bb_runner), and the resulting artifact
contents are all correct. After the build, the script checks the output
files that `bonanza_bazel` materialized below `bonanza-out/` in the test
project, covering artifact output as well. Among other things it exercises
`attr.int_list()`, `native.repo_name()`, `native.repository_name()`,
`native.module_name()`, `native.module_version()`,
`native.package_name()`, and `Label.workspace_name`. It also loads
rules_python's flags.bzl and calls a method on one of its enum-like
structs (whose method closures capture the struct itself), covering
the encoding of recursively defined Starlark values. The build is run
twice; the second invocation should complete in seconds, served from
the evaluation cache.

Notes:

- `bonanza_worker` exposes its build directory through a virtual file
  system, so the host has to permit mounting one. On Linux that means
  `/dev/fuse` must be present and openable; containers that do not grant
  access to it fail with `fusermount3: failed to open /dev/fuse:
  Operation not permitted` and the worker exits. Analysis-only targets
  (source files, `ctx.actions.symlink()` outputs) still build without a
  worker, but anything that runs a command does not.
- The demo deployment binds fixed TCP diagnostics ports (9980-9984), so
  only one cluster can run on a host at a time.
- Module dependencies of the test project are provided as local
  overrides cloned from git rather than downloaded as release archives,
  so the test also works in sandboxes whose egress policy blocks GitHub
  release downloads (such as Claude Code on the web).
- `rules_rust` is pinned to 0.62.0 and patched with
  `patches/rules_rust-extension-name.diff` (borrowed from bb-storage's
  bonanza branch), as newer versions declare module extensions whose
  names Bonanza cannot distinguish yet.
- Set `E2E_KEEP_CLUSTER=1` to leave the cluster running after the test,
  e.g. to inspect it with bonanza_browser at http://localhost:9982/.
- Set `E2E_RUN_DIR` to choose the state directory (default: a fresh
  directory under /tmp).
- `testproject/` is listed in `.bazelignore`, as its `platform()`
  targets use Bonanza-specific attributes that regular Bazel does not
  understand.
