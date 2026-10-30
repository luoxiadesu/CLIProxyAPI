package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	workbuddyauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/workbuddy"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/tiktoken-go/tokenizer"
)

// WorkBuddy outbound identity constants. They mirror the official desktop
// client shape used by the upstream risk-control checks.
const (
	workbuddyProviderKey   = "workbuddy"
	workBuddyClientVersion = "5.5.4"
	workBuddyCLIVersion    = "2.137.1"
	workBuddyUserAgent     = "WorkBuddy/" + workBuddyClientVersion + " WorkBuddy/" + workBuddyClientVersion + " CLI/" + workBuddyCLIVersion
	workBuddyOrigin        = "https://www.codebuddy.cn"
	workBuddyAgentIntent   = "craft"
	workBuddyProductName   = "WorkBuddy"
)

// WorkBuddyExecutor implements the cliproxyauth.ProviderExecutor for WorkBuddy
// (CodeBuddy CN) OAuth credentials.
type WorkBuddyExecutor struct {
	cfg *config.Config
}

var _ cliproxyauth.ProviderExecutor = (*WorkBuddyExecutor)(nil)

// NewWorkBuddyExecutor constructs a WorkBuddy executor.
func NewWorkBuddyExecutor(cfg *config.Config) *WorkBuddyExecutor {
	return &WorkBuddyExecutor{cfg: cfg}
}

// Identifier returns the provider identifier "workbuddy".
func (e *WorkBuddyExecutor) Identifier() string { return workbuddyProviderKey }

// RequestToFormat reports the upstream request format used after auth selection.
func (e *WorkBuddyExecutor) RequestToFormat(_ cliproxyexecutor.Request, _ cliproxyexecutor.Options) sdktranslator.Format {
	return sdktranslator.FormatOpenAI
}

// PrepareRequest injects WorkBuddy credentials and identity headers.
func (e *WorkBuddyExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	e.applyHeaders(req, auth, strings.TrimSpace(req.Header.Get("X-Model-ID")), nil)
	return nil
}

// HttpRequest injects WorkBuddy credentials into the request and executes it.
func (e *WorkBuddyExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("workbuddy executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if errPrepare := e.PrepareRequest(httpReq, auth); errPrepare != nil {
		return nil, errPrepare
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// Execute performs a non-streaming chat completion request. WorkBuddy only
// accepts streaming requests, so the upstream call is streamed and aggregated
// back into a chat.completion payload.
func (e *WorkBuddyExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	body, upstreamModel, _, errBuild := e.buildRequestPayload(ctx, req, opts, true)
	if errBuild != nil {
		return resp, errBuild
	}
	reporter.SetUpstreamModel(upstreamModel)

	url := workBuddyChatURL(workBuddyCreds(auth).BaseURL)
	httpReq, errNew := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if errNew != nil {
		return resp, errNew
	}
	e.applyHeaders(httpReq, auth, upstreamModel, opts.Headers)
	e.recordRequest(ctx, auth, url, httpReq.Header, body)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return resp, errDo
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("workbuddy executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = workBuddyStatusError(httpResp.StatusCode, httpResp.Header, b)
		return resp, err
	}

	data, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errRead)
		return resp, errRead
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)

	// WorkBuddy answers chat requests with SSE, but a proxy in front of it may
	// return a plain JSON body with a 2xx status. A chat.completion payload is
	// usable as-is; anything else is an error envelope.
	var aggregated []byte
	trimmedData := bytes.TrimSpace(data)
	if len(trimmedData) > 0 && (trimmedData[0] == '{' || trimmedData[0] == '[') && gjson.ValidBytes(trimmedData) {
		if gjson.GetBytes(trimmedData, "choices").Exists() {
			aggregated = bytes.Clone(trimmedData)
		} else {
			err = workBuddyStatusError(http.StatusOK, httpResp.Header, data)
			return resp, err
		}
	}
	if aggregated == nil {
		aggregated, err = helps.AggregateWorkBuddyStream(bytes.NewReader(data), helps.WorkBuddyAggregateOptions{
			AllowedTools:      helps.WorkBuddyToolNameAllowlist(body),
			AllowUnknownTools: true,
		})
		if err != nil {
			err = workBuddyAggregateError(err, httpResp.Header)
			return resp, err
		}
	}
	reporter.ObserveResponseModel(aggregated)

	to := sdktranslator.FromString("openai")
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, helps.ApplyPatchOriginalRequest(req, opts), body, aggregated, &param)
	if helps.ApplyPatchTranslationError(param) != nil || len(out) == 0 {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage}
	}
	reporter.Publish(ctx, helps.ParseOpenAIUsage(aggregated))
	if responseFormat == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
}

