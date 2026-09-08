package permission

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubjectScopedPersistentGrants(t *testing.T) {
	service := NewPermissionService("/tmp", false, []string{})

	req := CreatePermissionRequest{
		SessionID:   "subject-session",
		ToolCallID:  "call-build",
		ToolName:    "bash",
		Description: "Execute command: swift build",
		Action:      "execute",
		Path:        "/tmp",
		Subject:     "swift build",
	}

	events := service.Subscribe(t.Context())

	var granted bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		granted, _ = service.Request(t.Context(), req)
	}()

	event := <-events
	require.Equal(t, "swift build", event.Payload.Subject)
	require.True(t, service.GrantPersistent(event.Payload))
	<-done
	require.True(t, granted, "first request should be granted")

	// The same subject auto-approves even with different trailing args;
	// a matching grant publishes a notification but never a new prompt.
	same := req
	same.ToolCallID = "call-build-2"
	same.Description = "Execute command: swift build --explicit"
	ok, err := service.Request(t.Context(), same)
	require.NoError(t, err)
	assert.True(t, ok, "same subject should auto-approve")

	// A different subject must still prompt. With no respondent, the
	// request times out, which is how we prove no silent grant happened.
	other := CreatePermissionRequest{
		SessionID:   "subject-session",
		ToolCallID:  "call-rm",
		ToolName:    "bash",
		Description: "Execute command: rm -rf /tmp/x",
		Action:      "execute",
		Path:        "/tmp",
		Subject:     "rm -rf",
	}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	prompted := make(chan bool, 1)
	go func() {
		g, err := service.Request(ctx, other)
		prompted <- g && err == nil
	}()

	select {
	case ev := <-events:
		assert.Equal(t, "rm -rf", ev.Payload.Subject, "prompt should carry the new subject")
		<-prompted
	case <-time.After(3 * time.Second):
		t.Fatal("different subject should have prompted")
	}
}

func TestEmptySubjectKeepsLegacyBehavior(t *testing.T) {
	service := NewPermissionService("/tmp", false, []string{})

	req := CreatePermissionRequest{
		SessionID:   "legacy-session",
		ToolCallID:  "call-legacy",
		ToolName:    "file_tool",
		Description: "Edit file",
		Action:      "write",
		Path:        "/tmp/notes.md",
	}

	events := service.Subscribe(t.Context())

	var granted bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		granted, _ = service.Request(t.Context(), req)
	}()

	event := <-events
	require.True(t, service.GrantPersistent(event.Payload))
	<-done
	require.True(t, granted)

	same := req
	same.ToolCallID = "call-legacy-2"
	ok, err := service.Request(t.Context(), same)
	require.NoError(t, err)
	assert.True(t, ok, "empty subject should behave like the old tool+action+path key")
}

func TestScopedTierGrants(t *testing.T) {
	service := NewPermissionService("/tmp", false, []string{})

	req := CreatePermissionRequest{
		SessionID:   "tier-session",
		ToolCallID:  "call1",
		ToolName:    "bash",
		Description: "Execute command: git commit",
		Action:      "execute",
		Path:        "/tmp",
		Subject:     "git",
		SubjectFull: "git commit",
	}

	events := service.Subscribe(t.Context())

	// User approves the args tier: the grant is stored namespaced.
	var granted bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		granted, _ = service.Request(t.Context(), req)
	}()
	event := <-events
	event.Payload.Subject = "args:git commit"
	require.True(t, service.GrantPersistent(event.Payload))
	<-done
	require.True(t, granted)

	// Same shape, different args: no grant covers it, so it prompts (and
	// times out with no respondent).
	push := req
	push.ToolCallID = "call2"
	push.SubjectFull = "git push"
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_, err := service.Request(ctx, push)
	require.ErrorIs(t, err, context.DeadlineExceeded, "args grant must not cover different args")

	// Drain the prompt emitted by the timed-out request.
