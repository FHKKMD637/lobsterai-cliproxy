package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// executorStreamResponse mirrors the host's executor.execute_stream reply: a
// header block plus an inline chunk list for the synchronous fallback path.
type executorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

// executorStreamRequest is the host's executor.execute_stream RPC payload: the
// ExecutorRequest plus the async stream id used for host.stream.emit.
type executorStreamRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func clientNeedsSSEFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions", "/v1/responses":
		return false
	default:
		return true
	}
}

// prepareBody forces stream=true because the LobsterAI upstream rejects
// non-streaming chat requests, and normalizes tool_choice.
func prepareBody(raw []byte, model string) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("decode request body: %w", err)
	}
	if currentConfig().forceStream() {
		obj["stream"] = true
	}
	if strings.TrimSpace(model) != "" {
		obj["model"] = resolveUpstreamModel(model)
	}
	if tc, ok := obj["tool_choice"].(string); ok && (tc == "" || tc == "none") {
		delete(obj, "tool_choice")
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("encode request body: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Non-streaming execution
// ---------------------------------------------------------------------------

func handleExecExecute(raw []byte) ([]byte, error) {
	var req pluginapi.ExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if modelExcluded(req.Model) {
		return nil, fmt.Errorf("model_excluded: %s 已被插件配置排除，拒绝转发以免消耗 LobsterAI 额度", req.Model)
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	payload := req.Payload
	if len(payload) == 0 {
		payload = req.OriginalRequest
	}
	body, err := prepareBody(payload, req.Model)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	rc, _, err := newClient(currentConfig()).chatStream(ctx, sa, body)
	if err != nil {
		return nil, wrapUpstreamError(err)
	}
	defer rc.Close()
	completion, err := aggregateCompletion(rc, req.Model)
	if err != nil {
		if isModelDeniedError(err) {
			noteModelDenied(sa.Account.UID, req.Model)
		}
		return nil, wrapUpstreamError(err)
	}
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: completion,
		Headers: jsonHeaders(),
	})
}

// ---------------------------------------------------------------------------
// Streaming execution
// ---------------------------------------------------------------------------

func handleExecStream(raw []byte) ([]byte, error) {
	var req executorStreamRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if modelExcluded(req.Model) {
		return nil, fmt.Errorf("model_excluded: %s 已被插件配置排除，拒绝转发以免消耗 LobsterAI 额度", req.Model)
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	payload := req.Payload
	if len(payload) == 0 {
		payload = req.OriginalRequest
	}
	body, err := prepareBody(payload, req.Model)
	if err != nil {
		return nil, err
	}
	framed := clientNeedsSSEFrame(req.Metadata)

	// Synchronous collection when the host did not hand us a stream id.
	if strings.TrimSpace(req.StreamID) == "" {
		chunks, errCollect := collectStream(sa, body, framed, req.Model)
		if errCollect != nil {
			return nil, errCollect
		}
		return okEnvelope(executorStreamResponse{
			Headers: sseHeaders(),
			Chunks:  chunks,
		})
	}

	// Asynchronous pump: return headers immediately, stream chunks through
	// host.stream.emit so the downstream client sees true incremental output.
	go pumpStream(req.StreamID, sa, body, framed, req.Model)
	return okEnvelope(executorStreamResponse{Headers: sseHeaders()})
}

// pumpStream reads the upstream SSE stream and forwards cleaned chunks to the
// host-owned stream, closing it exactly once.
func pumpStream(streamID string, sa *storedAuth, body []byte, framed bool, model string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	rc, _, err := newClient(currentConfig()).chatStream(ctx, sa, body)
	if err != nil {
		streamEmitError(streamID, wrapUpstreamError(err).Error())
		streamClose(streamID)
		return
	}
	defer rc.Close()

	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "event:") {
			continue
		}
		payload := stripDataPrefix(line)
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) == nil {
			if errUpstream := sseErrorFromChunk(obj); errUpstream != nil {
				if isModelDeniedError(errUpstream) {
					noteModelDenied(sa.Account.UID, model)
				}
				streamEmitError(streamID, wrapUpstreamError(errUpstream).Error())
				streamClose(streamID)
				return
			}
		}
		cleaned := cleanChunkJSON(payload)
		if cleaned == "" {
			continue
		}
		if framed {
			cleaned = "data: " + cleaned + "\n\n"
		}
		if err := streamEmit(streamID, []byte(cleaned)); err != nil {
			// Downstream client is gone: stop reading the upstream.
			return
		}
	}
	if err := scanner.Err(); err != nil {
		streamEmitError(streamID, truncate(err.Error(), 200))
	}
	streamClose(streamID)
}

