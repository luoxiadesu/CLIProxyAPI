package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// ExecuteStream performs a streaming chat completion request to WorkBuddy.
func (e *WorkBuddyExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	body, upstreamModel, _, errBuild := e.buildRequestPayload(ctx, req, opts, true)
	if errBuild != nil {
		return nil, errBuild
	}
	reporter.SetUpstreamModel(upstreamModel)

	url := workBuddyChatURL(workBuddyCreds(auth).BaseURL)
	httpReq, errNew := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if errNew != nil {
		return nil, errNew
	}
	e.applyHeaders(httpReq, auth, upstreamModel, opts.Headers)
	e.recordRequest(ctx, auth, url, httpReq.Header, body)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	// Early role/empty frames are first-packet fallbacks, not effective tokens.
	httpClient = reporter.TrackHTTPClientRoundTripOnly(httpClient)
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return nil, errDo
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("workbuddy executor: close response body error: %v", errClose)
		}
		return nil, workBuddyStatusError(httpResp.StatusCode, httpResp.Header, b)
	}

	originalPayload := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayload = opts.OriginalRequest
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("workbuddy executor: close response body error: %v", errClose)
			}
		}()

		to := sdktranslator.FromString("openai")
		responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
		claudeInputTokens := helps.NewClaudeInputTokenState(opts.SourceFormat, to, responseFormat, bytes.Clone(originalPayload))
		repair := helps.NewWorkBuddyMarkupRepair(helps.WorkBuddyToolNameAllowlist(body), true)
		lastFrameMeta := map[string]any{}
		maxToolIndex := -1
		toolFinishSent := false
		var param any
		var streamUsage helps.StreamUsageBuffer
		seenDone := false
		failed := false
		aborted := false

		publish := func(chunk cliproxyexecutor.StreamChunk) bool {
			select {
			case out <- chunk:
				return true
			case <-ctx.Done():
				aborted = true
				return false
			}
		}
		publishError := func(streamErr error) {
			helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
			streamUsage.PublishFailure(ctx, reporter, streamErr)
			publish(cliproxyexecutor.StreamChunk{Err: streamErr})
			failed = true
		}
		emitFrames := func(frames [][]byte) bool {
			for _, frame := range frames {
				if len(frame) == 0 {
					continue
				}
				helps.ObserveChatTokenEvent(reporter, frame)
				streamLine := append([]byte("data: "), frame...)
				chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, helps.ApplyPatchOriginalRequest(req, opts), body, streamLine, &param, claudeInputTokens)
				if helps.ApplyPatchTranslationError(param) != nil {
					publishError(statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage})
					return false
				}
				for _, chunk := range chunks {
					if !publish(cliproxyexecutor.StreamChunk{Payload: chunk}) {
						return false
					}
				}
			}
			return true
		}
		// flushRepair emits any held-back content and a synthesized tool_calls
		// finish frame. It must run before the terminal [DONE] translation so
		// those business frames precede the client's message_stop event.
		flushRepair := func() bool {
			if tail := repair.Flush(); tail != "" {
				tailFrame := newWorkBuddyChunkFrame(lastFrameMeta, map[string]any{"content": tail}, "")
				if !emitFrames([][]byte{marshalWorkBuddyFrame(nil, tailFrame)}) {
					return false
				}
			}
			if repair.Converted() > 0 && !toolFinishSent {
				finishFrame := newWorkBuddyChunkFrame(lastFrameMeta, map[string]any{}, "tool_calls")
				if !emitFrames([][]byte{marshalWorkBuddyFrame(nil, finishFrame)}) {
					return false
				}
				toolFinishSent = true
			}
			return true
		}

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800) // 50MB
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			trimmed := bytes.TrimSpace(line)
			if !bytes.HasPrefix(trimmed, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(trimmed[len("data:"):])
			if len(payload) == 0 {
				continue
			}
			streamUsage.ObserveOpenAIStream(payload)
			if bytes.Equal(payload, []byte("[DONE]")) {
				// Repair frames must be emitted before the terminal translation,
				// otherwise they land after message_stop and are dropped.
				if !flushRepair() {
					return
				}
				if aborted {
					return
				}
				chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, helps.ApplyPatchOriginalRequest(req, opts), body, []byte("data: [DONE]"), &param, claudeInputTokens)
				for _, chunk := range chunks {
					if !publish(cliproxyexecutor.StreamChunk{Payload: chunk}) {
						break
					}
				}
				if !aborted {
					seenDone = true
				}
				break
			}
			if !json.Valid(payload) {
				publishError(statusErr{code: http.StatusBadGateway, msg: "workbuddy executor: upstream stream contained an invalid data frame"})
				return
			}
			if _, isError := helps.WorkBuddyStreamFrameErrorPayload(payload); isError {
				publishError(workBuddyStatusError(http.StatusOK, httpResp.Header, payload))
				return
			}
			rememberWorkBuddyFrameMeta(lastFrameMeta, payload)
			frames := applyWorkBuddyMarkupToFrame(payload, repair, &maxToolIndex, &toolFinishSent)
			if !emitFrames(frames) {
				return
			}
			if aborted {
				return
			}
		}
		errScan := scanner.Err()
		// [DONE] already flushed the repair state before translating the terminal
		// frame. Only an EOF without [DONE] reaches this path.
		if errScan == nil && !failed && !aborted && !seenDone {
			if !flushRepair() {
				return
			}
		}
		if failed || aborted {
			return
		}
		if errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			streamUsage.PublishFailure(ctx, reporter, errScan)
			publish(cliproxyexecutor.StreamChunk{Err: errScan})
			return
		}
		if !seenDone {
			streamErr := statusErr{code: http.StatusBadGateway, msg: "workbuddy executor: upstream stream closed before [DONE]", streamTruncated: true}
			helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
			streamUsage.PublishFailure(ctx, reporter, streamErr)
			publish(cliproxyexecutor.StreamChunk{Err: streamErr})
			return
		}
		streamUsage.Publish(ctx, reporter)
		reporter.EnsurePublished(ctx)
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// applyWorkBuddyMarkupToFrame converts DSML markup in one upstream frame into
// plain content plus synthesized tool_call frames. Frames without markup are
// returned unchanged.
func applyWorkBuddyMarkupToFrame(payload []byte, repair *helps.WorkBuddyMarkupRepair, maxToolIndex *int, toolFinishSent *bool) [][]byte {
	if repair == nil || !repair.Enabled() {
		return [][]byte{payload}
	}
	var frame map[string]any
	if errUnmarshal := json.Unmarshal(payload, &frame); errUnmarshal != nil {
		return [][]byte{payload}
	}
	choices, ok := frame["choices"].([]any)
	if !ok {
		return [][]byte{payload}
	}

	var recovered []helps.WorkBuddyMarkupCall
	for _, rawChoice := range choices {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		if calls, ok := delta["tool_calls"].([]any); ok {
			for _, rawCall := range calls {
				call, ok := rawCall.(map[string]any)
				if !ok {
					continue
				}
				if index, ok := call["index"].(float64); ok && int(index) > *maxToolIndex {
					*maxToolIndex = int(index)
				}
			}
		}
		text, ok := delta["content"].(string)
		if !ok || text == "" {
			continue
		}
		emit, calls := repair.Feed(text)
		recovered = append(recovered, calls...)
		if emit == "" {
			delete(delta, "content")
		} else {
			delta["content"] = emit
		}
	}

	if len(recovered) == 0 {
		if repair.Converted() > 0 && !*toolFinishSent {
			for _, rawChoice := range choices {
				choice, ok := rawChoice.(map[string]any)
				if !ok {
					continue
				}
				if reason, _ := choice["finish_reason"].(string); reason == "stop" {
					choice["finish_reason"] = "tool_calls"
					*toolFinishSent = true
				}
			}
		}
		return [][]byte{marshalWorkBuddyFrame(payload, frame)}
	}

	// The finish reason must follow the synthesized tool_call frames, otherwise
	// clients consider the turn finished before they see the call.
	finishReason := ""
	for _, rawChoice := range choices {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			continue
		}
		if reason, _ := choice["finish_reason"].(string); reason != "" {
			if finishReason == "" {
				finishReason = reason
			}
			delete(choice, "finish_reason")
		}
	}

	frames := [][]byte{marshalWorkBuddyFrame(payload, frame)}
	nextIndex := *maxToolIndex + 1
	for _, call := range recovered {
		callFrame := newWorkBuddyChunkFrame(frame, map[string]any{
			"tool_calls": helps.WorkBuddyMarkupToolCalls([]helps.WorkBuddyMarkupCall{call}, nextIndex),
		}, "")
		frames = append(frames, marshalWorkBuddyFrame(nil, callFrame))
		*maxToolIndex = nextIndex
		nextIndex++
	}
	if finishReason != "" {
		if finishReason == "stop" {
			finishReason = "tool_calls"
		}
		finishFrame := newWorkBuddyChunkFrame(frame, map[string]any{}, finishReason)
		frames = append(frames, marshalWorkBuddyFrame(nil, finishFrame))
		*toolFinishSent = finishReason == "tool_calls"
	}
	return frames
}

