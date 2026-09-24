package arguments_test

import (
	"testing"

	"bonanza.build/pkg/bazelclient/arguments"

	"github.com/stretchr/testify/require"
)

func TestParseCommandAndArguments(t *testing.T) {
	t.Run("NoArguments", func(t *testing.T) {
		// If no arguments are provided, Bazel defaults to
		// displaying help output.
		command, err := arguments.ParseCommandAndArguments(
			arguments.ConfigurationDirectives{},
			[]string{},
		)
		require.NoError(t, err)
		require.Equal(t, arguments.HelpFlags{
			HelpVerbosity: arguments.HelpVerbosity_Medium,
		}, command.(*arguments.HelpCommand).HelpFlags)
	})

	t.Run("CommandNotRecognized", func(t *testing.T) {
		_, err := arguments.ParseCommandAndArguments(
			arguments.ConfigurationDirectives{},
			[]string{
				"bquery",
				"--noinclude_aspects",
			},
		)
		require.EqualError(t, err, "command \"bquery\" not recognized")
	})

	t.Run("Build", func(t *testing.T) {
		t.Run("BuildSettingOverrides", func(t *testing.T) {
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"build",
					"--@rules_foo//:my_string=hello",
					"--//my/pkg:my_bool",
					"--no@rules_foo//:other_bool",
					"//...",
				},
			)
			require.NoError(t, err)
			require.Equal(t, []arguments.BuildSettingOverride{
				{Label: "@rules_foo//:my_string", Value: "hello"},
				{Label: "//my/pkg:my_bool", Value: "true"},
				{Label: "@rules_foo//:other_bool", Value: "false"},
			}, command.(*arguments.BuildCommand).BuildSettingOverrides)
		})

		t.Run("ModuleFlagAliases", func(t *testing.T) {
			command, err := arguments.ParseCommandAndArguments(arguments.ConfigurationDirectives{}, []string{
				"build", "--string_alias=value", "//...",
			})
			require.NoError(t, err)
			require.Equal(t, []arguments.BuildSettingOverride{
				{Label: "string_alias", Value: "value", IsAlias: true, HasExplicitValue: true},
			}, command.(*arguments.BuildCommand).BuildSettingOverrides)
		})

		t.Run("BuildSettingOverrideNegatedWithValue", func(t *testing.T) {
			_, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"build",
					"--no@rules_foo//:other_bool=false",
					"//...",
				},
			)
			require.EqualError(t, err, "flag --no@rules_foo//:other_bool not recognized")
		})

		t.Run("KeepGoingLong", func(t *testing.T) {
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"build",
					"--keep_going",
					"//...",
				},
			)
			require.NoError(t, err)
			require.True(t, command.(*arguments.BuildCommand).BuildFlags.KeepGoing)
		})

		t.Run("KeepGoingLong0", func(t *testing.T) {
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"build",
					"--keep_going=0",
					"//...",
				},
			)
			require.NoError(t, err)
			require.False(t, command.(*arguments.BuildCommand).BuildFlags.KeepGoing)
		})

		t.Run("KeepGoingLong1", func(t *testing.T) {
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"build",
					"--keep_going=1",
					"//...",
				},
			)
			require.NoError(t, err)
			require.True(t, command.(*arguments.BuildCommand).BuildFlags.KeepGoing)
		})

		t.Run("KeepGoingLongFalse", func(t *testing.T) {
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"build",
					"--keep_going=false",
					"//...",
				},
			)
			require.NoError(t, err)
			require.False(t, command.(*arguments.BuildCommand).BuildFlags.KeepGoing)
		})

		t.Run("KeepGoingLongTrue", func(t *testing.T) {
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"build",
					"--keep_going=true",
					"//...",
				},
			)
			require.NoError(t, err)
			require.True(t, command.(*arguments.BuildCommand).BuildFlags.KeepGoing)
		})

		t.Run("KeepGoingLongNo", func(t *testing.T) {
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"build",
					"--keep_going=no",
					"//...",
				},
			)
			require.NoError(t, err)
			require.False(t, command.(*arguments.BuildCommand).BuildFlags.KeepGoing)
		})

		t.Run("KeepGoingLongYes", func(t *testing.T) {
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"build",
					"--keep_going=yes",
					"//...",
				},
			)
			require.NoError(t, err)
			require.True(t, command.(*arguments.BuildCommand).BuildFlags.KeepGoing)
		})

		t.Run("KeepGoingLongOther", func(t *testing.T) {
			_, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"build",
					"--keep_going=maybe",
					"//...",
				},
			)
			require.EqualError(t, err, "flag --keep_going only accepts \"true\", \"false\", \"yes\", \"no\", \"1\" or \"0\", not \"maybe\"")
		})

		t.Run("KeepGoingShort", func(t *testing.T) {
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"build",
					"-k",
					"//...",
				},
			)
			require.NoError(t, err)
			require.True(t, command.(*arguments.BuildCommand).BuildFlags.KeepGoing)
		})

		t.Run("NoKeepGoingLong", func(t *testing.T) {
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"build",
					"-k",
					"--nokeep_going",
					"//...",
				},
			)
			require.NoError(t, err)
			require.False(t, command.(*arguments.BuildCommand).BuildFlags.KeepGoing)
		})

		t.Run("NoKeepGoingShort", func(t *testing.T) {
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"build",
					"-k",
					"-k-",
					"//...",
				},
			)
			require.NoError(t, err)
			require.False(t, command.(*arguments.BuildCommand).BuildFlags.KeepGoing)
		})

		t.Run("NoKeepGoingUnexpectedValue", func(t *testing.T) {
			_, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"build",
					"-k",
					"--nokeep_going=123",
					"//...",
				},
			)
			require.EqualError(t, err, "flag --nokeep_going does not take a value")
		})

		t.Run("PositiveNegativePatterns", func(t *testing.T) {
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"build",
					"--",
					"//...",
					"-//foo/...",
				},
			)
			require.NoError(t, err)
			require.Equal(t, []string{
				"//...",
				"-//foo/...",
			}, command.(*arguments.BuildCommand).Arguments)
		})

		t.Run("ArgumentsInConfiguration", func(t *testing.T) {
			// The "--" argument can be used to stop
			// processing flags. However, should only apply
			// within a single directive.
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{
					"build": [][]string{
						{
							"--",
							"//...",
							"-//doc/...",
						},
						{
							"--platforms",
							"@rules_go//go/toolchain:linux_amd64",
						},
					},
				},
				[]string{
					"build",
					"--keep_going",
				},
			)
			require.NoError(t, err)
			require.Equal(
				t,
				"@rules_go//go/toolchain:linux_amd64",
				command.(*arguments.BuildCommand).BuildFlags.Platforms,
			)
			require.Equal(t, []string{
				"//...",
				"-//doc/...",
			}, command.(*arguments.BuildCommand).Arguments)
		})
	})

	t.Run("Clean", func(t *testing.T) {
		t.Run("HelpVerbosityNotApplicableViaArguments", func(t *testing.T) {
			_, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"clean",
					"--help_verbosity",
					"short",
				},
			)
			require.EqualError(t, err, "flag --help_verbosity does not apply to this command")
		})

		t.Run("HelpVerbosityNotApplicableViaConfigClean", func(t *testing.T) {
			_, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{
					"clean": [][]string{{
						"--help_verbosity",
						"short",
					}},
				},
				[]string{
					"clean",
				},
			)
			require.EqualError(t, err, "flag --help_verbosity does not apply to this command")
		})

		t.Run("HelpVerbosityNotApplicableViaConfigCommon", func(t *testing.T) {
			// In "common", it is permitted to place flags
			// that aren't necessarily applicable to the
			// current command.
			_, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{
					"common": [][]string{{
						"--help_verbosity",
						"short",
					}},
				},
				[]string{
					"clean",
				},
			)
			require.NoError(t, err)
		})

		t.Run("HelpVerbosityNotApplicableViaConfigAlways", func(t *testing.T) {
			// When placed in "always", we must throw errors
			// if flags aren't applicable.
			_, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{
					"always": [][]string{{
						"--help_verbosity",
						"short",
					}},
				},
				[]string{
					"clean",
				},
			)
			require.EqualError(t, err, "flag --help_verbosity does not apply to this command")
		})

		t.Run("ShortNotApplicable", func(t *testing.T) {
			_, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"clean",
					"--short",
				},
			)
			require.EqualError(t, err, "flag --short does not apply to this command")
		})
	})

	t.Run("Help", func(t *testing.T) {
		t.Run("NoFlags", func(t *testing.T) {
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"help",
				},
			)
			require.NoError(t, err)
			require.Equal(t, arguments.HelpFlags{
				HelpVerbosity: arguments.HelpVerbosity_Medium,
			}, command.(*arguments.HelpCommand).HelpFlags)
		})

		t.Run("Short", func(t *testing.T) {
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"help",
					"--short",
				},
			)
			require.NoError(t, err)
			require.Equal(t, arguments.HelpFlags{
				HelpVerbosity: arguments.HelpVerbosity_Short,
			}, command.(*arguments.HelpCommand).HelpFlags)
		})

		t.Run("ShortUnexpectedValue", func(t *testing.T) {
			_, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"help",
					"--short=123",
				},
			)
			require.EqualError(t, err, "flag --short does not take a value")
		})

		t.Run("DashL", func(t *testing.T) {
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"help",
					"-l",
				},
			)
			require.NoError(t, err)
			require.Equal(t, arguments.HelpFlags{
				HelpVerbosity: arguments.HelpVerbosity_Long,
			}, command.(*arguments.HelpCommand).HelpFlags)
		})

		t.Run("HelpVerbosity", func(t *testing.T) {
			t.Run("Equals", func(t *testing.T) {
				command, err := arguments.ParseCommandAndArguments(
					arguments.ConfigurationDirectives{},
					[]string{
						"help",
						"--help_verbosity=short",
					},
				)
				require.NoError(t, err)
				require.Equal(t, arguments.HelpFlags{
					HelpVerbosity: arguments.HelpVerbosity_Short,
				}, command.(*arguments.HelpCommand).HelpFlags)
			})

			t.Run("Space", func(t *testing.T) {
				command, err := arguments.ParseCommandAndArguments(
					arguments.ConfigurationDirectives{},
					[]string{
						"help",
						"--help_verbosity",
						"long",
					},
				)
				require.NoError(t, err)
				require.Equal(t, arguments.HelpFlags{
					HelpVerbosity: arguments.HelpVerbosity_Long,
				}, command.(*arguments.HelpCommand).HelpFlags)
			})

			t.Run("MissingValue", func(t *testing.T) {
				_, err := arguments.ParseCommandAndArguments(
					arguments.ConfigurationDirectives{},
					[]string{
						"help",
						"--help_verbosity",
					},
				)
				require.EqualError(t, err, "flag --help_verbosity expects a value")
			})

			t.Run("UnknownValue", func(t *testing.T) {
				_, err := arguments.ParseCommandAndArguments(
					arguments.ConfigurationDirectives{},
					[]string{
						"help",
						"--help_verbosity",
						"large",
					},
				)
				require.EqualError(t, err, "flag --help_verbosity only accepts \"long\", \"medium\" or \"short\", not \"large\"")
			})
		})

		t.Run("WithCommandName", func(t *testing.T) {
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"help",
					"build",
				},
			)
			require.NoError(t, err)
			require.Equal(
				t,
				[]string{"build"},
				command.(*arguments.HelpCommand).Arguments,
			)
		})
	})

	t.Run("Run", func(t *testing.T) {
		t.Run("PlatformsMostSpecific", func(t *testing.T) {
			// If multiple directives specify the same
			// flags, we should always prefer the one that
			// is most specific.
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{
					"common": [][]string{{
						"--platforms",
						"@rules_go//go/toolchain:linux_amd64",
					}},
					"run": [][]string{{
						"--platforms",
						"@bazel_tools//tools:host_platform",
					}},
				},
				[]string{
					"run",
					"//cmd/my_tool",
					"--",
					"--help",
				},
			)
			require.NoError(t, err)
			require.Equal(
				t,
				"@bazel_tools//tools:host_platform",
				command.(*arguments.RunCommand).BuildFlags.Platforms,
			)
		})

		t.Run("RunUnderMultipleSameSpecificity", func(t *testing.T) {
			// If the same argument is provided at the same
			// specificity multiple times, the last value
			// should be applied.
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{
					"run": [][]string{
						{"--run_under=echo"},
						{"--run_under=time"},
					},
				},
				[]string{
					"run",
					"//cmd/my_tool",
				},
			)
			require.NoError(t, err)
			require.Equal(
				t,
				"time",
				command.(*arguments.RunCommand).RunFlags.RunUnder,
			)
		})
	})

	t.Run("Version", func(t *testing.T) {
		t.Run("NoFlags", func(t *testing.T) {
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"version",
				},
			)
			require.NoError(t, err)
			require.Equal(t, arguments.VersionFlags{
				GnuFormat: false,
			}, command.(*arguments.VersionCommand).VersionFlags)
		})

		t.Run("GNUFormatInConfigurationFile", func(t *testing.T) {
			// Perform an end to end test, where we have a
			// simple .bazelrc file in the home directory
			// that contains "version --gnu_format". When we
			// run "bazel version", this should cause it to
			// print just the program name and version
			// number.
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{
					"version": [][]string{
						{"--gnu_format"},
					},
				},
				[]string{
					"version",
				},
			)
			require.NoError(t, err)
			require.Equal(t, arguments.VersionFlags{
				GnuFormat: true,
			}, command.(*arguments.VersionCommand).VersionFlags)
		})

		t.Run("GNUFormatBehindConfigFlag", func(t *testing.T) {
			command, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{
					"version:foo": [][]string{
						{"--gnu_format"},
					},
				},
				[]string{
					"version",
					"--config=foo",
				},
			)
			require.NoError(t, err)
			require.Equal(t, arguments.VersionFlags{
				GnuFormat: true,
			}, command.(*arguments.VersionCommand).VersionFlags)
		})

		t.Run("InvalidConfig", func(t *testing.T) {
			_, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{},
				[]string{
					"version",
					"--config=foo",
				},
			)
			require.EqualError(t, err, "config value \"foo\" is not defined in any configuration file")
		})

		t.Run("CyclicConfig", func(t *testing.T) {
			_, err := arguments.ParseCommandAndArguments(
				arguments.ConfigurationDirectives{
					"version:foo": [][]string{
						{"--config=bar"},
					},
					"version:bar": [][]string{
						{"--config=baz"},
					},
					"version:baz": [][]string{
						{"--config=foo"},
					},
				},
				[]string{
					"version",
					"--config=foo",
				},
			)
			require.EqualError(t, err, "config expansion for configuration directive \"version:foo\" contains a cycle")
		})
	})
}

