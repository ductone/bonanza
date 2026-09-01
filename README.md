# Bonanza: an experimental remote build system

Bonanza is an experimental build system that takes the remote execution
model introduced by Bazel to the extreme. Whereas Bazel only uses it to
run build actions (compilation actions, tests) remotely, Bonanza uses it
for **everything**. This means that on your local system you may have a
command line utility that does little more than upload (local changes
to) your source tree to a cluster, followed by issuing a request to kick
off a build there, and report any progress updates received from the
cluster. By using this model we attempt to achieve the following:

- **Improved decoupling from the local system.**

  Remote execution already allows Bazel to run actions on platforms that
  differ from what is used locally. For example, Bazel running on a Mac
  may schedule build actions running on a Linux system. However, Bazel
  is unable to do this for things like repository rules. This means that
  it's not always possible to make Bazel behave as if it's truly running
  on a different system. You see that people sometimes solve this by
  running Bazel inside of a Docker container, or are unable to perform
  certain actions locally, requiring them to do "CI driven development".
  This shouldn't be necessary.

- **Better performance under high network latency.**

  Bazel's remote execution protocol is inherently latency sensitive, due
  to the fact that an action can only be looked up or executed after its
  full set of input files is known. As actions may depend on each other,
  this leads to unnecessary delays when latency between Bazel and the
  remote execution cluster is high. By running the build remotely it is
  easier to run it on systems closer to storage and workers, thereby
  giving reasonable performance both from CI, at the office, and from
  home.

- **Reduction in local disk space usage.**

  For certain projects you see that Bazel's disk space usage is
  excessive. Even though it's possible to reduce the size of
  `bazel-out/` using flags like `--remote_download_minimal` and
  `--nobuild_runfile_links`, there is no way reduce the size of
  `external/`. Even for a relatively simple project like Buildbarn's
  own bb-storage, `external/` is 2.5 GB in size, which is 200 times as
  big as the Git checkout of that project.

  By performing all analysis remotely, none of this data needs to be
  present on the local system. A cluster may also cache this data
  centrally, which should lead to less time waiting on downloads and a
  reduction in network traffic against third-party sites.

- **Easier integration.**

  Systems that are capable of calling into Bazel (CI systems, web-based
  IDEs, etc.) often need to provide a full execution environments for
  running the Bazel CLI, so they frequently do things like launching
  Docker containers behind the scenes. In the case of Bonanza it is
  possible to launch builds by calling into a gRPC based service,
  meaning there is an opportunity to simplify the design of such
  systems.

- **Improved collaboration.**

  By running builds fully remotely, it should be easier to launch builds
  and share their progress and results with others. By having all source
  code associated with a given build present in storage, it should be
  easier for people to "clone" a build and collaborate on addressing
  build failures.

