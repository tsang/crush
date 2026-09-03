package permission

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/google/uuid"
)

// hookApprovalKey is the unexported context key used to mark a tool call as
// pre-approved by a PreToolUse hook. The value is the tool call ID so an
// approval can't be reused across calls that happen to share a context.
type hookApprovalKey struct{}

// WithHookApproval returns a context that marks the given tool call ID as
// pre-approved by a hook. When the permission service sees a matching
// request it short-circuits the normal prompt and grants immediately.
func WithHookApproval(ctx context.Context, toolCallID string) context.Context {
	return context.WithValue(ctx, hookApprovalKey{}, toolCallID)
}

// hookApproved reports whether the context carries a hook approval for the
// given tool call ID.
func hookApproved(ctx context.Context, toolCallID string) bool {
	if toolCallID == "" {
		return false
	}
	v, _ := ctx.Value(hookApprovalKey{}).(string)
	return v == toolCallID
}

type CreatePermissionRequest struct {
	SessionID   string `json:"session_id"`
	ToolCallID  string `json:"tool_call_id"`
	ToolName    string `json:"tool_name"`
	Description string `json:"description"`
	Action      string `json:"action"`
	Params      any    `json:"params"`
	Path        string `json:"path"`
	// Subject narrows a grant to a specific invocation of a tool beyond
	// Path. For bash it is the command binary (e.g. "git"); the
	// cmd+subcommand shape lives in SubjectFull. An allow-for-session
	// stores whichever tier the user approves, namespaced by
	// ScopeCmd/ScopeArgs.
	Subject string `json:"subject"`
	// SubjectFull is the command with its subcommand (e.g. "git commit"),
	// i.e. binary plus first non-flag argument. Grants storing the args
	// tier match only that cmd+subcommand shape.
	SubjectFull string `json:"subject_full,omitempty"`
}

type PermissionNotification struct {
	ToolCallID string `json:"tool_call_id"`
	Granted    bool   `json:"granted"`
	Denied     bool   `json:"denied"`
}

type PermissionRequest struct {
	ID          string `json:"id"`
	SessionID   string `json:"session_id"`
	ToolCallID  string `json:"tool_call_id"`
	ToolName    string `json:"tool_name"`
	Description string `json:"description"`
	Action      string `json:"action"`
	Params      any    `json:"params"`
	Path        string `json:"path"`
	// Subject mirrors CreatePermissionRequest.Subject.
	Subject string `json:"subject"`
	// SubjectFull mirrors CreatePermissionRequest.SubjectFull.
	SubjectFull string `json:"subject_full,omitempty"`
}

type Service interface {
	pubsub.Subscriber[PermissionRequest]
	// GrantPersistent grants a permission request and remembers the grant
	// for the session. It returns true if this call actually resolved the
	// pending request; false if the request had already been resolved
	// (e.g., by another concurrent caller) or is unknown.
	GrantPersistent(permission PermissionRequest) bool
	// Grant grants a permission request. It returns true if this call
	// actually resolved the pending request; false if the request had
	// already been resolved or is unknown.
	Grant(permission PermissionRequest) bool
	// Deny denies a permission request. It returns true if this call
	// actually resolved the pending request; false if the request had
	// already been resolved or is unknown.
	Deny(permission PermissionRequest) bool
	Request(ctx context.Context, opts CreatePermissionRequest) (bool, error)
	AutoApproveSession(sessionID string)
	SetSkipRequests(skip bool)
	SkipRequests() bool
	SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[PermissionNotification]
}

// PermissionKey is a composite key for session permission lookups.
type PermissionKey struct {
	SessionID string
	ToolName  string
	Action    string
	Path      string
	// Subject mirrors PermissionRequest.Subject. An empty Subject keeps the
	// pre-existing behavior of covering every tool action at the path.
	Subject string
}

