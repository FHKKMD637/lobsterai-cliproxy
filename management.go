package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	resourcePath = "/credits"
	menuLabel    = "lobsterai"
)

// managementRegistration advertises the plugin-owned Management API routes and
// the browser-navigable resource page rendered as a sidebar menu in the CPA
// Management Center.
func managementRegistration() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{
				Method:      http.MethodGet,
				Path:        "/plugins/" + providerName + "/credits",
				Description: "LobsterAI 各账号当前积分余额（JSON）",
			},
			{
				Method:      http.MethodPost,
				Path:        "/plugins/" + providerName + "/credits/refresh",
				Description: "刷新全部 LobsterAI 账号的积分余额并写回凭据备注",
			},
			{
				Method:      http.MethodPost,
				Path:        "/plugins/" + providerName + "/checkin",
				Description: "执行每日签到（+积分），可选 body {auth_index} 指定单个账号",
			},
		},
		Resources: []pluginapi.ResourceRoute{
			{
				Path:        resourcePath,
				Menu:        menuLabel,
				Description: "LobsterAI 账号当前积分余额",
			},
		},
	}
}

// accountCredits is one row rendered by the credits page.
type accountCredits struct {
	AuthIndex string       `json:"auth_index"`
	Name      string       `json:"name"`
	Label     string       `json:"label"`
	UID       string       `json:"uid,omitempty"`
	Credits   float64      `json:"credits"`
	Items     []creditItem `json:"items,omitempty"`
	Status    string       `json:"status"`
	Err       string       `json:"error,omitempty"`
}

func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(req.Path, "/")
	resPrefix := "/v0/resource/plugins/" + providerName

	// Browser page: unauthenticated resource route, rendered server-side so the
	// page needs no management key.
	if req.Method == http.MethodGet && strings.HasPrefix(path, resPrefix) {
		sub := strings.TrimPrefix(path, resPrefix)
		if sub == "" || sub == resourcePath {
			force := req.Query.Get("refresh") == "1"
			notes := []string{}
			if req.Query.Get("checkin") == "1" {
				notes = append(notes, runCheckinAll()...)
				force = true
			}
			body := renderCreditsPage(collectCredits(force), notes)
			return okEnvelope(htmlResponse(http.StatusOK, body))
		}
		return okEnvelope(htmlResponse(http.StatusNotFound, "<h1>404</h1>"))
	}

	base := "/v0/management/plugins/" + providerName
	switch {
	case req.Method == http.MethodGet && path == base+"/credits":
		return okEnvelope(jsonResponse(http.StatusOK, creditsPayload(collectCredits(req.Query.Get("refresh") == "1"))))
	case req.Method == http.MethodPost && path == base+"/credits/refresh":
		return okEnvelope(jsonResponse(http.StatusOK, creditsPayload(collectCredits(true))))
	case req.Method == http.MethodPost && path == base+"/checkin":
		var body struct {
			AuthIndex string `json:"auth_index"`
		}
		_ = json.Unmarshal(req.Body, &body)
		results := runCheckin(body.AuthIndex)
		return okEnvelope(jsonResponse(http.StatusOK, map[string]any{"results": results}))
	default:
		return okEnvelope(jsonResponse(http.StatusNotFound, map[string]any{"error": "not found: " + path}))
	}
}

func htmlResponse(status int, body string) pluginapi.ManagementResponse {
	h := http.Header{}
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: h, Body: []byte(body)}
}

func jsonResponse(status int, v any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(v)
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: h, Body: body}
}

func creditsPayload(rows []accountCredits) map[string]any {
	var total float64
	for _, r := range rows {
		total += r.Credits
	}
	return map[string]any{
		"provider":       providerName,
		"accounts":       rows,
		"total_credits":  total,
		"account_count":  len(rows),
		"generated_at":   time.Now().Format(time.RFC3339),
		"upstream":       currentConfig().upstreamBase(),
		"client_version": currentConfig().clientVersion(),
		"auto_checkin":   autoCheckinSnapshot(),
		"effective_config": map[string]any{
			"upstream_base":   currentConfig().upstreamBase(),
			"client_version":  currentConfig().clientVersion(),
			"exclude_models":  currentConfig().ExcludeModels,
			"alias_models":    currentConfig().AliasModels,
			"alias_prefix":    currentConfig().aliasPrefix(),
			"checkin_auto":    currentConfig().checkinAuto(),
			"quota_minutes":   currentConfig().quotaRefreshMinutes(),
			"checkin_minutes": currentConfig().checkinIntervalMinutes(),
		},
	}
}