// CountTokens estimates prompt tokens locally. WorkBuddy has no token-count
// endpoint, so the request is built exactly as for a chat call and measured with
// an OpenAI tokenizer.
func (e *WorkBuddyExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	body, _, _, errBuild := e.buildRequestPayload(ctx, req, opts, false)
	if errBuild != nil {
		return cliproxyexecutor.Response{}, errBuild
	}
	enc, errTokenizer := tokenizer.Get(tokenizer.O200kBase)
	if errTokenizer != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("workbuddy executor: tokenizer init failed: %w", errTokenizer)
	}
	count, errCount := helps.CountOpenAIChatTokens(enc, body)
	if errCount != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("workbuddy executor: token counting failed: %w", errCount)
	}
	translated := sdktranslator.TranslateTokenCount(
		ctx,
		sdktranslator.FromString("openai"),
		cliproxyexecutor.ResponseFormatOrSource(opts),
		count,
		helps.BuildOpenAIUsageJSON(count),
	)
	return cliproxyexecutor.Response{Payload: translated}, nil
}

// Refresh exchanges the WorkBuddy refresh token for a new access token.
func (e *WorkBuddyExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("workbuddy executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, statusErr{code: http.StatusInternalServerError, msg: "workbuddy executor: auth is nil"}
	}
	refreshToken := workBuddyRefreshToken(auth)
	if refreshToken == "" {
		return auth, nil
	}
	creds := workBuddyCreds(auth)
	client := workbuddyauth.NewClient(e.cfg, auth.ProxyURL)
	token, errRefresh := client.Refresh(ctx, refreshToken, workbuddyauth.RefreshIdentity{
		UID:          creds.UID,
		EnterpriseID: creds.EnterpriseID,
		Domain:       creds.Domain,
	})
	if errRefresh != nil {
		if errors.Is(errRefresh, workbuddyauth.ErrRefreshRejected) {
			return nil, statusErr{code: http.StatusUnauthorized, msg: "workbuddy executor: refresh token rejected", credentialScoped: true}
		}
		log.Warnf("workbuddy executor: refresh failed: %v", errRefresh)
		return nil, statusErr{code: http.StatusBadGateway, msg: "workbuddy executor: refresh failed"}
	}

	now := time.Now().UTC()
	applyWorkBuddyRefreshToken(auth, token, now, creds)
	auth.LastRefreshedAt = time.Now()
	return auth, nil
}

// workBuddyFallbackRefreshLifetime is used when a refresh response carries no
// expiry information. Without a fresh timestamp the manager keeps seeing the
// stale expired value and refreshes on every scheduling pass.
const workBuddyFallbackRefreshLifetime = time.Hour

// applyWorkBuddyRefreshToken writes a refreshed token into the credential. It
// never leaves a stale expiry behind: a response without expiresAt/expiresIn
// gets a conservative one-hour lifetime.
func applyWorkBuddyRefreshToken(auth *cliproxyauth.Auth, token *workbuddyauth.TokenData, now time.Time, creds workBuddyCredentials) {
	if auth == nil || token == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	expired := workBuddyTokenExpiry(token, now)
	if expired == "" {
		expired = now.Add(workBuddyFallbackRefreshLifetime).Format(time.RFC3339)
	}

	auth.Metadata["access_token"] = token.AccessToken
	if token.RefreshToken != "" {
		auth.Metadata["refresh_token"] = token.RefreshToken
	}
	if token.TokenType != "" {
		auth.Metadata["token_type"] = token.TokenType
	}
	if token.ExpiresIn > 0 {
		auth.Metadata["expires_in"] = token.ExpiresIn
	}
	auth.Metadata["expired"] = expired
	if domain := strings.TrimSpace(token.Domain); domain != "" {
		auth.Metadata["domain"] = domain
	}
	if currentType, ok := auth.Metadata["type"].(string); !ok || strings.TrimSpace(currentType) == "" {
		auth.Metadata["type"] = workbuddyauth.Provider
	}
	auth.Metadata["last_refresh"] = now.Format(time.RFC3339)

	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	if strings.TrimSpace(auth.Attributes["base_url"]) == "" {
		auth.Attributes["base_url"] = creds.BaseURL
	}
	if strings.TrimSpace(auth.Attributes["domain"]) == "" {
		auth.Attributes["domain"] = creds.Domain
	}

	if storage, ok := auth.Storage.(*workbuddyauth.TokenStorage); ok && storage != nil {
		updated := *storage
		updated.AccessToken = token.AccessToken
		if token.RefreshToken != "" {
			updated.RefreshToken = token.RefreshToken
		}
		if token.TokenType != "" {
			updated.TokenType = token.TokenType
		}
		if token.ExpiresIn > 0 {
			updated.ExpiresIn = token.ExpiresIn
		}
		updated.Expired = expired
		if domain := strings.TrimSpace(token.Domain); domain != "" {
			updated.Domain = domain
		}
		updated.LastRefresh = now.Format(time.RFC3339)
		auth.Storage = &updated
	}
}

