package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// apiEnvelope is LobsterAI's uniform {code,message,data} response wrapper.
type apiEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Msg     string          `json:"msg"`
	Data    json.RawMessage `json:"data"`
}

func (e apiEnvelope) message() string {
	return firstNonEmpty(e.Message, e.Msg)
}

// client wraps the HTTP transport for all LobsterAI upstream calls. It is
// deliberately stateless: credentials travel in per-call arguments.
type client struct {
	http    *http.Client
	baseURL string
	portal  string
	ua      string
}

func newClient(cfg featureConfig) *client {
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}
	// LobsterAI is a domestic (CN) endpoint, so the default is a direct dial;
	// proxy_url opts into a specific egress when the host needs one.
	if raw := cfg.proxyURL(); raw != "" {
		if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
			transport.Proxy = http.ProxyURL(parsed)
		} else {
			logWarn("invalid proxy_url ignored", map[string]any{"proxy_url": raw})
		}
	}
	return &client{
		http:    &http.Client{Timeout: 300 * time.Second, Transport: transport},
		baseURL: cfg.upstreamBase(),
		portal:  cfg.loginPortal(),
		ua:      "LobsterAI/" + cfg.clientVersion(),
	}
}

// authHeaders are sent on authenticated upstream calls (chat / models / quota).
func (c *client) authHeaders(req *http.Request, sa *storedAuth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	req.Header.Set("X-LobsterAI-Client-Capabilities", clientCapabilitiesHdr)
	req.Header.Set("X-LobsterAI-Client-Version", currentConfig().clientVersion())
}

// baseHeaders are sent on unauthenticated / login calls.
func (c *client) baseHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", c.ua)
}

// doJSON performs a request and unwraps the {code,message,data} envelope.
func (c *client) doJSON(ctx context.Context, method, url string, auth *storedAuth, body []byte) (json.RawMessage, error) {
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	if auth != nil {
		c.authHeaders(req, auth)
	} else {
		c.baseHeaders(req)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &upstreamStatusError{Status: resp.StatusCode, Body: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		return nil, &apiError{Code: env.Code, Message: env.message()}
	}
	return env.Data, nil
}

// upstreamStatusError carries a non-2xx HTTP status from the upstream.
type upstreamStatusError struct {
	Status int
	Body   string
}

func (e *upstreamStatusError) Error() string {
	return fmt.Sprintf("upstream http %d: %s", e.Status, e.Body)
}

// apiError carries a business-level {code != 0} failure.
type apiError struct {
	Code    int
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("upstream code=%d msg=%s", e.Code, truncate(e.Message, 160))
}

// ---------------------------------------------------------------------------
// auth: exchange / refresh
// ---------------------------------------------------------------------------

// exchange trades an OAuth authCode for tokens. keyfrom is the desktop client
// fingerprint bundle captured when the login flow started.
func (c *client) exchange(ctx context.Context, code string, ls *loginSession) (*storedAuth, error) {
	body, _ := json.Marshal(map[string]any{
		"authCode":      code,
		"firstKeyfrom":  ls.firstKeyfrom,
		"latestKeyfrom": nowMillis(),
		"uuid":          ls.uuid,
		"version":       "0.1.0",
	})
	data, err := c.doJSON(ctx, http.MethodPost, c.baseURL+"/api/auth/exchange", nil, body)
	if err != nil {
		return nil, err
	}
	var ex struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		User         struct {
			ID       string `json:"id"`
			Yid      string `json:"yid"`
			UserID   string `json:"userId"`
			Nickname string `json:"nickname"`
		} `json:"user"`
	}
	if err := json.Unmarshal(data, &ex); err != nil {
		return nil, fmt.Errorf("exchange parse: %w", err)
	}
	if strings.TrimSpace(ex.AccessToken) == "" {
		return nil, fmt.Errorf("exchange response missing accessToken")
	}
	uid := firstNonEmpty(ex.User.ID, ex.User.UserID, ex.User.Yid)
	if uid == "" {
		uid = deriveUID(ex.AccessToken)
	}
	sa := &storedAuth{
		Auth: storedTokens{
			AccessToken:   ex.AccessToken,
			RefreshToken:  ex.RefreshToken,
			ExpiresAt:     expiresAtFrom(ex.ExpiresIn, ex.AccessToken),
			UUID:          ls.uuid,
			FirstKeyfrom:  ls.firstKeyfrom,
			LatestKeyfrom: nowMillis(),
		},
		Account: storedAccount{
			UID:      uid,
			UserID:   ex.User.UserID,
			Nickname: ex.User.Nickname,
		},
	}
	return sa, nil
}

// refresh rotates the access token. keyfrom fields are replayed so the upstream
// can still bind the refresh to the original desktop install fingerprint.
func (c *client) refresh(ctx context.Context, sa *storedAuth) error {
	if strings.TrimSpace(sa.Auth.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	body, _ := json.Marshal(map[string]any{
		"refreshToken":  sa.Auth.RefreshToken,
		"firstKeyfrom":  firstNonEmpty(sa.Auth.FirstKeyfrom, nowMillis()),
		"latestKeyfrom": nowMillis(),
		"uuid":          sa.Auth.UUID,
		"version":       "0.1.0",
	})
	data, err := c.doJSON(ctx, http.MethodPost, c.baseURL+"/api/auth/refresh", nil, body)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
	}
	if err := json.Unmarshal(data, &tok); err != nil {
		return fmt.Errorf("refresh parse: %w", err)
	}
	if strings.TrimSpace(tok.AccessToken) == "" {
		return fmt.Errorf("refresh_failed: response missing accessToken — re-login required")
	}
	sa.Auth.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		sa.Auth.RefreshToken = tok.RefreshToken
	}
	sa.Auth.ExpiresAt = expiresAtFrom(tok.ExpiresIn, tok.AccessToken)
	sa.Auth.LatestKeyfrom = nowMillis()
	return nil
}