type permissionService struct {
	*pubsub.Broker[PermissionRequest]

	notificationBroker    *pubsub.Broker[PermissionNotification]
	workingDir            string
	sessionPermissions    *csync.Map[PermissionKey, bool]
	pendingRequests       *csync.Map[string, chan bool]
	autoApproveSessions   map[string]bool
	autoApproveSessionsMu sync.RWMutex
	skip                  atomic.Bool
	allowedTools          []string
	// allowedToolsSource, when set, re-reads the allowed_tools list from
	// live config so grants persisted to the workspace config file take
	// effect without a restart.
	allowedToolsSource func() []string

	// used to make sure we only process one request at a time
	requestMu       sync.Mutex
	activeRequest   *PermissionRequest
	activeRequestMu sync.Mutex
}

// resolve atomically removes the pending request entry for the given
// permission and, if it was still pending, publishes exactly one
// PermissionNotification and forwards the outcome to the waiter on
// respCh. It returns true if this call resolved the request, false if
// it had already been resolved (e.g., by another concurrent caller) or
// the request ID is unknown.
//
// If onResolve is non-nil it runs after the pending entry has been
// taken but before the notification is published or the waiter is
// unblocked. This lets GrantPersistent record the session permission
// only when it actually wins the race, so a losing GrantPersistent
// that lost to a Deny does not leak an auto-approve entry.
//
// All three public resolution methods (Grant, GrantPersistent, Deny)
// route through this helper so multi-subscriber UIs can race safely:
// the first caller wins, the rest become no-ops.
func (s *permissionService) resolve(permission PermissionRequest, granted, denied bool, onResolve func()) bool {
	respCh, ok := s.pendingRequests.Take(permission.ID)
	if !ok {
		return false
	}

	if onResolve != nil {
		onResolve()
	}

	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		ToolCallID: permission.ToolCallID,
		Granted:    granted,
		Denied:     denied,
	})

	// respCh is buffered (cap 1) and only ever has at most one sender
	// per request because Take removes the entry under the map lock,
	// so this send never blocks.
	respCh <- granted

	s.activeRequestMu.Lock()
	if s.activeRequest != nil && s.activeRequest.ID == permission.ID {
		s.activeRequest = nil
	}
	s.activeRequestMu.Unlock()
	return true
}

// Grant scope tiers. A session grant stores its subject namespaced by the
// tier the user approved: ScopeCmd covers every invocation of the command
// binaries, ScopeArgs only the cmd+subcommand shapes the user saw in the
// dialog. Un-namespaced subjects are legacy grants matched verbatim
// (empty subject keeps the pre-tier tool+action+path behavior).
const (
	ScopeCmd  = "cmd:"
	ScopeArgs = "args:"
)

// ScopeUnknown marks a command whose binaries cannot be determined, e.g. a
// primary built by expansion ("$BIN run") or a command that fails to parse.
// Requests carrying it are never covered by a grant and never create one, so
// the user is asked every time rather than unknowingly approving something the
// scope derivation could not read.
const ScopeUnknown = "?"

// SubjectSeparator joins tokens in a composed subject, and MaxSubjectTokens
// bounds how many a single command may contribute. ValidToken keeps any
// token from containing the separator, so encoding a subject and splitting it
// again round-trips exactly. That matters now that each token in a cmd-tier
// grant becomes its own live session grant: a mis-split token such as `g++`
// arriving as `g` would silently widen what runs without prompting.
const (
	SubjectSeparator = ","
	MaxSubjectTokens = 32
)

// ValidToken reports whether s is safe to embed in a composed subject.
func ValidToken(s string) bool {
	return s != "" && !strings.ContainsAny(s, SubjectSeparator+" \t\n")
}

// JoinTokens sorts, dedupes, and joins tokens into a subject.
func JoinTokens(tokens []string) string {
	slices.Sort(tokens)
	return strings.Join(slices.Compact(tokens), SubjectSeparator)
}

