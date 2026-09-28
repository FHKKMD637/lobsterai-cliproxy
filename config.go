package main

import (
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

const (
	providerName          = "lobsterai"
	authFileNamePrefix    = "lobsterai-"
	pluginVersion         = "0.3.9"
	pluginRepo            = "https://github.com/xinxinshuhao-create/lobsterai2api"
	defaultUpstreamBase   = "https://lobsterai-server.youdao.com"
	defaultLoginPortal    = "https://lobsterai.youdao.com"
	defaultClientVersion  = "2026.9.4"
	clientCapabilitiesHdr = "kimi-k3-agentic-v1"
)

// featureConfig is the plugin-owned slice of plugins.configs.lobsterai.
type featureConfig struct {
	UpstreamBase    string `yaml:"upstream_base"`
	LoginPortal     string `yaml:"login_portal"`
	ClientVersion   string `yaml:"client_version"`
	ProxyURL        string `yaml:"proxy_url"`
	ForceStream     *bool  `yaml:"force_stream"`
	RefreshSkew     string `yaml:"refresh_skew"`
	QuotaCacheSecs  int    `yaml:"quota_cache_seconds"`
	QuotaRefreshMin *int   `yaml:"quota_refresh_minutes"`
	CheckinAuto     *bool  `yaml:"checkin_auto"`
	CheckinInterval *int   `yaml:"checkin_interval_minutes"`
	// ExcludeModels hides (and refuses) specific upstream model ids so client
	// traffic that happens to request them cannot silently spend LobsterAI
	// credits. Supports an exact id or a trailing "*" prefix match.
	ExcludeModels []string `yaml:"exclude_models"`
	// AliasModels registers the listed upstream model ids under
	// "<alias_prefix><id>" instead of their bare id, so the bare id stays owned
	// by whichever other provider also serves it. The executor maps the alias
	// back to the real upstream id before forwarding.
	AliasModels     []string `yaml:"alias_models"`
	AliasPrefix     string   `yaml:"alias_prefix"`
	ModelCacheSecs  int      `yaml:"model_cache_seconds"`
	DefaultUpstream string   `yaml:"default_upstream_base"`
}

var (
	cfgMu      sync.RWMutex
	activeCfg  = defaultFeatureConfig()
	loginState sync.Map // state -> *loginSession
)

func defaultFeatureConfig() featureConfig {
	return featureConfig{
		UpstreamBase:  defaultUpstreamBase,
		LoginPortal:   defaultLoginPortal,
		ClientVersion: defaultClientVersion,
		ForceStream:   boolPtr(true),
	}
}

func boolPtr(v bool) *bool { return &v }

// loadConfig parses the plugin-owned configuration delivered on plugin.register
// and plugin.reconfigure. An unparsable document keeps the previous settings.
func loadConfig(raw []byte) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return
	}
	next := featureConfig{}
	if err := yaml.Unmarshal(raw, &next); err != nil {
		logWarn("config parse failed, keeping previous configuration", map[string]any{"error": err.Error()})
		return
	}
	cfgMu.Lock()
	activeCfg = next
	cfgMu.Unlock()
	logInfo("plugin config loaded", map[string]any{
		"raw_bytes":      len(raw),
		"upstream_base":  next.upstreamBase(),
		"client_version": next.clientVersion(),
		"exclude_models": next.ExcludeModels,
		"checkin_auto":   next.checkinAuto(),
		"quota_minutes":  next.quotaRefreshMinutes(),
	})
}

// modelExcluded reports whether the model id must not be served by this plugin.
func modelExcluded(model string) bool {
	model = strings.TrimSpace(strings.ToLower(model))
	if model == "" {
		return false
	}
	for _, rule := range currentConfig().ExcludeModels {
		rule = strings.TrimSpace(strings.ToLower(rule))
		if rule == "" {
			continue
		}
		if strings.HasSuffix(rule, "*") {
			if strings.HasPrefix(model, strings.TrimSuffix(rule, "*")) {
				return true
			}
			continue
		}
		if model == rule {
			return true
		}
	}
	return false
}

// aliasFor returns the client-visible id for an upstream model, or the model
// itself when it is not aliased.
func aliasFor(model string) string {
	if !aliasableModel(model) {
		return model
	}
	return currentConfig().aliasPrefix() + model
}

// resolveUpstreamModel reverses aliasFor: a client-visible alias maps back to the real
// upstream model id.
func resolveUpstreamModel(model string) string {
	model = strings.TrimSpace(model)
	prefix := currentConfig().aliasPrefix()
	if prefix == "" || !strings.HasPrefix(model, prefix) {
		return model
	}
	base := strings.TrimPrefix(model, prefix)
	if aliasableModel(base) {
		return base
	}
	return model
}

func aliasableModel(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, m := range currentConfig().AliasModels {
		if strings.EqualFold(strings.TrimSpace(m), model) {
			return true
		}
	}
	return false
}

func (c featureConfig) aliasPrefix() string {
	if v := strings.TrimSpace(c.AliasPrefix); v != "" {
		return v
	}
	return "lobsterai-"
}

func currentConfig() featureConfig {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return activeCfg
}

func (c featureConfig) upstreamBase() string {
	if v := strings.TrimSpace(c.UpstreamBase); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultUpstreamBase
}

func (c featureConfig) loginPortal() string {
	if v := strings.TrimSpace(c.LoginPortal); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultLoginPortal
}

func (c featureConfig) clientVersion() string {
	if v := strings.TrimSpace(c.ClientVersion); v != "" {
		return v
	}
	return defaultClientVersion
}

func (c featureConfig) forceStream() bool {
	if c.ForceStream == nil {
		return true
	}
	return *c.ForceStream
}

func (c featureConfig) proxyURL() string {
	return strings.TrimSpace(c.ProxyURL)
}

// checkinAuto reports whether the background daily check-in is enabled.
// Unset defaults to enabled; 0/false disables it.
func (c featureConfig) checkinAuto() bool {
	if c.CheckinAuto == nil {
		return true
	}
	return *c.CheckinAuto
}

// checkinIntervalMinutes is the retry cadence while today's reward is unclaimed.
func (c featureConfig) checkinIntervalMinutes() int {
	if c.CheckinInterval == nil {
		return 30
	}
	if *c.CheckinInterval < 5 {
		return 5
	}
	return *c.CheckinInterval
}

// quotaRefreshMinutes is the background credit-note refresh cadence.
// An unset value defaults to 30 minutes; 0 or a negative value disables it.
func (c featureConfig) quotaRefreshMinutes() int {
	if c.QuotaRefreshMin == nil {
		return 30
	}
	if *c.QuotaRefreshMin <= 0 {
		return 0
	}
	return *c.QuotaRefreshMin
}