func TestQueryOutputFlagIsScopedToQueryCommands(t *testing.T) {
	query, err := arguments.ParseCommandAndArguments(arguments.ConfigurationDirectives{}, []string{
		"query", "--output=label_kind", "//pkg:all",
	})
	require.NoError(t, err)
	require.Equal(t, arguments.QueryOutput(arguments.QueryOutput_LabelKind), query.(*arguments.QueryCommand).QueryFlags.Output)

	cquery, err := arguments.ParseCommandAndArguments(arguments.ConfigurationDirectives{}, []string{
		"cquery", "--output=files", "--platforms=//platforms:exec", "set(//pkg:target)",
	})
	require.NoError(t, err)
	require.Equal(t, arguments.QueryOutput(arguments.QueryOutput_Files), cquery.(*arguments.CqueryCommand).CqueryFlags.Output)
	require.Equal(t, "//platforms:exec", cquery.(*arguments.CqueryCommand).BuildFlags.Platforms)

	_, err = arguments.ParseCommandAndArguments(arguments.ConfigurationDirectives{}, []string{
		"build", "--output=files", "//pkg:target",
	})
	require.ErrorContains(t, err, "does not apply")
}

func TestC1EnvironmentFlagsAndConfigExpansion(t *testing.T) {
	cmd, err := arguments.ParseCommandAndArguments(arguments.ConfigurationDirectives{
		"common":    {{"--announce_rc"}},
		"build":     {{"--action_env=DO_NOT_TRACK=1"}, {"--repo_env=DO_NOT_TRACK=1"}},
		"common:ci": {{"--color=no"}},
		"build:ci":  {{"--action_env=DO_NOT_TRACK=2"}},
	}, []string{"cquery", "--config=ci", "--output=files", "//bazel/python:check.py"})
	require.NoError(t, err)
	cquery := cmd.(*arguments.CqueryCommand)
	require.True(t, cquery.CommonFlags.AnnounceRc)
	require.Equal(t, []string{"DO_NOT_TRACK=1", "DO_NOT_TRACK=2"}, cquery.BuildFlags.ActionEnv)
	require.Equal(t, []string{"DO_NOT_TRACK=1"}, cquery.BuildFlags.RepoEnv)
	require.Equal(t, arguments.Color(arguments.Color_No), cquery.CommonFlags.Color)
	require.Contains(t, cquery.RCAnnouncements, "build:ci: --action_env=DO_NOT_TRACK=<redacted>")
	require.NotContains(t, cquery.RCAnnouncements, "build:ci: --action_env=DO_NOT_TRACK=2")
	require.Contains(t, cquery.RCAnnouncements, "build: --repo_env=DO_NOT_TRACK=<redacted>")
	require.Contains(t, cquery.RCAnnouncements, "common:ci: --color=no")
}

