package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRunMain_Version_NoDaemonAccessLogs(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMain([]string{"version"}, map[string]string{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runMain(version) code = %d, want 0", code)
	}
	if strings.Contains(stderr.String(), "event=") || strings.Contains(stderr.String(), "\"event\"") {
		t.Fatalf("version command wrote daemon-style logs to stderr: %q", stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got == "" {
		t.Fatalf("version command stdout empty")
	}
}

func TestRunMain_DaemonRootHelp(t *testing.T) {
	t.Parallel()

	cases := [][]string{
		{"--help"},
		{"-h"},
		{"help"},
	}
	for _, args := range cases {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			code := runMain(args, map[string]string{}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("runMain(%v) code = %d, want 0 stderr=%q", args, code, stderr.String())
			}
			out := stdout.String()
			if !strings.Contains(out, "usage: go-contactd [flags]") {
				t.Fatalf("stdout missing root usage: %q", out)
			}
			if !strings.Contains(out, "--listen-addr") || !strings.Contains(out, "--db-path") || !strings.Contains(out, "contactctl user") {
				t.Fatalf("stdout missing daemon/help details: %q", out)
			}
			for _, forbidden := range []string{"--listen addr", "--bind addr", "--addr addr", "--db path"} {
				if strings.Contains(out, forbidden) {
					t.Fatalf("stdout should not advertise daemon alias %q: %q", forbidden, out)
				}
			}
			for _, wantEnv := range []string{
				"CONTACTD_LISTEN_ADDR",
				"PORT",
				"CONTACTD_DB_PATH",
				"CONTACTD_BASE_URL",
				"CONTACTD_LOG_LEVEL",
				"CONTACTD_LOG_FORMAT",
				"CONTACTD_TRUST_PROXY_HEADERS",
				"CONTACTD_REQUEST_MAX_BYTES",
				"CONTACTD_VCARD_MAX_BYTES",
				"CONTACTD_FORCE_SEED",
				"CONTACTD_USERS",
				"CONTACTD_USER_*",
				"CONTACTD_DEFAULT_BOOK_SLUG",
				"CONTACTD_DEFAULT_BOOK_NAME",
				"CONTACTD_CHANGE_RETENTION_DAYS",
				"CONTACTD_CHANGE_RETENTION_MAX_REVISIONS",
				"CONTACTD_PRUNE_INTERVAL",
				"CONTACTD_ENABLE_ADDRESSBOOK_COLOR",
			} {
				if !strings.Contains(out, wantEnv) {
					t.Fatalf("stdout missing environment var %q: %q", wantEnv, out)
				}
			}
			if got := stderr.String(); got != "" {
				t.Fatalf("stderr = %q, want empty", got)
			}
		})
	}
}

func TestRunMain_ContactctlUserHelpFlagsAndSubcommand(t *testing.T) {
	t.Parallel()

	cases := [][]string{
		{"user", "--help"},
		{"user", "-h"},
		{"user", "help"},
	}
	for _, args := range cases {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			code := runMainProgramWithInput("contactctl", args, map[string]string{}, strings.NewReader(""), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("runMainProgramWithInput(contactctl,%v) code = %d, want 0 stderr=%q", args, code, stderr.String())
			}
			out := stdout.String()
			if !strings.Contains(out, "usage: contactctl user <add|list|delete|passwd>") {
				t.Fatalf("stdout missing user usage: %q", out)
			}
			if !strings.Contains(out, "password-stdin") {
				t.Fatalf("stdout missing password-stdin hint: %q", out)
			}
			if got := stderr.String(); got != "" {
				t.Fatalf("stderr = %q, want empty", got)
			}
		})
	}
}