// buildRequestPayload translates and normalizes the inbound request into the
// WorkBuddy upstream payload. The user payload finalizer is always the last
// mutation, and no caller may modify the business payload afterwards.
func (e *WorkBuddyExecutor) buildRequestPayload(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, upstreamStream bool) ([]byte, string, []byte, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")

	originalSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalSource = opts.OriginalRequest
	}
	originalTranslated := helps.TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, e.cfg, from, to, baseModel, bytes.Clone(originalSource), upstreamStream)

	body, errTranslate := helps.TranslateRequestReturningError(ctx, opts.Headers, e.cfg, from, to, baseModel, bytes.Clone(req.Payload), upstreamStream, false)
	if errTranslate != nil {
		return nil, "", nil, errTranslate
	}

	upstreamModel := normalizeWorkBuddyUpstreamModel(baseModel)
	body, errSetModel := sjson.SetBytes(body, "model", upstreamModel)
	if errSetModel != nil {
		return nil, "", nil, fmt.Errorf("workbuddy executor: failed to set model in payload: %w", errSetModel)
	}

	body, errThinking := helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier())
	if errThinking != nil {
		return nil, "", nil, errThinking
	}

	// Order matters: normalize protocol shapes before pairing repair, then force
	// the streaming contract, and finalize user rules exactly once at the end.
	body = helps.NormalizeOpenAIMaxTokens(body, false)
	body = helps.NormalizeWorkBuddyRoles(body)
	body = helps.NormalizeWorkBuddyToolChoice(body)
	body = helps.NormalizeWorkBuddyImageParts(body)
	body = helps.RepairWorkBuddyToolCallPairs(body)
	if upstreamStream {
		body = helps.SetBoolIfDifferent(body, "stream", true)
		body = helps.SetBoolIfDifferent(body, "stream_options.include_usage", true)
	}

	finalize := helps.NewPayloadFinalizer(e.cfg, e.Identifier(), baseModel, to.String(), "", originalTranslated, req, opts)
	body = finalize(body)
	return body, upstreamModel, originalTranslated, nil
}

// applyHeaders writes the WorkBuddy identity, attribution, and conversation
// header families. sourceHeaders carries the inbound client headers used to
// derive stable conversation identifiers.
func (e *WorkBuddyExecutor) applyHeaders(req *http.Request, auth *cliproxyauth.Auth, model string, sourceHeaders http.Header) {
	if req == nil {
		return
	}
	creds := workBuddyCreds(auth)
	header := req.Header
	header.Set("Content-Type", "application/json")
	// Chat is always streamed upstream, regardless of what the client asked for.
	header.Set("Accept", "application/json, text/event-stream")
	header.Set("X-Requested-With", "XMLHttpRequest")
	header.Set("Origin", workBuddyOrigin)
	header.Set("Referer", workBuddyOrigin+"/")
	header.Set("User-Agent", workBuddyUserAgent)
	header.Set("Accept-Language", "zh-CN")
	header.Set("X-CodeBuddy-Request", "1")

	if creds.AccessToken != "" {
		header.Set("Authorization", "Bearer "+creds.AccessToken)
	} else {
		header.Set("X-No-Authorization", "true")
	}
	if creds.UID != "" {
		header.Set("X-User-Id", creds.UID)
	} else {
		header.Set("X-No-User-Id", "true")
	}
	if creds.EnterpriseID != "" {
		header.Set("X-Enterprise-Id", creds.EnterpriseID)
	} else {
		header.Set("X-No-Enterprise-Id", "true")
	}
	if creds.Domain != "" {
		header.Set("X-Domain", creds.Domain)
	}
	if creds.Department != "" {
		header.Set("X-Department-Info", creds.Department)
		header.Del("X-No-Department-Info")
	} else {
		header.Set("X-No-Department-Info", "true")
	}
	if creds.UID != "" {
		header.Set("X-Machine-ID", workBuddyDeriveStableID(creds.UID, "machine"))
		header.Set("X-Session-ID", workBuddyDeriveStableID(creds.UID, "session"))
	}
	if creds.DeviceToken != "" {
		header.Set("X-Device-Token", creds.DeviceToken)
	}

	header.Set("X-Agent-Intent", workBuddyAgentIntent)
	if model != "" {
		header.Set("X-Model-ID", model)
	}
	header.Set("X-IDE-Name", workBuddyProductName)
	header.Set("X-IDE-Type", workBuddyProductName)
	header.Set("X-IDE-Version", workBuddyClientVersion)
	header.Set("X-Product", workBuddyProductName)
	header.Set("X-Product-Version", workBuddyClientVersion)

	header.Set("X-Request-Trace-Id", workBuddyRandomHex(16))
	applyWorkBuddyConversationHeaders(header, sourceHeaders)

	nowMillis := strconv.FormatInt(time.Now().UnixMilli(), 10)
	header.Set("monitor_promptPrepareStartTime", nowMillis)
	header.Set("monitor_httpSendTime", nowMillis)

	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs, sourceHeaders)
}

