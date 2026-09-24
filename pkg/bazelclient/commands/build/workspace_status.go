package build

import (
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"

	"bonanza.build/pkg/bazelclient/arguments"
)

const stampSetting = "@bazel_tools//command_line_option:stamp"

// workspaceStatus constructs Bazel's stable and volatile status inputs. Keep
// unstamped builds independent of the wall clock and machine identity: those
// inputs must remain reusable across invocations.
func workspaceStatus(overrides []arguments.BuildSettingOverride, embedLabel string, now time.Time, hostname, username string) (string, string, error) {
	if strings.ContainsAny(embedLabel, "\r\n\x00") {
		return "", "", fmt.Errorf("--embed_label must not contain a newline or NUL byte")
	}
	stamped, err := stampEnabled(overrides)
	if err != nil {
		return "", "", err
	}
	if !stamped {
		return "", "", nil
	}
	if strings.ContainsAny(hostname, "\r\n\x00") || strings.ContainsAny(username, "\r\n\x00") {
		return "", "", fmt.Errorf("workspace status hostname and username must not contain a newline or NUL byte")
	}
	stable := ""
	if embedLabel != "" {
		stable = "BUILD_EMBED_LABEL " + embedLabel + "\n"
	}
	stable += fmt.Sprintf("BUILD_HOST %s\nBUILD_USER %s\n", hostname, username)
	volatile := fmt.Sprintf("BUILD_TIMESTAMP %d\nFORMATTED_DATE %s\n", now.Unix(), now.UTC().Format("2006 Jan 2 15 04 05 Mon"))
	return stable, volatile, nil
}

func stampEnabled(overrides []arguments.BuildSettingOverride) (bool, error) {
	stamped := false
	for _, override := range overrides {
		if override.Label != stampSetting {
			continue
		}
		switch override.Value {
		case "true", "yes", "1":
			stamped = true
		case "false", "no", "0":
			stamped = false
		default:
			return false, fmt.Errorf("invalid --stamp value %q", override.Value)
		}
	}
	return stamped, nil
}

func currentWorkspaceStatus(overrides []arguments.BuildSettingOverride, embedLabel string) (string, string, error) {
	stamped, err := stampEnabled(overrides)
	if err != nil {
		return "", "", err
	}
	if !stamped {
		return workspaceStatus(overrides, embedLabel, time.Time{}, "", "")
	}
	hostname, err := os.Hostname()
	if err != nil {
		return "", "", fmt.Errorf("get build hostname: %w", err)
	}
	currentUser, err := user.Current()
	if err != nil {
		return "", "", fmt.Errorf("get build username: %w", err)
	}
	return workspaceStatus(overrides, embedLabel, time.Now(), hostname, currentUser.Username)
}