// SplitSubject decodes a subject written by JoinTokens. It returns nil for
// the unknown marker so a "?" never decodes into a grantable token.
func SplitSubject(subject string) []string {
	if subject == "" || subject == ScopeUnknown {
		return nil
	}
	parts := strings.Split(subject, SubjectSeparator)
	tokens := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			tokens = append(tokens, part)
		}
	}
	return tokens
}

// ScopeSubject composes a stored subject from a tier prefix and the raw
// scope string.
func ScopeSubject(prefix, subject string) string { return prefix + subject }

// CutScopedEntry splits a scoped allow-list entry such as
// "bash:cmd:mkdir,touch" into its tool ("bash") and tier subject
// ("cmd:mkdir,touch"). Entries without a scope tier ("bash",
// "bash:execute") are not grants and stay in the tool-level early check.
func CutScopedEntry(entry string) (tool, subject string, ok bool) {
	tool, rest, ok := strings.Cut(entry, ":")
	if !ok || tool == "" {
		return "", "", false
	}
	for _, prefix := range []string{ScopeCmd, ScopeArgs} {
		if strings.HasPrefix(rest, prefix) && len(rest) > len(prefix) {
			return tool, rest, true
		}
	}
	return "", "", false
}

// FlattenScopedEntry splits a cmd-tier allow-list entry into one entry per
// binary, so a stored chain grant ("bash:cmd:cd,git") becomes flat entries
// ("bash:cmd:cd", "bash:cmd:git") that persist and review one command at a
// time. Args-tier entries are verbatim cmd+args shapes and stay whole;
// unscoped entries pass through unchanged.
func FlattenScopedEntry(entry string) []string {
	tool, subject, ok := CutScopedEntry(entry)
	if !ok {
		return []string{entry}
	}
	rest, isCmd := strings.CutPrefix(subject, ScopeCmd)
	if !isCmd {
		return []string{entry}
	}
	tokens := SplitSubject(rest)
	flat := make([]string, 0, len(tokens))
	for _, bin := range tokens {
		flat = append(flat, tool+":"+ScopeSubject(ScopeCmd, bin))
	}
	return flat
}

// CmdBinaryAllowed reports whether the binary named by a flat cmd-tier entry
// ("bash:cmd:git") is already covered by any cmd-tier entry in entries,
// joined or flat alike. Overlapping approvals use this so a re-approval of
// an already-allowed command does not grow the config.
func CmdBinaryAllowed(entries []string, entry string) bool {
	tool, subject, ok := CutScopedEntry(entry)
	if !ok {
		return false
	}
	bin, isCmd := strings.CutPrefix(subject, ScopeCmd)
	if !isCmd || !ValidToken(bin) {
		return false
	}
	for _, e := range entries {
		eTool, eSubject, ok := CutScopedEntry(e)
		if !ok || eTool != tool {
			continue
		}
		rest, isCmd := strings.CutPrefix(eSubject, ScopeCmd)
		if !isCmd {
			continue
		}
		if slices.Contains(SplitSubject(rest), bin) {
			return true
		}
	}
	return false
}

// configScopedGrantCovers reports whether scoped allowed_tools entries from
// config pre-approve the request. Cmd-tier binaries pool across every cmd
// entry: once each binary of the request appears in some approved entry the
// chain runs silently, so a combination of individually approved commands
// does not re-prompt. A binary that was never approved still prompts.
// Args-tier entries match the cmd+args shape verbatim, exactly like the
// in-session tiers. Config grants therefore survive restarts while the
// unknown-subject fail-closed rule still applies upstream.
func (s *permissionService) configScopedGrantCovers(permission PermissionRequest) bool {
	var allowedBins []string
	for _, entry := range s.scopedAllowedEntries() {
		tool, subject, ok := CutScopedEntry(entry)
		if !ok || tool != permission.ToolName {
			continue
		}
		if rest, isCmd := strings.CutPrefix(subject, ScopeCmd); isCmd {
			allowedBins = append(allowedBins, SplitSubject(rest)...)
			continue
		}
		if _, isArgs := strings.CutPrefix(subject, ScopeArgs); isArgs {
			if permission.SubjectFull != "" && subject == ScopeSubject(ScopeArgs, permission.SubjectFull) {
				return true
			}
		}
	}
	bins := SplitSubject(permission.Subject)
	if len(bins) == 0 || len(allowedBins) == 0 {
		return false
	}
	for _, bin := range bins {
		if !slices.Contains(allowedBins, bin) {
			return false
		}
	}
	return true
}