func TestC1InvocationPrecedenceAndProtocol(t *testing.T) {
	rc := arguments.ConfigurationDirectives{
		"common":              {{"--curses"}, {"--show_progress_rate_limit=5"}},
		"common:remote-cache": {{"--remote_cache=grpc://bb-control-plane.cache.svc.cluster.local:8980"}},
		"build":               {{"--show_timestamps"}},
	}
	_, err := arguments.ParseCommandAndArguments(rc, []string{"build", "--config=remote-cache", "//:target"})
	require.ErrorContains(t, err, "--remote_cache")
	require.ErrorContains(t, err, "Bazel REAPI/HTTP")

	cmd, err := arguments.ParseCommandAndArguments(rc, []string{
		"build", "--config=remote-cache",
		"--remote_cache=bonanza+grpcs://storage.example:443",
		"--remote_executor=bonanza+grpcs://scheduler.example:443",
		"--nocurses", "--show_progress_rate_limit=0", "--noshow_timestamps", "//:target",
	})
	require.NoError(t, err)
	flags := cmd.(*arguments.BuildCommand).CommonFlags
	require.Equal(t, "bonanza+grpcs://storage.example:443", flags.RemoteCache)
	require.Equal(t, "bonanza+grpcs://scheduler.example:443", flags.RemoteExecutor)
	require.False(t, flags.Curses)
	require.False(t, flags.ShowTimestamps)
	require.Equal(t, "0", flags.ShowProgressRateLimit)
}