drain:
	for {
		select {
		case <-events:
		default:
			break drain
		}
	}

	// Approve the cmd tier for that prompt; a new invocation with fresh
	// args now passes silently under the binary grant.
	done2 := make(chan struct{})
	var granted2 bool
	go func() {
		defer close(done2)
		granted2, _ = service.Request(t.Context(), push)
	}()
	ev := <-events
	ev.Payload.Subject = "cmd:git"
	require.True(t, service.GrantPersistent(ev.Payload))
	<-done2
	require.True(t, granted2)

	pull := push
	pull.ToolCallID = "call3"
	pull.SubjectFull = "git pull"
	ok, err := service.Request(t.Context(), pull)
	require.NoError(t, err)
	assert.True(t, ok, "cmd grant should cover any git invocation")
}

// TestCmdGrantCoversSubsets pins the cmd tier's subset semantics: approving
// a chain also covers any subset of the binaries you saw, but never a binary
// you did not.
func TestCmdGrantCoversSubsets(t *testing.T) {
	service := NewPermissionService("/tmp", false, []string{})

	req := CreatePermissionRequest{
		SessionID:   "subset-session",
		ToolCallID:  "call-chain",
		ToolName:    "bash",
		Description: "Execute command: swift build && echo done",
		Action:      "execute",
		Path:        "/tmp",
		Subject:     "echo,sed,swift",
		SubjectFull: "echo,sed,swift build",
	}

	events := service.Subscribe(t.Context())

	var granted bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		granted, _ = service.Request(t.Context(), req)
	}()
	event := <-events
	event.Payload.Subject = ScopeSubject(ScopeCmd, req.Subject)
	require.True(t, service.GrantPersistent(event.Payload))
	<-done
	require.True(t, granted)

	// Single binary and reordered pairs all run silently, with SubjectFull
	// cleared so only the cmd tier can cover them.
	for i, subject := range []string{"swift", "echo,sed", "sed,swift,echo"} {
		sub := req
		sub.ToolCallID = fmt.Sprintf("call-subset-%d", i)
		sub.Subject = subject
		sub.SubjectFull = ""
		ok, err := service.Request(t.Context(), sub)
		require.NoError(t, err, "subset %q should not prompt", subject)
		assert.True(t, ok, "subset %q should auto-approve", subject)
	}

drain:
	for {
		select {
		case <-events:
		default:
			break drain
		}
	}

	// One unapproved binary added: the whole chain prompts again.
	extra := req
	extra.ToolCallID = "call-rm"
	extra.Subject = "rm,swift"
	extra.SubjectFull = ""
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_, err := service.Request(ctx, extra)
	require.ErrorIs(t, err, context.DeadlineExceeded, "an unapproved binary must still prompt")
}

// TestUnknownSubjectNeverCovers proves the fail-closed marker is inert in
// both directions: approving it grants only that call, and it is never
// remembered, so the next unreadable command asks again.
func TestUnknownSubjectNeverCovers(t *testing.T) {
	service := NewPermissionService("/tmp", false, []string{})

	req := CreatePermissionRequest{
		SessionID:   "unknown-session",
		ToolCallID:  "call-unknown",
		ToolName:    "bash",
		Description: `Execute command: "$BIN" run`,
		Action:      "execute",
		Path:        "/tmp",
		Subject:     ScopeUnknown,
		SubjectFull: ScopeUnknown,
	}

	events := service.Subscribe(t.Context())

	var granted bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		granted, _ = service.Request(t.Context(), req)
	}()
	event := <-events
	require.Equal(t, ScopeUnknown, event.Payload.Subject)
	require.True(t, service.GrantPersistent(event.Payload))
	<-done
	require.True(t, granted, "approving the prompt should still run this call")

	second := req
	second.ToolCallID = "call-unknown-2"
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_, err := service.Request(ctx, second)
	require.ErrorIs(t, err, context.DeadlineExceeded, "unknown scope must never be remembered")
}

