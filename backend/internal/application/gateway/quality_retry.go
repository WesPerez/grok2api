package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

const (
	ErrorQualityDegraded             = "quality_degraded"
	qualityRetryFailOpen             = "fail_open"
	qualityRetryFailClosed           = "fail_closed"
	defaultQualityMaxAttempts        = 6
	defaultQualityHoldTimeout        = 30 * time.Second
	defaultQualityMinOutput          = int64(8)
	defaultMinEncryptedBytes         = 256
	defaultMissingThinkingCooldown   = 12 * time.Hour
	lastErrorMissingThinking         = accountdomain.LastErrorMissingThinking
	lastErrorMissingThinkingDisabled = accountdomain.LastErrorMissingThinkingDisabled
	// An empty stream that idles while held is treated as an account-quality
	// failure: the request can still rotate before any bytes reach the client.
	qualityIdleAccountCooldown = 15 * time.Minute
)

var (
	errQualityDegraded    = errors.New("上游响应缺少推理")
	errQualityEmptyStream = errors.New("上游流式响应为空")
)

// QualityRetryRuntime is the isolated request-path withhold/retry policy.
// Zero Enabled leaves production behavior unchanged.
type QualityRetryRuntime struct {
	Enabled         bool
	MaxAttempts     int
	HoldTimeout     time.Duration
	MinOutputTokens int64
	OnExhausted     string
	AccountCooldown time.Duration
	// IdleAccountCooldown is applied to truly empty upstream streams
	// (idle timeout / empty peek). Missing-thinking still uses AccountCooldown.
	IdleAccountCooldown time.Duration
	MinEncryptedBytes   int
	// Request-local evidence; this is not a runtime setting. Tool-result
	// continuations may acknowledge a result without another reasoning item.
	toolResultContinuation bool
}

// QualityStreamSignals is the hold classifier input. Tests drive this
// directly and via ObserveQualityChunk on SSE fixtures.
type QualityStreamSignals struct {
	HasThinking            bool
	HasReasoningDelta      bool
	HasToolCall            bool
	ToolResultContinuation bool
	// ReasoningStarted is an empty reasoning item or the Chat SSE stub
	// `: grok2api-reasoning-start`. A marker alone contains no reasoning.
	ReasoningStarted bool
	VisibleTokens    int64
	ReasoningTokens  int64
	OutputTokens     int64
	EncryptedBytes   int
	EncryptedFloor   int64
	UsageReported    bool
	Terminal         bool
	TerminalFailure  bool
	HoldExpired      bool
}

// QualityVerdict is the hold decision for one upstream stream.
type QualityVerdict string

const (
	QualityWait     QualityVerdict = "wait"
	QualityDeliver  QualityVerdict = "deliver"
	QualityWithhold QualityVerdict = "withhold"
)

// QualityRetryAction is what the attempt loop does with a withhold verdict.
type QualityRetryAction string

const (
	QualityActionDeliver     QualityRetryAction = "deliver"
	QualityActionDeliverLast QualityRetryAction = "deliver_last"
	QualityActionRetry       QualityRetryAction = "retry"
	QualityActionReject      QualityRetryAction = "reject"
)

func normalizeQualityRetry(cfg QualityRetryRuntime) QualityRetryRuntime {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultQualityMaxAttempts
	}
	if cfg.HoldTimeout <= 0 {
		cfg.HoldTimeout = defaultQualityHoldTimeout
	}
	if cfg.MinOutputTokens <= 0 {
		cfg.MinOutputTokens = defaultQualityMinOutput
	}
	if cfg.AccountCooldown <= 0 {
		cfg.AccountCooldown = defaultMissingThinkingCooldown
	}
	if cfg.IdleAccountCooldown <= 0 {
		cfg.IdleAccountCooldown = qualityIdleAccountCooldown
	}
	if cfg.MinEncryptedBytes <= 0 {
		cfg.MinEncryptedBytes = defaultMinEncryptedBytes
	}
	cfg.OnExhausted = normalizeQualityExhaustionPolicy(cfg.OnExhausted)
	return cfg
}

func (s *Service) UpdateQualityRetry(cfg QualityRetryRuntime) {
	normalized := normalizeQualityRetry(cfg)
	s.qualityRetry.Store(&normalized)
}

