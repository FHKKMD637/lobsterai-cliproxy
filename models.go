package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const maxCompletionTokens int64 = 32768

// modelSpec is one statically declared model. modelID is the id sent upstream;
// aliases are extra ids clients may request for the same upstream model.
type modelSpec struct {
	modelID       string
	displayName   string
	contextLength int64
}

// staticModels is the fallback table. The live list is fetched from
// GET /api/models/available per credential and cached; this table keeps the
// provider usable before the first successful discovery and for offline hosts.
var staticModels = []modelSpec{
	{modelID: "deepseek-v4-pro", displayName: "DeepSeek V4 Pro", contextLength: 131072},
	{modelID: "deepseek-v4-flash", displayName: "DeepSeek V4 Flash", contextLength: 131072},
	{modelID: "MiniMax-M3", displayName: "MiniMax M3", contextLength: 131072},
	{modelID: "MiniMax-M2.7", displayName: "MiniMax M2.7", contextLength: 131072},
	{modelID: "qwen3.7-max", displayName: "Qwen3.7 Max", contextLength: 131072},
	{modelID: "qwen3.7-plus", displayName: "Qwen3.7 Plus", contextLength: 131072},
	{modelID: "qwen3.6-plus", displayName: "Qwen3.6 Plus", contextLength: 131072},
	{modelID: "kimi-k2.7-code", displayName: "Kimi K2.7 Code", contextLength: 131072},
	{modelID: "kimi-k2.7-code-highspeed", displayName: "Kimi K2.7 Code Highspeed", contextLength: 131072},
	{modelID: "kimi-k2.6", displayName: "Kimi K2.6", contextLength: 131072},
	{modelID: "kimi-k2.5", displayName: "Kimi K2.5", contextLength: 131072},
	{modelID: "doubao-seed-2-1-pro-260628", displayName: "Doubao Seed 2.1 Pro", contextLength: 131072},
	{modelID: "doubao-seed-2-1-turbo-260628", displayName: "Doubao Seed 2.1 Turbo", contextLength: 131072},
	{modelID: "doubao-seed-2-0-code-preview-260215", displayName: "Doubao Seed 2.0 Code Preview", contextLength: 131072},
	{modelID: "glm-5.2", displayName: "GLM-5.2", contextLength: 131072},
	{modelID: "glm-5.1", displayName: "GLM-5.1", contextLength: 131072},
	{modelID: "glm-5v-turbo", displayName: "GLM-5V Turbo", contextLength: 131072},
	{modelID: "glm-5", displayName: "GLM-5", contextLength: 131072},
}

// ---------------------------------------------------------------------------
// Dynamic model cache
// ---------------------------------------------------------------------------

type modelCacheEntry struct {
	models    []modelSpec
	fetchedAt time.Time
}

var (
	modelCacheMu sync.Mutex
	modelCache   = map[string]modelCacheEntry{}
	// modelDenied records models the upstream rejected for a given account
	// (code 40301) so they are not advertised once the cache refreshes.
	modelDeniedMu sync.Mutex
	modelDenied   = map[string]map[string]struct{}{}
)

// noteModelDenied remembers that one credential cannot use model.
func noteModelDenied(uid, model string) {
	uid = strings.ToLower(strings.TrimSpace(uid))
	model = strings.TrimSpace(model)
	if uid == "" || model == "" {
		return
	}
	modelDeniedMu.Lock()
	set := modelDenied[uid]
	if set == nil {
		set = map[string]struct{}{}
		modelDenied[uid] = set
	}
	set[model] = struct{}{}
	modelDeniedMu.Unlock()
}

func isModelDenied(uid, model string) bool {
	modelDeniedMu.Lock()
	defer modelDeniedMu.Unlock()
	set := modelDenied[strings.ToLower(strings.TrimSpace(uid))]
	if set == nil {
		return false
	}
	_, denied := set[strings.TrimSpace(model)]
	return denied
}

const modelCacheTTL = time.Hour

// modelsForAuth returns the model list for one credential, refreshing the live
// list from upstream when the cache is cold or stale. Discovery failures fall
// back to the static table so the provider never becomes unusable.
func modelsForAuth(ctx context.Context, sa *storedAuth) []modelSpec {
	key := strings.ToLower(sa.Account.UID)
	if key == "" {
		key = "default"
	}
	modelCacheMu.Lock()
	entry, ok := modelCache[key]
	modelCacheMu.Unlock()
	if ok && time.Since(entry.fetchedAt) < modelCacheTTL && len(entry.models) > 0 {
		return entry.models
	}

	fetchCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	upstream, err := newClient(currentConfig()).fetchModels(fetchCtx, sa)
	if err != nil || len(upstream) == 0 {
		if err != nil {
			logWarn("model discovery failed, using static table", map[string]any{
				"uid":   key,
				"error": truncate(err.Error(), 200),
			})
		}
		if ok && len(entry.models) > 0 {
			return entry.models
		}
		return staticModels
	}

	specs := make([]modelSpec, 0, len(upstream))
	for _, m := range upstream {
		if isModelDenied(key, m.ModelID) || modelExcluded(m.ModelID) {
			continue
		}
		specs = append(specs, modelSpec{
			modelID:       m.ModelID,
			displayName:   firstNonEmpty(m.ModelName, m.ModelID),
			contextLength: 131072,
		})
	}
	modelCacheMu.Lock()
	modelCache[key] = modelCacheEntry{models: specs, fetchedAt: time.Now()}
	modelCacheMu.Unlock()
	return specs
}

// aliasDescription explains the naming for Management Center catalog entries.
func aliasDescription(m modelSpec) string {
	if aliasFor(m.modelID) == m.modelID {
		return ""
	}
	return "LobsterAI 专属别名：上游模型 " + m.modelID + "（裸 id 归其他 provider）"
}

func toModelInfos(specs []modelSpec) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(specs))
	for _, m := range specs {
		if modelExcluded(m.modelID) {
			continue
		}
		out = append(out, pluginapi.ModelInfo{
			ID:                         aliasFor(m.modelID),
			Object:                     "model",
			OwnedBy:                    providerName,
			DisplayName:                m.displayName,
			Name:                       m.modelID, // provider-native id
			Description:                aliasDescription(m),
			SupportedGenerationMethods: []string{"chat"},
			ContextLength:              m.contextLength,
			MaxCompletionTokens:        maxCompletionTokens,
			UserDefined:                true,
		})
	}
	return out
}

// handleStaticModels serves the fallback table before any credential exists.
func handleStaticModels() ([]byte, error) {
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: toModelInfos(staticModels)})
}

// handleModelsForAuth serves the per-credential list, preferring live discovery.
func handleModelsForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: toModelInfos(staticModels)})
	}
	return okEnvelope(pluginapi.ModelResponse{
		Provider: providerName,
		Models:   toModelInfos(modelsForAuth(context.Background(), sa)),
	})
}