Whereas many new build systems make the mistake of designing their own
build language, Bonanza attempts to be compatible with Bazel as much as
realistically possible. It is therefore capable of parsing `BUILD.bazel`
files, reading rule definitions from ordinary `*.bzl` files, and
downloading modules from [Bazel Central Registry](https://registry.bazel.build)
that are declared in `MODULE.bazel`. Bonanza comes with a command line
utility named `bonanza_bazel`. This tool attempts to be a drop-in
replacement for the Bazel command line utility, accepting the same style
of command line flags and `.bazelrc` files.

## Status

Bonanza is at this point still highly experimental. However, it is
already capable of building all targets inside a slightly altered copy
of the bb-storage source tree. This means that Bonanza is already
complete enough that C++ compilation works (at least good enough to
build a functioning copy of `protoc`), and that Starlark rules such as
ones provided by bazel-gazelle, rules\_go, rules\_js, rules\_oci, and
rules\_python tend to work as expected.

Bonanza caches evaluation results. Because loading, analysis,
configuration and action execution are all expressed as keys in the
same Skyframe-like evaluation graph, one cache covers all of them: a
repeated build resolves previously computed keys through the Tag Store
instead of recomputing them. Cache entries are keyed on a semantics
version that workers increment whenever evaluating identical keys with
identical dependency values starts yielding different results, so a
mixed-version fleet cannot serve entries across a rolling upgrade.
Cached values that refer to objects which have since disappeared from
storage are invalidated and recomputed rather than failing the build.
Caching is not optional; `bonanza_builder` refuses to start without a
tag signing key.

This machinery is implemented and wired end to end, but it has not been
measured at scale. `tools/e2e/run.sh` performs a cold build followed by
a warm one and reports the elapsed time of each; it does not assert a
hit rate.

What Bonanza still cannot do is give you your build outputs.
`bonanza_bazel build` verifies that a build succeeds and prints a link
into `bonanza_browser`; `BuildResult.Value` carries no output set, and
the client has no artifact materialization. The client implements
`build`, `test`, `info`, `license` and `version`. There is no `run`,
`query` or `cquery` command, and no Build Event Protocol.

`test` builds the targets its patterns match, runs whichever of them are
declared by a test rule, and prints a per-target summary. A failing test
is a result rather than a build failure: the client reports it and exits
with status 3, the way Bazel does. `--test_output` controls whether the
captured output of a test is printed, and `--test_filter` reaches the
test binary as `TESTBRIDGE_TEST_ONLY`. There is no `test.xml`, no
sharding, and no test caching across invocations beyond what the
evaluation cache already gives. A test carrying `exec_compatible_with`
is not yet honoured -- the test action resolves its execution platform
the way a target with an empty exec group does.

## Differences from upstream

This fork tracks [buildbarn/bonanza](https://github.com/buildbarn/bonanza)
and adds the following. All of it is loading- and analysis-phase work:
none of it makes the client able to run a test, resolve a query or
launch a binary, because those commands do not exist yet.

**Aspects.** `aspect()` supports `attrs`, `toolchains`,
`required_providers`, `required_aspect_providers`, `provides`,
`attr_aspects`, `exec_groups` and `fragments`. Declared providers are
enforced: an aspect that fails to return one errors out, and duplicate
providers are rejected. Aspects are applied to configured targets
through a first-class analysis key, and toolchains declared on an
aspect become its default exec group, mirroring how rules behave.

**Test execution.** `bonanza_bazel test`, backed by a `TestResult`
analysis function that expands the target patterns and a
`TargetTestResult` that runs one test. A test is run as an action
synthesized from the target's `DefaultInfo.files_to_run` -- its
executable, with a runfiles directory populated beside it -- rather than
as an action declared on the configured target: `target.actions` is
exposed to aspects, so an action that exists only because someone ran
`test` would change what every aspect observes about the target.
`CompletedActionResult` exists for the same reason a test is not a build
failure: unlike `SuccessfulActionResult` it reports a non-zero exit as a
value, so the client can print which tests failed instead of the
evaluation stopping at the first one.

**Analysis-time testing.** `testing.analysis_test()`,
`rule(analysis_test = True)`, `analysis_test_transition()` and
`--allow_analysis_failures` work. Test rules receive the common test
attributes (`size`, `timeout`, `flaky`, `local`, `shard_count`) and an
implicit `"test"` exec group that inherits the default exec group's
constraints. `test_suite()` builds the tests it references, but
running them and expanding an empty `tests` attribute to every test in
the package remain unimplemented.

**Repository rule APIs.** `repository_ctx.getenv()`, `path.is_dir()`,
`path.readdir()` and `path.realpath()` are implemented, the last of
these resolving symbolic links inside the input root. File operations
in repository rules follow symlinks, and more archive extensions are
recognized when inferring an archive's format from its URL.

**Starlark API coverage.** `target.actions` is exposed to rule and
aspect implementations, reporting file types and mnemonics, and
carrying the content written by `write()` and `expand_template()`.
`native.existing_rule()` works inside module extensions. User-defined
build settings can be set on the command line. Transitions can parse
string values assigned to native options and can read non-configurable
attribute values. `ctx.runfiles(skip_conflict_checking = ...)` is
accepted. Two additions are deliberately partial:
`ctx.actions.template_dict()` is emulated in the Starlark rule wrapper
by computing substitutions at analysis time, since the native action
encoding path still lacks it, and `config_feature_flag` always
resolves to its default value because Bonanza does not model feature
flag configuration.

**Cache hardening.** Cache tag keys carry a semantics version, so
workers implementing different evaluation semantics read and write
disjoint keys instead of serving each other stale results. Cached
evaluations that reference objects missing from storage are
invalidated and recomputed rather than failing the build.

**End-to-end smoke test.** `tools/e2e/run.sh` builds the demo
deployment and the client, launches an isolated cluster, and drives
`bonanza_bazel` against a test project that asserts analysis, remote
action execution and artifact contents, along with aspect propagation
and repository rule conformance. It is a local tool; CI does not run
it.

## Running Bonanza

This repository contains an example deployment of Bonanza's server side
components, which can be be spawned by running `bazel run
//deployments/demo`. Furthermore, the `bonanza_bazel` command line tool
can be built by running `bazel build //cmd/bonanza_bazel`.

After launching a cluster, it's worth reading the instructions on
[how to build bb-storage using Bonanza](docs/building_bb_storage.md), as
it gives a good overview of the differences between plain
Bazel/Buildbarn and Bonanza.

## Contributing to Bonanza

As this project essentially attempts to provide an alternative to Bazel,
it has a fairly large scope. Contributions are therefore very much
appreciated. Be sure to [join `#buildbarn` on Slack](https://github.com/buildbarn#join-us-on-slack)
to get involved.

## Additional resources

- [Presentation](https://docs.google.com/presentation/d/1uh6CxvvziQunw55e_bs1Juz3jfaiE-QJVs2DCfeMeTw/edit?usp=sharing) at the 2025-03-20 Snowflake Buildbarn meetup.