// ---------------------------------------------------------------------------
// Daily check-in
// ---------------------------------------------------------------------------

// runCheckin claims the daily reward for one credential (authIndex != "") or all
// of them, returning one human-readable line per account.
func runCheckin(authIndex string) []map[string]any {
	entries, err := hostAuthList()
	if err != nil {
		return []map[string]any{{"error": err.Error()}}
	}
	out := make([]map[string]any, 0)
	for _, e := range entries {
		if !isLobsterAuthEntry(e) {
			continue
		}
		if authIndex != "" && e.AuthIndex != authIndex {
			continue
		}
		row := map[string]any{"auth_index": e.AuthIndex, "name": e.Name}
		raw, errGet := hostAuthGet(e.AuthIndex)
		if errGet != nil {
			row["error"] = errGet.Error()
			out = append(out, row)
			continue
		}
		sa, errParse := parseStored(raw)
		if errParse != nil {
			row["error"] = errParse.Error()
			out = append(out, row)
			continue
		}
		res, errCheckin := checkinWithTimeout(sa)
		if errCheckin != nil {
			row["status"] = "error"
			row["error"] = truncate(errCheckin.Error(), 200)
			out = append(out, row)
			continue
		}
		row["status"] = res.Status
		row["credits_granted"] = res.Credits
		row["note"] = res.Note
		if res.Balance > 0 {
			row["balance"] = res.Balance
		}
		out = append(out, row)
	}
	return out
}

// runCheckinAll is the page-friendly wrapper returning display lines.
func runCheckinAll() []string {
	lines := make([]string, 0)
	for _, r := range runCheckin("") {
		name, _ := r["name"].(string)
		switch r["status"] {
		case "ok":
			lines = append(lines, fmt.Sprintf("%s 签到成功 +%s 积分", name, formatCredits(toFloat(r["credits_granted"]))))
		case "already":
			lines = append(lines, fmt.Sprintf("%s 今天已签到", name))
		case "unavailable":
			lines = append(lines, fmt.Sprintf("%s 当前无可用签到活动", name))
		default:
			lines = append(lines, fmt.Sprintf("%s 签到失败：%v", name, r["error"]))
		}
	}
	return lines
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	}
	return 0
}

// ---------------------------------------------------------------------------
// Background note refresher
// ---------------------------------------------------------------------------

var noteRefresherOnce sync.Once

// startNoteRefresher keeps each credential's note (shown in the Management
// Center credential list) roughly in sync with the live balance, so the UI
// shows the current credit count without opening the credits page.
func startNoteRefresher() {
	noteRefresherOnce.Do(func() {
		go func() {
			// Let the host finish registering credentials before the first pass.
			time.Sleep(20 * time.Second)
			for {
				minutes := currentConfig().quotaRefreshMinutes()
				if minutes <= 0 {
					time.Sleep(5 * time.Minute)
					continue
				}
				if rows := collectCredits(true); len(rows) > 0 {
					logInfo("credit notes refreshed", map[string]any{"accounts": len(rows)})
				}
				time.Sleep(time.Duration(minutes) * time.Minute)
			}
		}()
	})
}

// ---------------------------------------------------------------------------
// Credit collection
// ---------------------------------------------------------------------------

var (
	creditCacheMu sync.RWMutex
	creditCache   = map[string]cachedCredits{}
)

type cachedCredits struct {
	credits float64
	items   []creditItem
	at      time.Time
}

const creditCacheTTL = 60 * time.Second