// TestDownloadDomainGrantCoversDomain proves the download tool's
// domain-keyed session grant: approving one download from a host covers
// every later download from that host, so files no longer re-prompt, while
// a different host still asks.
func TestDownloadDomainGrantCoversDomain(t *testing.T) {
	service := NewPermissionService("/tmp", false, []string{})

	req := CreatePermissionRequest{
		SessionID:   "download-session",
		ToolCallID:  "call-dl-1",
		ToolName:    "download",
		Description: "Download file from URL: https://example.com/first.zip to /tmp/first.zip",
		Action:      "download",
		Path:        "/tmp",
		Subject:     "example.com",
	}

	events := service.Subscribe(t.Context())

	var granted bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		granted, _ = service.Request(t.Context(), req)
	}()

	event := <-events
	require.Equal(t, "example.com", event.Payload.Subject)
	require.True(t, service.GrantPersistent(event.Payload))
	<-done
	require.True(t, granted, "first request should be granted")

	// A different file from the same domain auto-approves.
	same := req
	same.ToolCallID = "call-dl-2"
	same.Description = "Download file from URL: https://example.com/second.zip to /tmp/second.zip"
	ok, err := service.Request(t.Context(), same)
	require.NoError(t, err)
	assert.True(t, ok, "same-domain download should auto-approve")

	// A different domain must still prompt.
	other := same
	other.ToolCallID = "call-dl-3"
	other.Subject = "other.example.net"
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	prompted := make(chan bool, 1)
	go func() {
		g, err := service.Request(ctx, other)
		prompted <- g && err == nil
	}()

	select {
	case ev := <-events:
		assert.Equal(t, "other.example.net", ev.Payload.Subject, "prompt should carry the new domain")
		<-prompted
	case <-time.After(3 * time.Second):
		t.Fatal("different domain should have prompted")
	}
}

// TestPromptFlagsUncoveredTokens proves a partially approved chain prompts
// with only the unapproved binaries marked as new, so the dialog can show
// what the approval is actually about instead of one mixed list.
func TestPromptFlagsUncoveredTokens(t *testing.T) {
	service := NewPermissionService("/tmp", false, []string{"bash:cmd:mkdir"})

	// Approve git alone first, so the next chain has one session-granted
	// binary, one config-granted binary, and one brand new one.
	first := CreatePermissionRequest{
		SessionID:   "split-session",
		ToolCallID:  "call-split-1",
		ToolName:    "bash",
		Description: "Execute command: git status",
		Action:      "execute",
		Path:        "/tmp",
		Subject:     "git",
		SubjectFull: "git status",
	}

	events := service.Subscribe(t.Context())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = service.Request(t.Context(), first)
	}()
	event := <-events
	// Mirror the dialog's cmd-tier approval: the session key is stored
	// with the cmd: prefix so later chains can reuse it per binary.
	grant := event.Payload
	grant.Subject = ScopeSubject(ScopeCmd, grant.Subject)
	require.True(t, service.GrantPersistent(grant))
	<-done

	second := CreatePermissionRequest{
		SessionID:   "split-session",
		ToolCallID:  "call-split-2",
		ToolName:    "bash",
		Description: "Execute command: git commit && mkdir out && swift build",
		Action:      "execute",
		Path:        "/tmp",
		Subject:     "git,mkdir,swift",
		SubjectFull: "git commit,mkdir,swift build",
	}
	go func() {
		_, _ = service.Request(t.Context(), second)
	}()

	prompt := (<-events).Payload
	require.Equal(t, "git,mkdir,swift", prompt.Subject)
	require.Equal(t, "swift", prompt.SubjectNew, "only swift lacks a grant")

	service.Deny(prompt)
}

// TestPromptFullyUncoveredMarksAllNew proves a chain with no prior grants
// marks every binary new, so the dialog keeps its single-list display.
func TestPromptFullyUncoveredMarksAllNew(t *testing.T) {
	service := NewPermissionService("/tmp", false, []string{})

	req := CreatePermissionRequest{
		SessionID:   "fresh-session",
		ToolCallID:  "call-fresh",
		ToolName:    "bash",
		Description: "Execute command: mkdir out && swift build",
		Action:      "execute",
		Path:        "/tmp",
		Subject:     "mkdir,swift",
		SubjectFull: "mkdir,swift build",
	}

	events := service.Subscribe(t.Context())
	go func() {
		_, _ = service.Request(t.Context(), req)
	}()
	prompt := (<-events).Payload
	require.Equal(t, "mkdir,swift", prompt.SubjectNew)
	service.Deny(prompt)
}