func TestBonanzaEndpointProtocols(t *testing.T) {
	for _, test := range []struct {
		endpoint string
		target   string
		tls      bool
	}{
		{"bonanza+grpc://storage.example:8980", "storage.example:8980", false},
		{"bonanza+grpcs://scheduler.example:443", "scheduler.example:443", true},
		{"bonanza+unix:///run/bonanza/storage.sock", "unix:///run/bonanza/storage.sock", false},
	} {
		target, tls, err := arguments.ParseBonanzaEndpoint(test.endpoint)
		require.NoError(t, err)
		require.Equal(t, test.target, target)
		require.Equal(t, test.tls, tls)
	}
	for _, endpoint := range []string{
		"grpc://bb-control-plane.cache.svc.cluster.local:8980",
		"http://localhost:9095",
		"unix:///run/bonanza/storage.sock",
		"bonanza+unix://host/run/storage.sock",
		"bonanza+grpc://storage.example:8980/path",
		"bonanza+grpcs://user:password@scheduler.example:443",
		"bonanza+grpc://storage.example:0",
		"bonanza+grpc://storage.example:65536",
		"bonanza+grpc://storage.example",
	} {
		_, _, err := arguments.ParseBonanzaEndpoint(endpoint)
		require.Error(t, err, endpoint)
		require.NotContains(t, err.Error(), "password")
	}
}

func TestC1UnsupportedFlagsAreNotModuleAliases(t *testing.T) {
	for _, flag := range []string{
		"--verbose_failures", "--experimental_ui_max_stdouterr_bytes=-1",
		"--show_result=20", "--test_summary=detailed",
		"--incompatible_default_to_explicit_init_py",
		"--remote_upload_local_results=true", "--remote_timeout=30", "--remote_retries=2",
		"--remote_default_exec_properties=c1.queue=small",
		"--remote_download_minimal", "--jobs=100",
	} {
		_, err := arguments.ParseCommandAndArguments(arguments.ConfigurationDirectives{}, []string{"build", flag, "//:target"})
		require.ErrorContains(t, err, "unsupported", flag)
	}
}

func TestInvalidProgressRateLimitRejectsBothRcAndCLI(t *testing.T) {
	for _, rate := range []string{"-1", "NaN", "Inf", "not-a-number"} {
		_, err := arguments.ParseCommandAndArguments(
			arguments.ConfigurationDirectives{"common": {{"--show_progress_rate_limit=5"}}},
			[]string{"build", "--show_progress_rate_limit=" + rate},
		)
		require.ErrorContains(t, err, "--show_progress_rate_limit")
	}
}