func expiresAtFrom(expiresIn int64, accessToken string) int64 {
	if expiresIn > 0 {
		return time.Now().Add(time.Duration(expiresIn) * time.Second).Unix()
	}
	if exp := jwtExpiry(accessToken); exp > 0 {
		return exp
	}
	return 0
}

// ---------------------------------------------------------------------------
// models / quota
// ---------------------------------------------------------------------------

type upstreamModel struct {
	ModelID   string `json:"modelId"`
	ModelName string `json:"modelName"`
	Provider  string `json:"provider"`
	APIFormat string `json:"apiFormat"`
}

// fetchModels lists the models the account can actually use. Errors are
// non-fatal for the caller, which falls back to the static table.
func (c *client) fetchModels(ctx context.Context, sa *storedAuth) ([]upstreamModel, error) {
	data, err := c.doJSON(ctx, http.MethodGet, c.baseURL+"/api/models/available", sa, nil)
	if err != nil {
		return nil, err
	}
	var out []upstreamModel
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	filtered := make([]upstreamModel, 0, len(out))
	for _, m := range out {
		if strings.TrimSpace(m.ModelID) != "" {
			filtered = append(filtered, m)
		}
	}
	return filtered, nil
}

type quotaSummary struct {
	TotalCreditsRemaining float64      `json:"totalCreditsRemaining"`
	CreditItems           []creditItem `json:"creditItems"`
}

type creditItem struct {
	Type             string  `json:"type"`
	Label            string  `json:"label"`
	CreditsRemaining float64 `json:"creditsRemaining"`
	ExpiresAt        string  `json:"expiresAt"`
}

// fetchQuota reads the account credit balance driving load balancing decisions.
func (c *client) fetchQuota(ctx context.Context, sa *storedAuth) (*quotaSummary, error) {
	data, err := c.doJSON(ctx, http.MethodGet, c.baseURL+"/api/user/profile-summary", sa, nil)
	if err != nil {
		return nil, err
	}
	var q quotaSummary
	if err := json.Unmarshal(data, &q); err != nil {
		return nil, fmt.Errorf("profile-summary parse: %w", err)
	}
	return &q, nil
}

// ---------------------------------------------------------------------------
// chat
// ---------------------------------------------------------------------------

// chatStream posts a chat completion and returns the raw upstream SSE body. The
// upstream only serves streaming, so non-streaming callers aggregate locally.
func (c *client) chatStream(ctx context.Context, sa *storedAuth, body []byte) (io.ReadCloser, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/proxy/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	c.authHeaders(req, sa)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, resp.StatusCode, &upstreamStatusError{Status: resp.StatusCode, Body: truncate(string(raw), 400)}
	}
	return resp.Body, resp.StatusCode, nil
}

// sseError is the in-stream error frame the LobsterAI gateway returns with
// HTTP 200, e.g.:
//
//	event:error
//	data:{"type":"error","error":{"message":"免费额度已用完，请升级套餐","code":40201}}
type sseError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Code    int    `json:"code"`
	} `json:"error"`
}

// sseErrorFromChunk extracts an upstream in-stream error, if present, from a
// decoded SSE data frame. Without this the error frame is indistinguishable
// from a content chunk and callers would see an empty completion instead of a
// failure.
func sseErrorFromChunk(obj map[string]any) error {
	if obj == nil {
		return nil
	}
	raw, ok := obj["error"]
	if !ok {
		if t, _ := obj["type"].(string); t != "error" {
			return nil
		}
	}
	errObj, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	msg, _ := errObj["message"].(string)
	code := 0
	if v, ok := errObj["code"].(float64); ok {
		code = int(v)
	}
	if strings.TrimSpace(msg) == "" && code == 0 {
		return nil
	}
	return &apiError{Code: code, Message: msg}
}

// quotaError reports whether the given error indicates an exhausted balance so
// the caller can cool the credential down for a long window.
func isQuotaError(err error) bool {
	if err == nil {
		return false
	}
	if ae, ok := err.(*apiError); ok && (ae.Code == 40200 || ae.Code == 40201) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "额度已用完", "免费额度已用完",
		"insufficient credit", "quota exceeded", "payment required",
	} {
		if strings.Contains(msg, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

// isAuthError reports whether the upstream rejected the credential itself.
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	if se, ok := err.(*upstreamStatusError); ok && (se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden) {
		return true
	}
	if ae, ok := err.(*apiError); ok && (ae.Code == 40100 || ae.Code == 40101 || ae.Code == 40300 || ae.Code < 0) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"登录已过期", "未登录", "请重新登录", "token rejected", "unauthorized"} {
		if strings.Contains(msg, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}