// applyWorkBuddyConversationHeaders derives the conversation/request/trace
// header family. Inbound identifiers are reused when present; otherwise a fresh
// 32-hex value is generated.
func applyWorkBuddyConversationHeaders(header http.Header, source http.Header) {
	conversationID := firstWorkBuddyHeader(source, "X-Conversation-ID")
	if conversationID == "" {
		conversationID = workBuddyRandomHex(16)
	}
	conversationRequestID := firstWorkBuddyHeader(source, "X-Conversation-Request-ID", "X-Request-ID", "X-Session-Id", "X-Session-ID")
	if conversationRequestID == "" {
		conversationRequestID = workBuddyRandomHex(16)
	}
	messageID := conversationRequestID

	header.Set("X-Conversation-ID", conversationID)
	header.Set("X-Conversation-Request-ID", conversationRequestID)
	header.Set("X-Conversation-Message-ID", messageID)
	header.Set("X-Request-ID", messageID)
	header.Set("X-Root-Request-ID", conversationRequestID)
	header.Set("X-Trace-ID", conversationRequestID)

	b3TraceID := conversationRequestID
	if !workBuddyValidTraceID(b3TraceID) {
		b3TraceID = workBuddyRandomHex(16)
	}
	spanID := workBuddyRandomHex(8)
	header.Set("X-B3-TraceId", b3TraceID)
	header.Set("X-B3-SpanId", spanID)
	header.Set("X-B3-Sampled", "1")
	header.Set("b3", b3TraceID+"-"+spanID+"-1")
}

// recordRequest writes the structured upstream request log entry.
func (e *WorkBuddyExecutor) recordRequest(ctx context.Context, auth *cliproxyauth.Auth, url string, headers http.Header, body []byte) {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   headers.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
}

// workBuddyCredentials is the credential view used to build one request.
type workBuddyCredentials struct {
	AccessToken  string
	BaseURL      string
	Domain       string
	UID          string
	EnterpriseID string
	Department   string
	DeviceToken  string
}

