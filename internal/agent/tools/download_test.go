package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDownloadPermissionSubject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want string
	}{
		{"host lowercased", "https://Example.COM/dir/file.zip", "example.com"},
		{"port stripped", "https://example.com:8443/x.bin", "example.com"},
		{"subdomain kept", "http://files.example.org/a.txt", "files.example.org"},
		{"userinfo ignored", "https://user:pw@example.com/a", "example.com"},
		{"ipv6 host", "http://[::1]:8080/a.bin", "::1"},
		{"no host", "http://", permission.ScopeUnknown},
		{"unparseable", "https://ex ample/x", permission.ScopeUnknown},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, downloadPermissionSubject(tc.url))
		})
	}
}

// TestDownloadDomainGrantAutoApproves proves the end-to-end wiring: the
// first download prompts and carries its host as the grant subject, and a
// session approval for that host silently covers later downloads of other
// files from the same domain, while a new domain still prompts.
func TestDownloadDomainGrantAutoApproves(t *testing.T) {
	t.Parallel()

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		fmt.Fprint(w, "payload")
	}))
	defer srv.Close()

	workingDir := t.TempDir()
	perms := permission.NewPermissionService(workingDir, false, nil)
	tool := NewDownloadTool(perms, workingDir, srv.Client())

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "dl-session")
	events := perms.Subscribe(ctx)

	run := func(url, dest, callID string) (string, error) {
		resp, err := tool.Run(ctx, fantasy.ToolCall{
			ID:    callID,
			Name:  DownloadToolName,
			Input: `{"url":"` + url + `","file_path":"` + dest + `"}`,
		})
		return resp.Content, err
	}

	// First download: prompts with the host as subject.
	done := make(chan error, 1)
	go func() {
		_, err := run(srv.URL+"/first.txt", "first.txt", "call-1")
		done <- err
	}()

	prompt := (<-events).Payload
	require.Equal(t, DownloadToolName, prompt.ToolName)
	require.Equal(t, "call-1", prompt.ToolCallID)
	require.Equal(t, downloadPermissionSubject(srv.URL), prompt.Subject)
	require.True(t, perms.GrantPersistent(prompt), "first prompt should be grantable")
	require.NoError(t, <-done)

	// Second download, different file, same domain: no prompt, no block.
	res, err := run(srv.URL+"/second.txt", "second.txt", "call-2")
	require.NoError(t, err)
	require.Contains(t, res, "Successfully downloaded")
	select {
	case ev := <-events:
		t.Fatalf("same-domain download should not prompt, got %+v", ev.Payload)
	case <-time.After(200 * time.Millisecond):
	}

	// Another domain must still prompt. The .invalid TLD never resolves,
	// so the prompt is denied before any connection is attempted.
	go func() {
		_, _ = run("http://unexistent.invalid/other.bin", "other.bin", "call-3")
	}()
	select {
	case ev := <-events:
		require.Equal(t, "call-3", ev.Payload.ToolCallID)
		require.Equal(t, "unexistent.invalid", ev.Payload.Subject)
		perms.Deny(ev.Payload)
	case <-time.After(3 * time.Second):
		t.Fatal("different domain should have prompted")
	}

	require.FileExists(t, filepath.Join(workingDir, "first.txt"))
	require.FileExists(t, filepath.Join(workingDir, "second.txt"))
}