// newWorkBuddyChunkFrame builds a chat.completion.chunk frame, copying message
// identity fields from src when available.
func newWorkBuddyChunkFrame(src map[string]any, delta map[string]any, finishReason string) map[string]any {
	choice := map[string]any{
		"index": 0,
		"delta": delta,
	}
	if finishReason != "" {
		choice["finish_reason"] = finishReason
	}
	frame := map[string]any{
		"object":  "chat.completion.chunk",
		"choices": []any{choice},
	}
	for _, key := range []string{"id", "created", "model", "system_fingerprint"} {
		if src == nil {
			break
		}
		if value, ok := src[key]; ok && value != nil {
			frame[key] = value
		}
	}
	return frame
}

// marshalWorkBuddyFrame encodes a frame, falling back to original on failure.
func marshalWorkBuddyFrame(original []byte, frame map[string]any) []byte {
	encoded, errEncode := json.Marshal(frame)
	if errEncode != nil {
		return original
	}
	return encoded
}

// rememberWorkBuddyFrameMeta keeps the identity fields used by synthesized
// frames later in the stream.
func rememberWorkBuddyFrameMeta(meta map[string]any, payload []byte) {
	if id := gjson.GetBytes(payload, "id"); id.Exists() && id.String() != "" {
		meta["id"] = id.String()
	}
	if model := gjson.GetBytes(payload, "model"); model.Exists() && model.String() != "" {
		meta["model"] = model.String()
	}
	if created := gjson.GetBytes(payload, "created"); created.Exists() {
		meta["created"] = created.Int()
	}
}
