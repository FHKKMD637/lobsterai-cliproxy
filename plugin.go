// Package main implements the LobsterAI (网易有道龙虾) CLIProxyAPI dynamic
// plugin. It wraps https://lobsterai-server.youdao.com as a first-class CPA
// provider: browser OAuth login, automatic token refresh, quota-aware account
// scheduling, and OpenAI-compatible chat forwarding with SSE streaming.
//
// The upstream protocol was derived from the community reverse-engineering
// project https://github.com/xinxinshuhao-create/lobsterai2api (MIT).
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const pluginLogoURL = "https://lobsterai.youdao.com/portal/lobsterai-logo.png"

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type registrationCapability struct {
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	QuotaProvider         bool                         `json:"quota_provider"`
	ManagementAPI         bool                         `json:"management_api"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "LobsterAI",
			Version:          pluginVersion,
			Author:           "community (protocol research: xinxinshuhao-create/lobsterai2api)",
			GitHubRepository: pluginRepo,
			Logo:             pluginLogoURL,
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "upstream_base",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "LobsterAI API base URL (default https://lobsterai-server.youdao.com).",
				},
				{
					Name:        "login_portal",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "LobsterAI portal used to start browser login (default https://lobsterai.youdao.com).",
				},
				{
					Name:        "client_version",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Desktop client version reported upstream (default 2026.9.4). Raise it if the vendor gates older clients.",
				},
				{
					Name:        "proxy_url",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Optional per-plugin upstream proxy URL (empty uses the global CPA proxy).",
				},
				{
					Name:        "force_stream",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Always request SSE from upstream; LobsterAI rejects non-streaming chat requests (default true).",
				},
				{
					Name:        "quota_refresh_minutes",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "How often to refresh each credential's credit balance into its note (default 30, 0 disables).",
				},
				{
					Name:        "checkin_auto",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Description: "Automatically claim the daily LobsterAI reward (+100 credits) for every credential (default true).",
				},
				{
					Name:        "alias_models",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Upstream model ids to expose under \"<alias_prefix><id>\" (e.g. lobsterai-deepseek-flash) so the bare id stays owned by other providers.",
				},
				{
					Name:        "alias_prefix",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Prefix used for alias_models (default \"lobsterai-\").",
				},
				{
					Name:        "exclude_models",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Model ids to hide and refuse (exact id or trailing * prefix), e.g. [\"glm-5.3-flash\", \"deepseek-flash\"]. Protects LobsterAI credits from client traffic that would otherwise route here.",
				},
				{
					Name:        "checkin_interval_minutes",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Retry cadence while today's reward is unclaimed (default 30, minimum 5).",
				},
			},
		},
		Capabilities: registrationCapability{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			QuotaProvider:         true,
			ManagementAPI:         true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
		},
	}
}

// handleMethod dispatches one host RPC call to its handler.
func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		loadConfig(registrationConfigYAML(request))
		startNoteRefresher()
		startCheckinScheduler()
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		closeAllLoginSessions()
		return okEnvelope(map[string]any{"ok": true})

	case pluginabi.MethodModelStatic:
		return handleStaticModels()
	case pluginabi.MethodModelForAuth:
		return handleModelsForAuth(request)

	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case pluginabi.MethodAuthParse:
		return handleParseAuth(request)
	case pluginabi.MethodAuthLoginStart:
		return handleStartLogin(request)
	case pluginabi.MethodAuthLoginPoll:
		return handlePollLogin(request)
	case pluginabi.MethodAuthRefresh:
		return handleRefreshAuth(request)

	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case pluginabi.MethodExecutorExecute:
		return handleExecExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return handleExecStream(request)

	case pluginabi.MethodQuotaIdentifier:
		return okEnvelope(identifierResponse{Identifier: providerName})
	case pluginabi.MethodQuotaDescribe:
		return handleQuotaDescribe()
	case pluginabi.MethodQuotaFetch:
		return handleQuotaFetch(request)

	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)

	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// registrationConfigYAML extracts the plugin-owned config block delivered on
// plugin.register / plugin.reconfigure. The host encodes the field as a Go
// []byte, so the wire value is either a base64 string or a raw JSON string.
func registrationConfigYAML(request []byte) []byte {
	if len(request) == 0 {
		return nil
	}
	var payload struct {
		ConfigYAML string `json:"config_yaml"`
	}
	if err := json.Unmarshal(request, &payload); err != nil {
		return request
	}
	raw := strings.TrimSpace(payload.ConfigYAML)
	if raw == "" {
		return nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil {
		return decoded
	}
	return []byte(raw)
}

func closeAllLoginSessions() {
	loginState.Range(func(_, value any) bool {
		if ls, ok := value.(*loginSession); ok {
			ls.close()
		}
		return true
	})
}

// ---------------------------------------------------------------------------
// Quota provider
// ---------------------------------------------------------------------------

func handleQuotaDescribe() ([]byte, error) {
	return okEnvelope(pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{providerName},
		DisplayName:        "LobsterAI Credits",
	})
}

func handleQuotaFetch(raw []byte) ([]byte, error) {
	var req pluginapi.QuotaFetchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	quota, err := newClient(currentConfig()).fetchQuota(ctx, sa)
	if err != nil {
		return nil, err
	}
	return okEnvelope(quotaResponse(quota))
}

// quotaResponse normalizes the credit balance into the CPA quota contract.
func quotaResponse(q *quotaSummary) pluginapi.QuotaFetchResponse {
	total := 0.0
	if q != nil {
		total = q.TotalCreditsRemaining
	}
	remaining := 0.0
	if total > 0 {
		remaining = 1
	}
	resp := pluginapi.QuotaFetchResponse{
		Summary: []pluginapi.QuotaMetric{{
			Key:   "credits_remaining",
			Label: "剩余积分",
			Value: total,
			Unit:  "credits",
		}},
		Groups: []pluginapi.QuotaGroup{{
			DisplayName: "LobsterAI",
			Buckets: []pluginapi.QuotaBucket{{
				Window:            "account",
				RemainingFraction: remaining,
				Description:       "账户可用积分",
			}},
		}},
	}
	if q != nil && len(q.CreditItems) > 0 {
		for _, item := range q.CreditItems {
			resp.Summary = append(resp.Summary, pluginapi.QuotaMetric{
				Key:   "credit_" + sanitizeKey(firstNonEmpty(item.Label, item.Type)),
				Label: firstNonEmpty(item.Label, item.Type),
				Value: item.CreditsRemaining,
				Unit:  "credits",
			})
		}
	}
	return resp
}

func sanitizeKey(in string) string {
	out := make([]rune, 0, len(in))
	for _, r := range in {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		case r == '_' || r == '-':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