func (s *Service) qualityRetryConfig() QualityRetryRuntime {
	if s == nil {
		return normalizeQualityRetry(QualityRetryRuntime{})
	}
	if value := s.qualityRetry.Load(); value != nil {
		return *value
	}
	return normalizeQualityRetry(QualityRetryRuntime{})
}

// ClassifyQualityHold decides whether a held stream may be forwarded.
// Transport batching and reasoning/output ratios do not establish response
// quality. The same SSE events must not quarantine an account merely because
// they arrive in one Read instead of several. Tool calls are useful output
// even when a forced call has no visible reasoning.
func ClassifyQualityHold(sig QualityStreamSignals, minOutput int64) QualityVerdict {
	if minOutput <= 0 {
		minOutput = defaultQualityMinOutput
	}
	if sig.TerminalFailure || sig.HasThinking || sig.HasReasoningDelta || sig.HasToolCall {
		return QualityDeliver
	}
	// Only actual visible content counts. Usage-only responses are handled as
	// empty streams by finishQualityPeek, not as missing-reasoning strikes.
	output := sig.VisibleTokens
	if sig.ToolResultContinuation && sig.Terminal && output > 0 {
		return QualityDeliver
	}
	if sig.Terminal {
		if output <= 0 {
			return QualityWait
		}
		if output >= minOutput {
			return QualityWithhold
		}
		return QualityDeliver
	}
	// Reasoning evidence and tool calls may arrive after initial text. Do not
	// reject an unfinished response based on missing terminal metadata. The
	// hold deadline bounds buffering once visible output is available; truly
	// empty streams remain covered by the provider's semantic idle timeout.
	if sig.HoldExpired && sig.VisibleTokens > 0 {
		return QualityDeliver
	}
	return QualityWait
}

// qualityPeekAbortError prefers the idle-timeout cause over a plain
// context.Canceled so the attempt loop can retry instead of treating the
// abort as a client 499.
func qualityPeekAbortError(ctx context.Context, err error) error {
	if ctx != nil {
		if cause := context.Cause(ctx); neterrorpkg.IsUpstreamStreamIdleTimeout(cause) {
			return cause
		}
	}
	if neterrorpkg.IsUpstreamStreamIdleTimeout(err) {
		return err
	}
	if err != nil {
		return err
	}
	if ctx != nil {
		return ctx.Err()
	}
	return nil
}

// isClientRequestCancel reports a real client disconnect. Upstream idle
// timeouts cancel the same context and must not be classified as 499.
func isClientRequestCancel(ctx context.Context, err error) bool {
	return neterrorpkg.IsClientRequestCancel(ctx, err)
}

// DecideQualityRetry caps withhold recovery at maxAttempts (default 6:
// original + five extra accounts). The last withhold
// (attemptIndex == maxAttempts-1) is fail-open unless OnExhausted is fail_closed.
func DecideQualityRetry(verdict QualityVerdict, attemptIndex, maxAttempts int, onExhausted string) QualityRetryAction {
	if verdict != QualityWithhold {
		return QualityActionDeliver
	}
	if maxAttempts <= 0 {
		maxAttempts = defaultQualityMaxAttempts
	}
	if attemptIndex < 0 {
		attemptIndex = 0
	}
	if attemptIndex < maxAttempts-1 {
		return QualityActionRetry
	}
	// attemptIndex == maxAttempts-1 (or past it): do not retry again.
	if normalizeQualityExhaustionPolicy(onExhausted) == qualityRetryFailClosed {
		return QualityActionReject
	}
	return QualityActionDeliverLast
}

// BoundQualityRetry turns a Retry into DeliverLast/Reject when the routing
// loop has no remaining account slot, so the already-held body is not dropped
// on continue-into-exhausted-loop.
func BoundQualityRetry(action QualityRetryAction, hasNextRoutingAttempt bool, onExhausted string) QualityRetryAction {
	if action != QualityActionRetry || hasNextRoutingAttempt {
		return action
	}
	if normalizeQualityExhaustionPolicy(onExhausted) == qualityRetryFailClosed {
		return QualityActionReject
	}
	return QualityActionDeliverLast
}

func normalizeQualityExhaustionPolicy(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), qualityRetryFailOpen) {
		return qualityRetryFailOpen
	}
	return qualityRetryFailClosed
}

// QualityCommit is the single attempt-loop decision for a held stream.
type QualityCommit struct {
	Action   QualityRetryAction
	Audit    bool
	KeepBody bool
}