// workBuddyCreds resolves credentials with Attributes → Metadata → Storage
// precedence (Attributes wins). Endpoint and identity fields keep their defaults
// when absent.
func workBuddyCreds(auth *cliproxyauth.Auth) workBuddyCredentials {
	creds := workBuddyCredentials{
		BaseURL: workbuddyauth.ChatBaseURL,
		Domain:  workbuddyauth.Domain,
	}
	if auth == nil {
		return creds
	}
	// Apply from lowest to highest precedence so a higher source overrides.
	if storage, ok := auth.Storage.(*workbuddyauth.TokenStorage); ok && storage != nil {
		creds.AccessToken = workBuddyFirstNonEmpty(storage.AccessToken, creds.AccessToken)
		creds.BaseURL = workBuddyFirstNonEmpty(storage.BaseURL, creds.BaseURL)
		creds.Domain = workBuddyFirstNonEmpty(storage.Domain, creds.Domain)
		creds.UID = workBuddyFirstNonEmpty(storage.UID, creds.UID)
		creds.EnterpriseID = workBuddyFirstNonEmpty(storage.EnterpriseID, creds.EnterpriseID)
		creds.Department = workBuddyFirstNonEmpty(storage.Department, creds.Department)
		creds.DeviceToken = workBuddyFirstNonEmpty(storage.DeviceToken, creds.DeviceToken)
	}
	if auth.Metadata != nil {
		creds.AccessToken = workBuddyFirstNonEmpty(workBuddyMetadataString(auth.Metadata, "access_token"), creds.AccessToken)
		creds.BaseURL = workBuddyFirstNonEmpty(workBuddyMetadataString(auth.Metadata, "base_url"), creds.BaseURL)
		creds.Domain = workBuddyFirstNonEmpty(workBuddyMetadataString(auth.Metadata, "domain"), creds.Domain)
		creds.UID = workBuddyFirstNonEmpty(workBuddyMetadataString(auth.Metadata, "uid"), creds.UID)
		creds.EnterpriseID = workBuddyFirstNonEmpty(workBuddyMetadataString(auth.Metadata, "enterprise_id"), creds.EnterpriseID)
		creds.Department = workBuddyFirstNonEmpty(workBuddyMetadataString(auth.Metadata, "department"), creds.Department)
		creds.DeviceToken = workBuddyFirstNonEmpty(workBuddyMetadataString(auth.Metadata, "device_token"), creds.DeviceToken)
	}
	if auth.Attributes != nil {
		creds.AccessToken = workBuddyFirstNonEmpty(auth.Attributes["access_token"], creds.AccessToken)
		creds.BaseURL = workBuddyFirstNonEmpty(auth.Attributes["base_url"], creds.BaseURL)
		creds.Domain = workBuddyFirstNonEmpty(auth.Attributes["domain"], creds.Domain)
		creds.UID = workBuddyFirstNonEmpty(auth.Attributes["uid"], creds.UID)
		creds.EnterpriseID = workBuddyFirstNonEmpty(auth.Attributes["enterprise_id"], creds.EnterpriseID)
		creds.Department = workBuddyFirstNonEmpty(auth.Attributes["department"], creds.Department)
		creds.DeviceToken = workBuddyFirstNonEmpty(auth.Attributes["device_token"], creds.DeviceToken)
	}
	return creds
}

// workBuddyRefreshToken returns the stored refresh token.
func workBuddyRefreshToken(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if token := workBuddyMetadataString(auth.Metadata, "refresh_token"); token != "" {
			return token
		}
	}
	if storage, ok := auth.Storage.(*workbuddyauth.TokenStorage); ok && storage != nil {
		return strings.TrimSpace(storage.RefreshToken)
	}
	return ""
}

// workBuddyMetadataString reads a trimmed string from auth metadata.
func workBuddyMetadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

// workBuddyChatURL joins the credential base URL with the chat path.
func workBuddyChatURL(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = workbuddyauth.ChatBaseURL
	}
	return strings.TrimRight(baseURL, "/") + workbuddyauth.ChatPath
}

// normalizeWorkBuddyUpstreamModel strips the routing prefix and context suffix
// that must not reach the upstream.
func normalizeWorkBuddyUpstreamModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	if len(model) >= len("workbuddy/") && strings.EqualFold(model[:len("workbuddy/")], "workbuddy/") {
		model = model[len("workbuddy/"):]
	}
	if open := strings.LastIndex(model, "["); open > 0 && strings.HasSuffix(model, "]") {
		model = model[:open]
	}
	return strings.TrimSpace(model)
}

// workBuddyTokenExpiry renders the access-token expiry timestamp.
func workBuddyTokenExpiry(token *workbuddyauth.TokenData, now time.Time) string {
	if token == nil {
		return ""
	}
	if token.ExpiresAt > 0 {
		return time.Unix(token.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	if token.ExpiresIn > 0 {
		return now.Add(time.Duration(token.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	return ""
}

// workBuddyDeriveStableID derives a stable 36-hex account identifier. It stays
// constant across restarts and differs per account and purpose.
func workBuddyDeriveStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18])
}

// workBuddyRandomHex returns n random bytes hex-encoded.
func workBuddyRandomHex(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	if _, errRead := rand.Read(buf); errRead == nil {
		return hex.EncodeToString(buf)
	}
	// Keep the header well-formed even if the entropy source fails.
	sum := sha256.Sum256([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
	if n > sha256.Size {
		n = sha256.Size
	}
	return hex.EncodeToString(sum[:n])
}

// workBuddyValidTraceID reports whether s is a valid B3 trace id (16 or 32 hex).
func workBuddyValidTraceID(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

// firstWorkBuddyHeader returns the first non-empty trimmed value among the
// named headers.
func firstWorkBuddyHeader(header http.Header, names ...string) string {
	if header == nil {
		return ""
	}
	for _, name := range names {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

// workBuddyFirstNonEmpty returns value when trimmed non-empty, otherwise fallback.
func workBuddyFirstNonEmpty(value, fallback string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(fallback)
}