// collectCredits walks every LobsterAI credential the host knows about and
// resolves its live balance, writing the value back into the credential note so
// the Management Center's credential list shows it inline.
func collectCredits(force bool) []accountCredits {
	entries, err := hostAuthList()
	if err != nil {
		return []accountCredits{{Name: "-", Status: "读取凭据失败", Err: err.Error()}}
	}
	filtered := make([]pluginapi.HostAuthFileEntry, 0, len(entries))
	for _, e := range entries {
		if !isLobsterAuthEntry(e) {
			continue
		}
		filtered = append(filtered, e)
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Name < filtered[j].Name })

	out := make([]accountCredits, 0, len(filtered))
	for _, e := range filtered {
		out = append(out, creditsForEntry(e, force))
	}
	return out
}

func isLobsterAuthEntry(e pluginapi.HostAuthFileEntry) bool {
	if strings.EqualFold(strings.TrimSpace(e.Provider), providerName) ||
		strings.EqualFold(strings.TrimSpace(e.Type), providerName) {
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(e.Name)), authFileNamePrefix)
}

func creditsForEntry(e pluginapi.HostAuthFileEntry, force bool) accountCredits {
	row := accountCredits{
		AuthIndex: e.AuthIndex,
		Name:      e.Name,
		Label:     firstNonEmpty(e.Label, e.Name),
		Status:    "ok",
	}
	if e.Disabled {
		row.Status = "已禁用"
	}

	raw, err := hostAuthGet(e.AuthIndex)
	if err != nil {
		row.Status = "读取失败"
		row.Err = truncate(err.Error(), 160)
		return row
	}
	sa, err := parseStored(raw)
	if err != nil {
		row.Status = "凭据解析失败"
		row.Err = truncate(err.Error(), 160)
		return row
	}
	row.UID = sa.Account.UID
	if row.Label == "" {
		row.Label = sa.displayName()
	}

	cacheKey := firstNonEmpty(e.AuthIndex, e.Name)
	creditCacheMu.RLock()
	cached, ok := creditCache[cacheKey]
	creditCacheMu.RUnlock()
	if ok && !force && time.Since(cached.at) < creditCacheTTL {
		row.Credits = cached.credits
		row.Items = cached.items
		return row
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	quota, err := newClient(currentConfig()).fetchQuota(ctx, sa)
	if err != nil {
		if isAuthError(err) {
			row.Status = "登录已失效，需重新登录"
		} else {
			row.Status = "查询失败"
		}
		row.Err = truncate(err.Error(), 160)
		return row
	}
	row.Credits = quota.TotalCreditsRemaining
	row.Items = quota.CreditItems

	creditCacheMu.Lock()
	creditCache[cacheKey] = cachedCredits{credits: row.Credits, items: row.Items, at: time.Now()}
	creditCacheMu.Unlock()

	note := fmt.Sprintf("龙虾 · 剩余 %s 积分", formatCredits(row.Credits))
	if err := persistNote(firstNonEmpty(e.Path, e.Name), raw, note); err != nil {
		logWarn("persist credit note failed", map[string]any{"name": e.Name, "error": truncate(err.Error(), 160)})
	} else {
		row.Status = "ok（已同步到凭据备注）"
	}
	return row
}

// persistNote rewrites the credential JSON with a human-readable balance note.
//
// The file is written directly (atomic temp+rename) rather than through
// host.auth.save: the host's save path re-parses auth files with its generic
// builder, which derives the credential label from the "type" field and would
// reset a friendly label back to the provider key. Writing the file lets the
// host watcher reload it through this plugin's own auth.parse, which keeps the
// label (nickname) intact.
func persistNote(path string, raw []byte, note string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("empty auth file path")
	}
	if !strings.HasSuffix(strings.ToLower(path), ".json") {
		// Fall back to the host save path for non-file (runtime) credentials.
		return fmt.Errorf("credential %s is not backed by a json file", filepath.Base(path))
	}
	if current, errRead := os.ReadFile(path); errRead == nil && len(current) > 0 {
		// Prefer the freshest on-disk payload so a concurrent token refresh is
		// not clobbered by our stale copy.
		raw = current
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("decode auth file: %w", err)
	}
	quoted, err := json.Marshal(note)
	if err != nil {
		return err
	}
	if prev, ok := doc["note"]; ok && string(prev) == string(quoted) {
		return nil
	}
	doc["note"] = quoted
	out, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------------------------------------------------------------------------
// host callbacks
// ---------------------------------------------------------------------------

func hostAuthList() ([]pluginapi.HostAuthFileEntry, error) {
	rawEnv, err := hostCall(pluginabi.MethodHostAuthList, nil)
	if err != nil {
		return nil, err
	}
	result, err := unwrapHostResult(rawEnv)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, err
	}
	return resp.Files, nil
}

