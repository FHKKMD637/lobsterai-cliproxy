package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	loginTTL         = 10 * time.Minute
	callbackPath     = "/auth/callback"
	callbackHost     = "127.0.0.1"
	refreshSkew      = 10 * time.Minute
	sessionPollGrace = 30 * time.Second
)

// loginSession tracks one in-flight browser login. The LobsterAI portal
// validates redirect_uri against a loopback callback, so the plugin runs a
// short-lived listener inside the CPA process and exchanges the code there.
type loginSession struct {
	state        string
	uuid         string
	firstKeyfrom string
	port         int
	server       *http.Server
	expires      time.Time

	mu       sync.Mutex
	auth     *storedAuth
	err      error
	done     bool
	consumed bool
}

func (s *loginSession) setResult(sa *storedAuth, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	s.auth = sa
	s.err = err
	s.done = true
}

func (s *loginSession) result() (*storedAuth, error, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.done {
		return nil, nil, false
	}
	return s.auth, s.err, true
}

func (s *loginSession) close() {
	if s != nil && s.server != nil {
		_ = s.server.Close()
	}
}

// randomHex returns n random bytes hex-encoded.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// newUUID returns a random RFC 4122 v4 UUID, matching the desktop client's
// install fingerprint field.
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return randomHex(16)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func nowMillis() string {
	return fmt.Sprintf("%d", time.Now().UnixMilli())
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func handleParseAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.RawJSON)
	if err != nil {
		// Not a LobsterAI credential — let the host try other providers.
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: toAuthData(sa)})
}