func TestRunMain_ServeHelpFlagPrintsHelp(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{{"serve", "--help"}, {"serve", "-h"}} {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			code := runMain(args, map[string]string{}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("runMain(%v) code = %d, want 0; stderr=%q", args, code, stderr.String())
			}
			out := stdout.String()
			if !strings.Contains(out, "usage: go-contactd [flags]") {
				t.Fatalf("stdout missing serve usage: %q", out)
			}
			if !strings.Contains(out, "--listen-addr") || !strings.Contains(out, "--port") || !strings.Contains(out, "--db-path") {
				t.Fatalf("stdout missing expected serve flags: %q", out)
			}
			if strings.Contains(out, "--listen addr") || strings.Contains(out, "--db path") {
				t.Fatalf("stdout should not advertise daemon aliases: %q", out)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestRunMain_NoArgsDefaultsToServe(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMain(nil, map[string]string{"CONTACTD_DB_PATH": t.TempDir()}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("runMain(no args) code = %d, want 2 startup failure from serve path", code)
	}
	if got := stderr.String(); !strings.Contains(got, "go-contactd:") {
		t.Fatalf("stderr = %q, want daemon-style fatal error prefix", got)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunMain_FlagArgsDispatchToServe(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMain([]string{"-d", t.TempDir()}, map[string]string{}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("runMain(-d <dir>) code = %d, want 2 startup failure from serve path", code)
	}
	if got := stderr.String(); !strings.Contains(got, "go-contactd:") {
		t.Fatalf("stderr = %q, want daemon-style fatal error prefix", got)
	}
}

func TestRunMain_StartupFailure_NoDuplicateStructuredLog(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMain([]string{"-d", t.TempDir()}, map[string]string{}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("runMain startup failure code = %d, want 2", code)
	}
	out := stderr.String()
	if !strings.Contains(out, "go-contactd: cannot open database ") {
		t.Fatalf("stderr = %q, want daemon-style open db error", out)
	}
	if strings.Contains(out, `event="db error"`) || strings.Contains(out, `level=ERROR`) {
		t.Fatalf("stderr should not include structured startup log duplicate, got %q", out)
	}
}

func TestRunMain_ContactctlHelp(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMainProgramWithInput("contactctl", []string{"--help"}, map[string]string{}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("contactctl --help code = %d, want 0 stderr=%q", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "usage: contactctl <user|export|import|version>") {
		t.Fatalf("contactctl help stdout = %q, want admin usage", got)
	}
}

func TestLogging_CLI_NoDaemonAccessLogs(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMain([]string{"version"}, map[string]string{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runMain(version) code = %d, want 0", code)
	}
	if strings.Contains(stderr.String(), "event=") || strings.Contains(stderr.String(), "\"event\"") {
		t.Fatalf("version command wrote daemon-style logs to stderr: %q", stderr.String())
	}
}

func TestRunMain_Version_FormatTextAndJSON(t *testing.T) {
	t.Parallel()

	origVersion, origCommit, origBuildDate := version, commit, buildDate
	version, commit, buildDate = "v0.1.0", "abc1234", "2026-02-24"
	defer func() {
		version, commit, buildDate = origVersion, origCommit, origBuildDate
	}()

	var textOut, textErr bytes.Buffer
	code := runMain([]string{"version", "--format", "text"}, map[string]string{}, &textOut, &textErr)
	if code != 0 {
		t.Fatalf("version --format text code = %d, want 0 stderr=%q", code, textErr.String())
	}
	if got := textOut.String(); !strings.Contains(got, "go-contactd v0.1.0") || !strings.Contains(got, "commit abc1234") || !strings.Contains(got, "built 2026-02-24") {
		t.Fatalf("version text output missing metadata: %q", got)
	}

	var jsonOut, jsonErr bytes.Buffer
	code = runMain([]string{"version", "--format", "json"}, map[string]string{}, &jsonOut, &jsonErr)
	if code != 0 {
		t.Fatalf("version --format json code = %d, want 0 stderr=%q", code, jsonErr.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(jsonOut.Bytes(), &doc); err != nil {
		t.Fatalf("json.Unmarshal version output: %v; out=%q", err, jsonOut.String())
	}
	if got, want := doc["version"], "v0.1.0"; got != want {
		t.Fatalf("version json field = %#v, want %q", got, want)
	}
	if got, want := doc["commit"], "abc1234"; got != want {
		t.Fatalf("commit json field = %#v, want %q", got, want)
	}
	if got, want := doc["build_date"], "2026-02-24"; got != want {
		t.Fatalf("build_date json field = %#v, want %q", got, want)
	}
}

func TestRunMain_Version_InvalidFormatReturns2(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMain([]string{"version", "--format", "yaml"}, map[string]string{}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("version invalid format code = %d, want 2", code)
	}
	if got := stderr.String(); !strings.Contains(got, "invalid --format") {
		t.Fatalf("stderr missing invalid format error: %q", got)
	}
}

func TestRunMainProgram_Version_UsesProgramNameInTextOutput(t *testing.T) {
	origVersion, origCommit, origBuildDate := version, commit, buildDate
	version, commit, buildDate = "v0.1.0", "abc1234", "2026-02-24"
	defer func() {
		version, commit, buildDate = origVersion, origCommit, origBuildDate
	}()

	tests := []struct {
		name string
		prog string
		args []string
		want string
	}{
		{name: "daemon_short_flag", prog: "contactd", args: []string{"-V"}, want: "contactd v0.1.0"},
		{name: "admin_short_flag", prog: "contactctl", args: []string{"-V"}, want: "contactctl v0.1.0"},
		{name: "daemon_version_subcommand", prog: "contactd", args: []string{"version"}, want: "contactd v0.1.0"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runMainProgramWithInput(tt.prog, tt.args, map[string]string{}, strings.NewReader(""), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("runMainProgramWithInput(%q,%v) code = %d, want 0 stderr=%q", tt.prog, tt.args, code, stderr.String())
			}
			if got := stdout.String(); !strings.Contains(got, tt.want) {
				t.Fatalf("stdout = %q, want substring %q", got, tt.want)
			}
		})
	}
}

func TestRunMainProgram_DaemonUnknownCommand_IsDaemonStyle(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMainProgramWithInput("contactd", []string{"server"}, map[string]string{}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	errOut := stderr.String()
	if !strings.Contains(errOut, "contactd: unknown command: server") {
		t.Fatalf("stderr = %q, want daemon-style unknown command", errOut)
	}
	if strings.Contains(errOut, "compat alias") {
		t.Fatalf("stderr = %q, want no compat alias hint", errOut)
	}
}

func TestRunMainProgram_AdminUnknownCommand_HasProgramPrefix(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMainProgramWithInput("contactctl", []string{"wat"}, map[string]string{}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "contactctl: unknown command: wat") {
		t.Fatalf("stderr = %q, want contactctl-prefixed unknown command", got)
	}
}

func TestRunMainProgram_AdminUserUnknownSubcommand_HasProgramPrefix(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := runMainProgramWithInput("contactctl", []string{"user", "wat"}, map[string]string{}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("code = %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	got := stderr.String()
	if !strings.Contains(got, "contactctl: unknown user subcommand: wat") {
		t.Fatalf("stderr = %q, want contactctl-prefixed user subcommand error", got)
	}
}

func TestRunMainProgram_DaemonFlagErrors_AreFlattenedForUsers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		want    string
		notWant []string
	}{
		{
			name:    "unknown_flag",
			args:    []string{"--bogus"},
			want:    "flag provided but not defined",
			notWant: []string{"load config:", "parse serve flags:"},
		},
		{
			name:    "missing_arg",
			args:    []string{"-d"},
			want:    "flag needs an argument: -d",
			notWant: []string{"load config:", "parse serve flags:"},
		},
		{
			name:    "invalid_port_range",
			args:    []string{"-p", "0"},
			want:    "--port must be",
			notWant: []string{"load config:", "parse serve flags:"},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			code := runMainProgramWithInput("contactd", tt.args, map[string]string{}, strings.NewReader(""), &stdout, &stderr)
			if code != 2 {
				t.Fatalf("code = %d, want 2; stderr=%q", code, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
			got := stderr.String()
			if !strings.Contains(got, "contactd: ") {
				t.Fatalf("stderr = %q, want daemon prefix", got)
			}
			if !strings.Contains(got, tt.want) {
				t.Fatalf("stderr = %q, want substring %q", got, tt.want)
			}
			for _, s := range tt.notWant {
				if strings.Contains(got, s) {
					t.Fatalf("stderr = %q, should not contain %q", got, s)
				}
			}
		})
	}
}

func TestRunMainProgram_VersionHelp_UsesProgramName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		prog string
		args []string
		want string
	}{
		{prog: "contactd", args: []string{"version", "--help"}, want: "usage: contactd version [--format text|json]"},
		{prog: "contactctl", args: []string{"version", "--help"}, want: "usage: contactctl version [--format text|json]"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.prog, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			code := runMainProgramWithInput(tt.prog, tt.args, map[string]string{}, strings.NewReader(""), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("code = %d, want 0 stderr=%q", code, stderr.String())
			}
			if got := stdout.String(); !strings.Contains(got, tt.want) {
				t.Fatalf("stdout = %q, want %q", got, tt.want)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}