func hostAuthGet(authIndex string) ([]byte, error) {
	body, err := json.Marshal(pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return nil, err
	}
	rawEnv, err := hostCall(pluginabi.MethodHostAuthGet, body)
	if err != nil {
		return nil, err
	}
	result, err := unwrapHostResult(rawEnv)
	if err != nil {
		return nil, err
	}
	var resp struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, err
	}
	if len(resp.JSON) == 0 {
		return nil, fmt.Errorf("credential %s has no payload", authIndex)
	}
	return resp.JSON, nil
}

func unwrapHostResult(raw []byte) (json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode host envelope: %w", err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host returned not-ok")
	}
	return env.Result, nil
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

var creditsTemplate = template.Must(template.New("credits").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>LobsterAI 额度</title>
<style>
:root{color-scheme:light dark}
body{font:14px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC",sans-serif;margin:0;padding:24px;background:#f6f7f9;color:#1a1a1a}
@media (prefers-color-scheme:dark){body{background:#16181d;color:#e8e8ea}.card{background:#1f2229!important;border-color:#2c3038!important}th{color:#9aa0aa!important}td,th{border-color:#2c3038!important}}
h1{font-size:18px;margin:0 0 4px}
.sub{color:#6b7280;font-size:12px;margin-bottom:16px}
.card{background:#fff;border:1px solid #e5e7eb;border-radius:10px;padding:16px;margin-bottom:16px}
table{border-collapse:collapse;width:100%}
th,td{text-align:left;padding:8px 10px;border-bottom:1px solid #eef0f3;vertical-align:top}
th{font-weight:600;color:#6b7280;font-size:12px;text-transform:uppercase;letter-spacing:.04em}
.num{font-variant-numeric:tabular-nums;font-weight:600}
.big{font-size:26px;font-weight:700;font-variant-numeric:tabular-nums}
.ok{color:#15803d}.bad{color:#b91c1c}.muted{color:#6b7280}
.btn{display:inline-block;padding:6px 12px;border-radius:8px;background:#2563eb;color:#fff;text-decoration:none;font-size:13px}
.btn.alt{background:#0f766e}
.notices{font-size:13px;line-height:1.9}
.err{color:#b91c1c;font-size:12px}
</style></head>
<body>
<h1>LobsterAI 额度</h1>
<div class="sub">上游 {{.Upstream}} · 客户端 {{.ClientVersion}} · 更新于 {{.GeneratedAt}} · 共 {{.Count}} 个账号</div>
<div class="sub">自动签到：{{.CheckinText}}</div>
<div class="card">
  <div class="muted" style="font-size:12px">全部账号合计</div>
  <div class="big">{{.Total}}</div>
</div>
<div class="card">
<table><thead><tr><th>账号</th><th>剩余积分</th><th>批次明细</th><th>状态</th></tr></thead><tbody>
{{range .Rows}}<tr>
  <td>{{.Label}}{{if .UID}}<div class="muted" style="font-size:12px">uid {{.UID}}</div>{{end}}</td>
  <td class="num">{{.CreditsText}}</td>
  <td>{{if .Items}}{{range .Items}}<div>{{.Label}}：{{.CreditsText}}{{if .ExpiresAt}} <span class="muted">（{{.ExpiresAt}} 到期）</span>{{end}}</div>{{end}}{{else}}<span class="muted">-</span>{{end}}</td>
  <td>{{if .Err}}<span class="bad">{{.Status}}</span><div class="err">{{.Err}}</div>{{else}}<span class="ok">{{.Status}}</span>{{end}}</td>
</tr>{{else}}<tr><td colspan="4" class="muted">还没有 LobsterAI 账号。请在「AI 提供商 / 凭据」里添加，或用浏览器登录。</td></tr>{{end}}
</tbody></table>
</div>
{{if .Notices}}<div class="card notices">{{range .Notices}}<div>{{.}}</div>{{end}}</div>{{end}}
<div style="display:flex;gap:8px">
  <a class="btn" href="?refresh=1">刷新额度</a>
  <a class="btn alt" href="?checkin=1">每日签到</a>
</div>
</body></html>`))

type creditsView struct {
	CheckinText   string
	Notices       []string
	Upstream      string
	ClientVersion string
	GeneratedAt   string
	Count         int
	Total         string
	Rows          []creditRowView
}

type creditRowView struct {
	Label       string
	UID         string
	CreditsText string
	Items       []creditItemView
	Status      string
	Err         string
}

type creditItemView struct {
	Label       string
	CreditsText string
	ExpiresAt   string
}

func renderCreditsPage(rows []accountCredits, notices []string) string {
	view := creditsView{
		CheckinText:   checkinSummaryText(),
		Notices:       notices,
		Upstream:      currentConfig().upstreamBase(),
		ClientVersion: currentConfig().clientVersion(),
		GeneratedAt:   time.Now().Format("2006-01-02 15:04:05"),
		Count:         len(rows),
	}
	var total float64
	for _, r := range rows {
		total += r.Credits
		rv := creditRowView{
			Label:       firstNonEmpty(r.Label, r.Name, r.UID),
			UID:         r.UID,
			CreditsText: formatCredits(r.Credits),
			Status:      r.Status,
			Err:         r.Err,
		}
		for _, it := range r.Items {
			rv.Items = append(rv.Items, creditItemView{
				Label:       firstNonEmpty(it.Label, it.Type),
				CreditsText: formatCredits(it.CreditsRemaining),
				ExpiresAt:   formatExpiry(it.ExpiresAt),
			})
		}
		view.Rows = append(view.Rows, rv)
	}
	view.Total = formatCredits(total)

	var sb strings.Builder
	if err := creditsTemplate.Execute(&sb, view); err != nil {
		return "<h1>LobsterAI 额度</h1><p>渲染失败：" + htmlEscape(err.Error()) + "</p>"
	}
	return sb.String()
}

// checkinSummaryText renders the auto check-in state for the page header.
func checkinSummaryText() string {
	st := autoCheckinSnapshot()
	if !st.Enabled {
		return "已关闭（可在 config.yaml 里设置 checkin_auto: true 打开）"
	}
	if st.LastRun.IsZero() {
		return "已开启，等待首次执行"
	}
	ok, already, failed := 0, 0, 0
	for _, r := range st.LastResult {
		switch r["status"] {
		case "ok":
			ok++
		case "already":
			already++
		default:
			if _, isErr := r["error"]; isErr {
				failed++
			}
		}
	}
	parts := []string{fmt.Sprintf("已开启 · 上次 %s", st.LastRun.Format("01-02 15:04"))}
	if ok > 0 {
		parts = append(parts, fmt.Sprintf("新签到 %d", ok))
	}
	if already > 0 {
		parts = append(parts, fmt.Sprintf("已签 %d", already))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("失败 %d", failed))
	}
	if !st.NextRun.IsZero() {
		parts = append(parts, fmt.Sprintf("下次 %s", st.NextRun.Format("15:04")))
	}
	return strings.Join(parts, " · ")
}

func formatCredits(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.2f", v)
}

// formatExpiry trims the ISO timestamp down to a readable date.
func formatExpiry(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.Local().Format("2006-01-02")
	}
	if idx := strings.IndexAny(raw, "T "); idx > 0 {
		return raw[:idx]
	}
	return raw
}
