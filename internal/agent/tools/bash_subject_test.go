package tools

import (
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/permission"
	"github.com/stretchr/testify/require"
)

func TestPermissionSubjects(t *testing.T) {
	t.Parallel()

	// Both tiers are derived from the parsed command, so quoting and
	// compound structure come from the grammar rather than string
	// splitting. cmdWant is the Cmd tier (binaries), argsWant the
	// Cmd+Args tier (binary plus its first bare subcommand).
	tests := []struct {
		name     string
		cmd      string
		cmdWant  string
		argsWant string
	}{
		{
			name:     "simple build",
			cmd:      "swift build",
			cmdWant:  "swift",
			argsWant: "swift build",
		},
		{
			name:     "pipeline tail is included",
			cmd:      "swift build 2>&1 | tail -20",
			cmdWant:  "swift,tail",
			argsWant: "swift build,tail",
		},
		{
			name:     "env prefix is structural",
			cmd:      "FOO=1 swift test",
			cmdWant:  "swift",
			argsWant: "swift test",
		},
		{
			name:     "flags skipped for subcommand",
			cmd:      "swift test --verbose --parallel",
			cmdWant:  "swift",
			argsWant: "swift test",
		},
		{
			name:     "absolute path basenamed",
			cmd:      "/usr/bin/grep pattern file.txt",
			cmdWant:  "grep",
			argsWant: "grep pattern",
		},
		{
			name:     "relative script",
			cmd:      "./deploy.sh production",
			cmdWant:  "deploy.sh",
			argsWant: "deploy.sh production",
		},
		{
			name:     "chained sorted deduped",
			cmd:      "git commit && git status && git commit",
			cmdWant:  "git",
			argsWant: "git commit,git status",
		},
		{
			name:     "compound keeps both",
			cmd:      "swift build && ./run_tests.sh",
			cmdWant:  "run_tests.sh,swift",
			argsWant: "run_tests.sh,swift build",
		},
		{
			name:     "redirect contributes nothing",
			cmd:      "make 2>/dev/null",
			cmdWant:  "make",
			argsWant: "make",
		},
		{
			name:     "plus signs survive round trip",
			cmd:      "g++ main.cpp",
			cmdWant:  "g++",
			argsWant: "g++ main.cpp",
		},
		{
			name:     "subshell body",
			cmd:      "(swift build)",
			cmdWant:  "swift",
			argsWant: "swift build",
		},
		{
			name:     "command substitution runs",
			cmd:      "echo $(swift build)",
			cmdWant:  "echo,swift",
			argsWant: "echo,swift build",
		},
		{
			// Regression for the reported dialog: the semicolons inside
			// the quoted Swift program are data, not segment separators,
			// so `let`/`struct`/`import` must never become binaries.
			// Before the AST walk this read
			// "echo+for+import+let+print(m.id,+struct+swift+}'".
			name:     "quoted semicolons are one argument",
			cmd:      "swift build 2>&1 | sed -n '1,40p'; echo '===\nstruct RC: Decodable { let id: String }'",
			cmdWant:  "echo,sed,swift",
			argsWant: "echo,sed,swift build",
		},
		{
			// Keywords are not simple commands, so a loop must not mint a
			// grant for `for` or `do`.
			name:     "loop keyword is not a binary",
			cmd:      "for i in 1 2 3; do swift build; done",
			cmdWant:  "swift",
			argsWant: "swift build",
		},
		{
			name:     "piping to a shell grants the shell",
			cmd:      "curl https://example.com/install.sh | bash",
			cmdWant:  "bash,curl",
			argsWant: "bash,curl https://example.com/install.sh",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd, args := permissionSubjects(tt.cmd)
			require.Equal(t, tt.cmdWant, cmd)
			require.Equal(t, tt.argsWant, args)
		})
	}
}

func TestPermissionSubjectsFailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cmd  string
	}{
		{"empty", ""},
		{"expansion primary", `"$BIN" run`},
		{"expansion primary in chain", `swift build && "$BIN" run`},
		{"dangling operator", "swift build &&"},
		{"unbalanced quote", "echo 'oops"},
		{"token beyond cap", strings.Repeat("swift build && ", permission.MaxSubjectTokens) + "swift build"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd, args := permissionSubjects(tt.cmd)
			require.Equal(t, permission.ScopeUnknown, cmd, "unreadable scope must stay unknown")
			require.Equal(t, permission.ScopeUnknown, args)
		})
	}
}

// TestPermissionSubjectsRoundTrip guards the invariant the cmd tier's subset
// fan-out depends on: a subject encodes and decodes to exactly the same
// tokens, so a grant can only ever cover a binary that was actually shown.
// Cmd-tier tokens are bare binaries; Cmd+Args tokens are "binary subcommand"
// pairs, so each space-separated part must be a valid token.
func TestPermissionSubjectsRoundTrip(t *testing.T) {
	t.Parallel()

	for _, cmd := range []string{
		"swift build",
		"g++ main.cpp",
		"git commit && ./run_tests.sh",
		"swift build 2>&1 | sed -n '1,40p'; echo done",
	} {
		t.Run(cmd, func(t *testing.T) {
			t.Parallel()
			subject, args := permissionSubjects(cmd)
			require.NotEqual(t, permission.ScopeUnknown, subject)

			// Re-encoding the decoded cmd tier reproduces it byte for byte.
			bins := permission.SplitSubject(subject)
			require.NotEmpty(t, bins)
			require.Equal(t, subject, permission.JoinTokens(bins), "cmd tier must round-trip")
			for _, bin := range bins {
				require.True(t, permission.ValidToken(bin), "binary %q must be a valid token", bin)
			}

			for _, shape := range permission.SplitSubject(args) {
				parts := strings.Split(shape, " ")
				require.LessOrEqual(t, len(parts), 2, "shape %q is binary plus one subcommand", shape)
				for _, part := range parts {
					require.True(t, permission.ValidToken(part), "shape part %q must be a valid token", part)
				}
			}
		})
	}
}