func handleStartLogin(raw []byte) ([]byte, error) {
	var req struct {
		Provider string         `json:"provider"`
		BaseURL  string         `json:"base_url"`
		Metadata map[string]any `json:"metadata"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	cfg := currentConfig()
	cli := newClient(cfg)

	state := randomHex(16)
	ls := &loginSession{
		state:        state,
		uuid:         newUUID(),
		firstKeyfrom: nowMillis(),
		expires:      time.Now().Add(loginTTL),
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("%s:0", callbackHost))
	if err != nil {
		return nil, fmt.Errorf("login listener: %w", err)
	}
	ls.port = ln.Addr().(*net.TCPAddr).Port

	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if code := strings.TrimSpace(q.Get("code")); code != "" && strings.TrimSpace(q.Get("state")) == ls.state {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			sa, errExchange := cli.exchange(ctx, code, ls)
			cancel()
			ls.setResult(sa, errExchange)
			writeCallbackPage(w, errExchange)
			return
		}
		if errMsg := strings.TrimSpace(firstNonEmpty(q.Get("error_description"), q.Get("error"))); errMsg != "" {
			ls.setResult(nil, fmt.Errorf("login rejected: %s", errMsg))
			writeCallbackPage(w, fmt.Errorf("login rejected: %s", errMsg))
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("授权回调参数无效，请重新发起登录"))
	})

	ls.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = ls.server.Serve(ln) }()
	go expireLoginSession(ls)

	loginState.Store(state, ls)

	redirectURI := fmt.Sprintf("http://%s:%d%s", callbackHost, ls.port, callbackPath)
	loginURL := fmt.Sprintf("%s/portal#/login?source=electron&redirect_uri=%s&state=%s",
		cfg.loginPortal(), urlQueryEscape(redirectURI), state)

	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       loginURL,
		State:     state,
		ExpiresAt: ls.expires.UTC(),
	})
}

// expireLoginSession tears down a listener whose flow was never completed so
// abandoned logins cannot leak ports or sessions.
func expireLoginSession(ls *loginSession) {
	timer := time.NewTimer(loginTTL + sessionPollGrace)
	defer timer.Stop()
	<-timer.C
	ls.close()
	loginState.Delete(ls.state)
}

func writeCallbackPage(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprintf(w, "<html><body><h2>登录失败</h2><p>%s</p><p>可以关闭此页面，回到 CPA 面板重试。</p></body></html>", htmlEscape(err.Error()))
		return
	}
	_, _ = w.Write([]byte("<html><body><h2>登录成功，可以关闭此窗口了</h2><p>凭据已写入 CPA。</p></body></html>"))
}

func handlePollLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return nil, fmt.Errorf("poll: empty state")
	}
	v, ok := loginState.Load(state)
	var ls *loginSession
	if ok {
		ls, _ = v.(*loginSession)
	}

	// Remote-browser fallback: when the panel's "回调 URL" box is used (or the
	// browser cannot reach the plugin's loopback listener), the host persists the
	// code as {authDir}/.oauth-lobsterai-<state>.oauth. Consume it exactly like the
	// built-in providers do so both login paths work.
	if ls == nil || func() bool { _, _, done := ls.result(); return !done }() {
		if authDir := strings.TrimSpace(req.Host.AuthDir); authDir != "" {
			if code, errCallback, found := readHostCallbackFile(authDir, state); found {
				if ls == nil {
					ls = &loginSession{
						state:        state,
						uuid:         newUUID(),
						firstKeyfrom: nowMillis(),
						expires:      time.Now().Add(loginTTL),
					}
					loginState.Store(state, ls)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				if errCallback != nil {
					cancel()
					ls.setResult(nil, errCallback)
				} else {
					sa, errExchange := newClient(currentConfig()).exchange(ctx, code, ls)
					cancel()
					ls.setResult(sa, errExchange)
				}
			}
		}
	}

	if ls == nil {
		return nil, fmt.Errorf("poll: unknown or expired state (restart login)")
	}
	if time.Now().After(ls.expires.Add(sessionPollGrace)) {
		ls.close()
		loginState.Delete(state)
		return nil, fmt.Errorf("poll: login expired")
	}

	sa, errResult, done := ls.result()
	if !done {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for login",
		})
	}
	if errResult != nil {
		ls.close()
		loginState.Delete(state)
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: errResult.Error(),
		})
	}

	ls.mu.Lock()
	alreadyConsumed := ls.consumed
	ls.consumed = true
	ls.mu.Unlock()
	if alreadyConsumed {
		return okEnvelope(pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "credential already delivered",
		})
	}

	ls.close()
	loginState.Delete(state)

	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   toAuthData(sa),
	})
}

func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	if strings.TrimSpace(sa.Auth.RefreshToken) == "" {
		return nil, fmt.Errorf("refresh: credential has no refreshToken")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := newClient(currentConfig()).refresh(ctx, sa); err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             toAuthData(sa),
		NextRefreshAfter: nextRefreshAfter(sa),
	})
}

// callbackFilePayload mirrors the host's .oauth callback file written by
// POST /v0/management/oauth-callback.
type callbackFilePayload struct {
	Code  string `json:"code"`
	State string `json:"state"`
	Error string `json:"error"`
}

// readHostCallbackFile reads and consumes the host-written OAuth callback file
// for this plugin. found reports whether a file existed; err carries a
// user-facing failure from the callback payload.
func readHostCallbackFile(authDir, state string) (code string, err error, found bool) {
	path := filepath.Join(authDir, fmt.Sprintf(".oauth-%s-%s.oauth", providerName, state))
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return "", nil, false
	}
	// The host writes atomically (temp + rename), so a successful read is whole.
	_ = os.Remove(path)
	var payload callbackFilePayload
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		return "", fmt.Errorf("invalid oauth callback payload: %w", errUnmarshal), true
	}
	if msg := strings.TrimSpace(payload.Error); msg != "" {
		return "", fmt.Errorf("login rejected: %s", msg), true
	}
	code = strings.TrimSpace(payload.Code)
	if code == "" {
		return "", fmt.Errorf("oauth callback carried no code"), true
	}
	return code, nil, true
}

func urlQueryEscape(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var out []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' || c == '/' || c == ':':
			out = append(out, c)
		default:
			out = append(out, '%', hexDigits[c>>4], hexDigits[c&0x0f])
		}
	}
	return string(out)
}

func htmlEscape(s string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return replacer.Replace(s)
}