// collectStream drains the upstream into discrete chunks for the synchronous
// fallback path (no host stream id).
func collectStream(sa *storedAuth, body []byte, framed bool, model string) ([]pluginapi.ExecutorStreamChunk, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	rc, _, err := newClient(currentConfig()).chatStream(ctx, sa, body)
	if err != nil {
		if isModelDeniedError(err) {
			noteModelDenied(sa.Account.UID, model)
		}
		return nil, wrapUpstreamError(err)
	}
	defer rc.Close()

	chunks := make([]pluginapi.ExecutorStreamChunk, 0, 64)
	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "event:") {
			continue
		}
		payload := stripDataPrefix(line)
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) == nil {
			if errUpstream := sseErrorFromChunk(obj); errUpstream != nil {
				if isModelDeniedError(errUpstream) {
					noteModelDenied(sa.Account.UID, model)
				}
				return nil, wrapUpstreamError(errUpstream)
			}
		}
		cleaned := cleanChunkJSON(payload)
		if cleaned == "" {
			continue
		}
		if framed {
			cleaned = "data: " + cleaned + "\n\n"
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: []byte(cleaned)})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return chunks, nil
}

// wrapUpstreamError converts upstream failures into actionable messages so the
// CPA scheduler can classify and cool the credential.
func wrapUpstreamError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case isQuotaError(err):
		return fmt.Errorf("insufficient_credit: %w", err)
	case isAuthError(err):
		return fmt.Errorf("auth_expired: %w", err)
	}
	return err
}

// isModelDeniedError reports whether the upstream refused this specific model
// for the account (LobsterAI advertises models the current tier cannot call).
func isModelDeniedError(err error) bool {
	if err == nil {
		return false
	}
	if ae, ok := err.(*apiError); ok && ae.Code == 40301 {
		return true
	}
	msg := err.Error()
	for _, marker := range []string{"模型不可见", "无访问权限", "model not available", "not visible"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// streaming helpers
// ---------------------------------------------------------------------------

func sseHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	return h
}

func jsonHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return h
}

func stripDataPrefix(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "data:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	}
	return s
}

// cleanChunkJSON drops empty-valued delta fields (null / "" / [] / {}) that
// strict OpenAI clients reject, e.g. {"tool_calls":[],"function_call":null}.
func cleanChunkJSON(s string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(s), &obj) != nil {
		return s
	}
	for _, k := range []string{"usage", "service_tier"} {
		if v, ok := obj[k]; ok && v == nil {
			delete(obj, k)
		}
	}
	if choices, ok := obj["choices"].([]any); ok {
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				for k, v := range delta {
					if isEmptyValue(v) {
						delete(delta, k)
					}
				}
			}
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return s
	}
	return string(out)
}

// aggregateCompletion folds the SSE stream into one chat.completion object for
// non-streaming clients, merging tool_call deltas by index.
func aggregateCompletion(r io.Reader, model string) ([]byte, error) {
	var (
		id, respModel, role, finish string
		created                     int64
		content                     strings.Builder
		reasoning                   strings.Builder
		usage                       map[string]any
		toolCalls                   = map[int]map[string]any{}
		toolOrder                   []int
	)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if errUpstream := sseErrorFromChunk(chunk); errUpstream != nil {
			return nil, errUpstream
		}
		if v, ok := chunk["id"].(string); ok && v != "" && id == "" {
			id = v
		}
		if v, ok := chunk["model"].(string); ok && v != "" && respModel == "" {
			respModel = v
		}
		if v, ok := chunk["created"].(float64); ok && created == 0 {
			created = int64(v)
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if choice == nil {
				continue
			}
			if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
				finish = fr
			}
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				if msg, ok := choice["message"].(map[string]any); ok {
					delta = msg
				}
			}
			if delta == nil {
				continue
			}
			if v, ok := delta["role"].(string); ok && v != "" {
				role = v
			}
			if v, ok := delta["content"].(string); ok {
				content.WriteString(v)
			}
			if v, ok := delta["reasoning_content"].(string); ok {
				reasoning.WriteString(v)
			}
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					call, ok := tc.(map[string]any)
					if !ok {
						continue
					}
					idx := 0
					if v, ok := call["index"].(float64); ok {
						idx = int(v)
					}
					merged, seen := toolCalls[idx]
					if !seen {
						merged = map[string]any{"index": idx}
						toolCalls[idx] = merged
						toolOrder = append(toolOrder, idx)
					}
					mergeToolCallDelta(merged, call)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	message := map[string]any{"role": firstNonEmpty(role, "assistant"), "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	result := map[string]any{
		"id":      firstNonEmpty(id, fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())),
		"object":  "chat.completion",
		"created": created,
		"model":   firstNonEmpty(respModel, model),
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": firstNonEmpty(finish, "stop"),
		}},
	}
	if usage != nil {
		result["usage"] = usage
	}
	return json.Marshal(result)
}

func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

func sortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}