// scopedAllowedEntries returns the scoped entries from both the startup
// snapshot and, when wired, the live config source.
func (s *permissionService) scopedAllowedEntries() []string {
	if s.allowedToolsSource == nil {
		return s.allowedTools
	}
	return append(slices.Clone(s.allowedTools), s.allowedToolsSource()...)
}

// grantCovers reports whether any stored grant covers the incoming request.
// The legacy raw subject and the args tier are matched verbatim. The cmd
// tier is per-binary: every one must carry its own grant, in-session or in
// config, so a chain containing a binary that was never approved still
// prompts.
func (s *permissionService) grantCovers(permission PermissionRequest) bool {
	if permission.Subject == ScopeUnknown {
		return false
	}
	if s.configScopedGrantCovers(permission) {
		return true
	}
	key := PermissionKey{
		SessionID: permission.SessionID,
		ToolName:  permission.ToolName,
		Action:    permission.Action,
		Path:      permission.Path,
	}
	if _, ok := s.sessionPermissions.Get(key.WithSubject(permission.Subject)); ok {
		return true
	}
	if permission.SubjectFull != "" {
		if _, ok := s.sessionPermissions.Get(key.WithSubject(ScopeSubject(ScopeArgs, permission.SubjectFull))); ok {
			return true
		}
	}
	bins := SplitSubject(permission.Subject)
	if len(bins) == 0 {
		return false
	}
	if _, ok := s.sessionPermissions.Get(key.WithSubject(ScopeSubject(ScopeCmd, permission.Subject))); ok {
		return true
	}
	for _, bin := range bins {
		if _, ok := s.sessionPermissions.Get(key.WithSubject(ScopeSubject(ScopeCmd, bin))); !ok {
			return false
		}
	}
	return true
}

// WithSubject returns a copy of the key scoped to a single subject, so
// coverage can probe several tiers without repeating the identity fields.
func (k PermissionKey) WithSubject(subject string) PermissionKey {
	k.Subject = subject
	return k
}

func (s *permissionService) GrantPersistent(permission PermissionRequest) bool {
	// An unknown subject means the scope derivation could not read the
	// command, so there is nothing meaningful to remember: resolving it
	// grants this call only.
	if permission.Subject == ScopeUnknown {
		return s.Grant(permission)
	}
	// Record the persistent grant only if this call wins the
	// pending-request race. Otherwise a losing GrantPersistent that
	// lost to a Deny would still leave an auto-approve entry behind,
	// silently flipping later denied calls to allowed.
	return s.resolve(permission, true, false, func() {
		key := PermissionKey{
			SessionID: permission.SessionID,
			ToolName:  permission.ToolName,
			Action:    permission.Action,
			Path:      permission.Path,
			Subject:   permission.Subject,
		}
		s.sessionPermissions.Set(key, true)
		if _, ok := strings.CutPrefix(permission.Subject, ScopeCmd); !ok {
			return
		}
		// Fan the cmd tier out one key per binary, so approving a,b,c
		// also covers any subset of those binaries. A binary that was
		// never granted has no key of its own and still prompts.
		for _, bin := range SplitSubject(strings.TrimPrefix(permission.Subject, ScopeCmd)) {
			s.sessionPermissions.Set(key.WithSubject(ScopeSubject(ScopeCmd, bin)), true)
		}
	})
}

func (s *permissionService) Grant(permission PermissionRequest) bool {
	return s.resolve(permission, true, false, nil)
}

func (s *permissionService) Deny(permission PermissionRequest) bool {
	return s.resolve(permission, false, true, nil)
}