// CommitQualityHold is the shipped withhold/retry/commit unit. The attempt
// loop must not re-derive this from Decide+Bound+switch.
func CommitQualityHold(verdict QualityVerdict, qualityAttempt, maxAttempts int, hasNextRouting bool, onExhausted string) QualityCommit {
	action := BoundQualityRetry(
		DecideQualityRetry(verdict, qualityAttempt, maxAttempts, onExhausted),
		hasNextRouting,
		onExhausted,
	)
	switch action {
	case QualityActionRetry, QualityActionReject:
		return QualityCommit{Action: action, Audit: true, KeepBody: false}
	case QualityActionDeliverLast:
		return QualityCommit{Action: action, Audit: false, KeepBody: true}
	default:
		return QualityCommit{Action: QualityActionDeliver, Audit: false, KeepBody: true}
	}
}

func shouldHoldQualityStream(input Input, ownership *inferencedomain.ResponseOwnership, route modeldomain.Route, operation audit.Operation, cfg QualityRetryRuntime) bool {
	if !cfg.Enabled || !input.Streaming || input.ForcedEgressNodeID != 0 || input.skipQualityHold {
		return false
	}
	switch operation {
	case audit.OperationChat, audit.OperationResponses, audit.OperationMessages, "":
	default:
		return false
	}
	// Context compaction is a system summary operation, not a normal reasoning
	// turn. Holding it can quarantine a healthy account for producing the
	// expected summary without streamed reasoning. Keep both compaction forms
	// excluded even if a caller reaches this gate without skipQualityHold.
	if isResponsesCompactionRequest(input.Body) {
		return false
	}
	if route.Provider != accountdomain.ProviderBuild && route.Provider != accountdomain.ProviderConsole {
		return false
	}
	// TUI commonly declares tools and follow-ups carry previous_response_id.
	// They still need quality classification, but replay safety is decided
	// separately: detecting a degraded response must not imply that an
	// account-bound or side-effecting request can run on another account.
	if qualityRequestDisablesReasoning(input.Body) {
		return false
	}
	if modeldomain.SupportsReasoningForProvider(route.Provider, input.PublicModel) {
		return true
	}
	return modeldomain.SupportsReasoningForProvider(route.Provider, route.UpstreamModel)
}

// canReplayQualityHoldAcrossAccounts separates response classification from
// retry authority. Stored Responses are account-bound, while hosted tools may
// already have produced an external side effect before their held response is
// rejected. Both may be held, audited, and penalized, but neither is replayed
// on another account.
func canReplayQualityHoldAcrossAccounts(input Input, ownership *inferencedomain.ResponseOwnership) bool {
	return ownership == nil && !qualityRequestHasReplayUnsafeHostedTools(input.Body)
}

// qualityRequestEndsWithToolResult only exempts the immediate tool-result
// continuation. A tool result earlier in the history must not exempt a later
// user request. Inspect protocol items, never arbitrary tool output/schema data.
func qualityRequestEndsWithToolResult(body []byte) bool {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	items, _ := payload["messages"].([]any)
	if raw, exists := payload["input"]; exists {
		items, _ = raw.([]any)
	}
	for index := len(items) - 1; index >= 0; index-- {
		item, ok := items[index].(map[string]any)
		if !ok {
			return false
		}
		switch jsonNodeString(item["type"]) {
		case "function_call_output", "custom_tool_call_output", "local_shell_call_output",
			"shell_call_output", "apply_patch_call_output", "mcp_tool_call_output",
			"tool_search_output", "computer_call_output", "tool_result":
			return true
		case "reasoning", "additional_tools":
			continue
		}
		switch jsonNodeString(item["role"]) {
		case "system", "developer":
			continue
		case "tool", "function":
			return true
		case "user":
			blocks, _ := item["content"].([]any)
			for _, rawBlock := range blocks {
				block, _ := rawBlock.(map[string]any)
				if jsonNodeString(block["type"]) != "tool_result" {
					return false
				}
			}
			return len(blocks) > 0
		}
		return false
	}
	return false
}

