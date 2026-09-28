package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// storedAuth is the on-disk shape of one LobsterAI credential. It matches the
// nested {auth,account} layout written by upstream lobsterai2api and accepted
// (with obvious omissions tolerated) by the official desktop client.
type storedAuth struct {
	Auth    storedTokens  `json:"auth"`
	Account storedAccount `json:"account"`
	// Note is the host-rendered credential remark (e.g. current credit balance).
	Note string `json:"note,omitempty"`
}

type storedTokens struct {
	AccessToken   string `json:"accessToken"`
	RefreshToken  string `json:"refreshToken"`
	ExpiresAt     int64  `json:"expiresAt"`
	UUID          string `json:"uuid,omitempty"`
	FirstKeyfrom  string `json:"firstKeyfrom,omitempty"`
	LatestKeyfrom string `json:"latestKeyfrom,omitempty"`
}

type storedAccount struct {
	UID      string `json:"uid"`
	UserID   string `json:"userId,omitempty"`
	Nickname string `json:"nickname,omitempty"`
}

// parseStored accepts both the nested {auth,account} form and the flat
// accessToken/uid form, mirroring upstream lobsterai2api's dual-shape parser.
func parseStored(raw []byte) (*storedAuth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var sa storedAuth
	if _, nested := probe["auth"]; nested {
		if err := json.Unmarshal(raw, &sa); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
	} else {
		var flat struct {
			storedTokens
			UID      string `json:"uid"`
			UserID   string `json:"userId"`
			Nickname string `json:"nickname"`
		}
		if err := json.Unmarshal(raw, &flat); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		sa = storedAuth{
			Auth:    flat.storedTokens,
			Account: storedAccount{UID: flat.UID, UserID: flat.UserID, Nickname: flat.Nickname},
		}
	}
	if strings.TrimSpace(sa.Auth.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	if strings.TrimSpace(sa.Account.UID) == "" {
		sa.Account.UID = deriveUID(sa.Auth.AccessToken)
	}
	return &sa, nil
}

// deriveUID falls back to a stable synthetic account id when the auth blob
// carries no uid (hand-imported flat credentials).
func deriveUID(accessToken string) string {
	sum := sha256Hex(accessToken)
	if len(sum) > 16 {
		sum = sum[:16]
	}
	return "lobsterai-" + sum
}

// displayName is the user-facing label rendered by the Management Center.
func (sa *storedAuth) displayName() string {
	if sa == nil {
		return ""
	}
	if strings.TrimSpace(sa.Account.Nickname) != "" {
		return sa.Account.Nickname
	}
	if strings.TrimSpace(sa.Account.UID) != "" {
		return sa.Account.UID
	}
	return "LobsterAI"
}

// needsRefresh reports whether the access token expires within within.
func (sa *storedAuth) needsRefresh(within time.Duration) bool {
	if sa == nil {
		return true
	}
	if sa.Auth.ExpiresAt <= 0 {
		if exp := jwtExpiry(sa.Auth.AccessToken); exp > 0 {
			return time.Now().Add(within).Unix() >= exp
		}
		return false
	}
	return time.Now().Add(within).Unix() >= sa.Auth.ExpiresAt
}

// jwtExpiry decodes the exp claim of a JWT payload without verifying it.
func jwtExpiry(token string) int64 {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return 0
	}
	return claims.Exp
}

// toAuthData wraps the credential for the host. ID stays stable per account so
// multiple LobsterAI logins coexist as separate CPA credentials.
func toAuthData(sa *storedAuth) pluginapi.AuthData {
	storage, _ := json.Marshal(sa)
	uid := strings.TrimSpace(sa.Account.UID)
	if uid == "" {
		uid = "default"
	}
	fileName := authFileNamePrefix + uid + ".json"
	label := strings.TrimSpace(sa.displayName())
	return pluginapi.AuthData{
		Provider:    providerName,
		ID:          fileName,
		FileName:    fileName,
		Label:       label,
		StorageJSON: storage,
		Metadata: map[string]any{
			"type":     providerName,
			"uid":      uid,
			"nickname": sa.Account.Nickname,
			"disabled": false,
			"note":     sa.Note,
		},
		NextRefreshAfter: nextRefreshAfter(sa),
	}
}

// nextRefreshAfter tells the host when to run AuthProvider.RefreshAuth again.
// Tokens that cannot be refreshed are parked far in the future so the host does
// not spin on them.
func nextRefreshAfter(sa *storedAuth) time.Time {
	if sa == nil {
		return time.Now().UTC().Add(24 * time.Hour)
	}
	if strings.TrimSpace(sa.Auth.RefreshToken) == "" {
		return time.Now().UTC().Add(30 * 24 * time.Hour)
	}
	if sa.Auth.ExpiresAt > 0 {
		return time.Unix(sa.Auth.ExpiresAt, 0).UTC().Add(-refreshSkew)
	}
	if exp := jwtExpiry(sa.Auth.AccessToken); exp > 0 {
		return time.Unix(exp, 0).UTC().Add(-refreshSkew)
	}
	return time.Now().UTC().Add(1 * time.Hour)
}