func (s *permissionService) Request(ctx context.Context, opts CreatePermissionRequest) (bool, error) {
	if s.skip.Load() {
		return true, nil
	}

	// Check if the tool/action combination is in the allowlist
	commandKey := opts.ToolName + ":" + opts.Action
	if slices.Contains(s.allowedTools, commandKey) || slices.Contains(s.allowedTools, opts.ToolName) {
		return true, nil
	}

	// A PreToolUse hook that returned decision=allow stamps the context
	// with the tool call ID. Treat that as a pre-approval and skip the
	// prompt entirely. We still publish a granted notification so the UI
	// and audit subscribers see the outcome.
	if hookApproved(ctx, opts.ToolCallID) {
		s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return true, nil
	}

	s.requestMu.Lock()
	defer s.requestMu.Unlock()

	// tell the UI that a permission was requested
	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		ToolCallID: opts.ToolCallID,
	})

	s.autoApproveSessionsMu.RLock()
	autoApprove := s.autoApproveSessions[opts.SessionID]
	s.autoApproveSessionsMu.RUnlock()

	if autoApprove {
		s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return true, nil
	}

	fileInfo, err := os.Stat(opts.Path)
	dir := opts.Path
	if err == nil {
		if fileInfo.IsDir() {
			dir = opts.Path
		} else {
			dir = filepath.Dir(opts.Path)
		}
	}

	if dir == "." {
		dir = s.workingDir
	}
	permission := PermissionRequest{
		ID:          uuid.New().String(),
		Path:        dir,
		SessionID:   opts.SessionID,
		ToolCallID:  opts.ToolCallID,
		ToolName:    opts.ToolName,
		Description: opts.Description,
		Action:      opts.Action,
		Params:      opts.Params,
		Subject:     opts.Subject,
		SubjectFull: opts.SubjectFull,
	}

	if s.grantCovers(permission) {
		s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return true, nil
	}

	s.activeRequestMu.Lock()
	s.activeRequest = &permission
	s.activeRequestMu.Unlock()

	respCh := make(chan bool, 1)
	s.pendingRequests.Set(permission.ID, respCh)
	defer s.pendingRequests.Del(permission.ID)

	// Publish the request
	s.Publish(pubsub.CreatedEvent, permission)

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case granted := <-respCh:
		return granted, nil
	}
}

func (s *permissionService) AutoApproveSession(sessionID string) {
	s.autoApproveSessionsMu.Lock()
	s.autoApproveSessions[sessionID] = true
	s.autoApproveSessionsMu.Unlock()
}

func (s *permissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[PermissionNotification] {
	return s.notificationBroker.Subscribe(ctx)
}

func (s *permissionService) SetSkipRequests(skip bool) {
	s.skip.Store(skip)
}

func (s *permissionService) SkipRequests() bool {
	return s.skip.Load()
}

func NewPermissionService(workingDir string, skip bool, allowedTools []string, opts ...Option) Service {
	svc := &permissionService{
		Broker:              pubsub.NewBroker[PermissionRequest](),
		notificationBroker:  pubsub.NewBroker[PermissionNotification](),
		workingDir:          workingDir,
		sessionPermissions:  csync.NewMap[PermissionKey, bool](),
		autoApproveSessions: make(map[string]bool),
		allowedTools:        allowedTools,
		pendingRequests:     csync.NewMap[string, chan bool](),
	}
	for _, opt := range opts {
		opt(svc)
	}
	svc.skip.Store(skip)
	return svc
}

// Option customizes a permission service at construction.
type Option func(*permissionService)

// WithAllowedToolsSource wires a live view of the config allow-list into
// the service. Scoped entries (see CutScopedEntry) persisted to the
// workspace config are re-checked on every permission lookup, so grants
// approved in a previous session — and edits made while running — apply
// immediately without a restart.
func WithAllowedToolsSource(fn func() []string) Option {
	return func(s *permissionService) { s.allowedToolsSource = fn }
}