func qualityRequestHasReplayUnsafeHostedTools(body []byte) bool {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return false
	}
	if raw, exists := payload["web_search_options"]; exists && raw != nil {
		return true
	}
	if raw, exists := payload["mcp_servers"]; exists && raw != nil {
		servers, ok := raw.([]any)
		if !ok || len(servers) > 0 {
			return true
		}
	}
	if qualityToolListHasReplayUnsafeHostedTool(payload["tools"]) {
		return true
	}
	// Responses Tool Search can load declarations later in the request. Only
	// inspect additional_tools items; arbitrary user/schema objects may also
	// contain a field named "tools" and must not affect the retry policy.
	items, _ := payload["input"].([]any)
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok || jsonNodeString(item["type"]) != "additional_tools" {
			continue
		}
		if qualityToolListHasReplayUnsafeHostedTool(item["tools"]) {
			return true
		}
	}
	return false
}

func qualityToolListHasReplayUnsafeHostedTool(value any) bool {
	tools, ok := value.([]any)
	if !ok {
		return false
	}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		kind := jsonNodeString(tool["type"])
		switch kind {
		case "", "function", "custom", "local_shell", "apply_patch", "tool_search":
			// These declarations only ask the model to return a call. Execution
			// happens in the client after the held response is committed.
			continue
		case "shell":
			environment, _ := tool["environment"].(map[string]any)
			if jsonNodeString(environment["type"]) != "local" {
				return true
			}
		case "namespace":
			if qualityToolListHasReplayUnsafeHostedTool(tool["tools"]) {
				return true
			}
		default:
			// Default to no replay for every server/native tool, including types
			// added by future protocol versions that this gateway does not know yet.
			return true
		}
	}
	return false
}

func jsonNodeString(value any) string {
	text, _ := value.(string)
	return strings.ToLower(strings.TrimSpace(text))
}

func qualityRequestDisablesReasoning(body []byte) bool {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	if jsonStringEquals(payload["reasoning_effort"], modeldomain.ReasoningEffortNone) {
		return true
	}
	for _, key := range []string{"reasoning", "output_config", "thinking"} {
		var nested map[string]json.RawMessage
		if json.Unmarshal(payload[key], &nested) != nil {
			continue
		}
		if jsonStringEquals(nested["effort"], modeldomain.ReasoningEffortNone) || jsonStringEquals(nested["type"], "disabled") {
			return true
		}
		var budget int64
		if raw, ok := nested["budget_tokens"]; ok && json.Unmarshal(raw, &budget) == nil && budget == 0 {
			return true
		}
	}
	return jsonStringEquals(payload["thinking"], "disabled")
}

func jsonStringEquals(raw json.RawMessage, want string) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && strings.EqualFold(strings.TrimSpace(value), want)
}

func (s *Service) applyMissingThinkingPenalty(ctx context.Context, requestID string, credential accountdomain.Credential, cooldown time.Duration) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
	defer cancel()
	action, err := s.selector.markMissingThinking(writeCtx, credential, cooldown)
	if err != nil {
		s.logger.Error("quality_degraded_penalty_failed", "request_id", requestID, "account_id", credential.ID, "action", action, "error", err)
		return
	}
	switch action {
	case missingThinkingPenaltyDisabled:
		s.logger.Info("quality_degraded_disabled", "request_id", requestID, "account_id", credential.ID)
	case missingThinkingPenaltyCooled:
		s.logger.Info("quality_degraded_cooldown", "request_id", requestID, "account_id", credential.ID, "cooldown", cooldown.String())
	}
}

func (s *Service) recordQualityDegraded(ctx context.Context, base audit.Record, credential accountdomain.Credential, usage Usage, startedAt time.Time, trace *infraegress.Trace, provider accountdomain.Provider) {
	record := base
	record.EventID = newAuditEventID()
	accountID := credential.ID
	record.AccountID = &accountID
	record.AccountName = credential.Name
	record.StatusCode = http.StatusOK
	record.ErrorCode = ErrorQualityDegraded
	record.OutputTokens = usage.OutputTokens
	record.ReasoningTokens = usage.ReasoningTokens
	record.TotalTokens = usage.TotalTokens
	record.InputTokens = usage.InputTokens
	if usage.Reported {
		record.UsageSource = audit.UsageSourceUpstream
	}
	record.DurationMS = time.Since(startedAt).Milliseconds()
	record.CreatedAt = time.Now().UTC()
	applyAuditEgress(&record, trace, provider)
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
	defer cancel()
	if err := s.audits.Create(writeCtx, record); err != nil {
		s.logger.Error("quality_degraded_audit_failed", "event_id", record.EventID, "request_id", record.RequestID, "error", err)
	}
}
