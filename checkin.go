package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const checkinPlacement = "desktop_sidebar"

// checkinSlot is the activity slot advertised by the daily check-in service.
type checkinSlot struct {
	SlotState string `json:"slotState"`
	Activity  struct {
		ActivityCode   string `json:"activityCode"`
		ConfigRevision int    `json:"configRevision"`
	} `json:"activity"`
}

// checkinContext reports whether today's reward is still claimable.
type checkinContext struct {
	State struct {
		ClaimedToday  bool    `json:"claimedToday"`
		RewardCredits float64 `json:"rewardCredits"`
		ClaimedDays   int     `json:"claimedDays"`
	} `json:"state"`
	Actions []string `json:"actions"`
}

// checkinResult summarises one check-in attempt.
type checkinResult struct {
	Status   string  `json:"status"`
	Credits  float64 `json:"credits_granted,omitempty"`
	Activity string  `json:"activity,omitempty"`
	Balance  float64 `json:"balance_after,omitempty"`
	Note     string  `json:"note,omitempty"`
}

// doRaw issues an authenticated request and unwraps the envelope, allowing
// non-JSON-free paths (activities) to be handled without the chat helpers.
func (c *client) doRaw(ctx context.Context, method, url string, sa *storedAuth, body []byte) (json.RawMessage, error) {
	var reader io.Reader
	if len(body) > 0 {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	c.authHeaders(req, sa)
	req.Header.Set("Accept", "application/json")
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
		return nil, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, &apiError{Code: env.Code, Message: env.message()}
	}
	return env.Data, nil
}

// checkin claims the daily reward for one account. Missing activity or an
// already-claimed day are reported as normal outcomes, not errors.
func (c *client) checkin(ctx context.Context, sa *storedAuth) (*checkinResult, error) {
	version := currentConfig().clientVersion()
	slotURL := fmt.Sprintf("%s/api/client-activities/slot?placement=%s&clientVersion=%s&containerApiVersion=2&platform=win32",
		c.baseURL, checkinPlacement, version)
	data, err := c.doRaw(ctx, http.MethodGet, slotURL, sa, nil)
	if err != nil {
		return nil, err
	}
	var slot checkinSlot
	if err := json.Unmarshal(data, &slot); err != nil {
		return nil, fmt.Errorf("slot parse: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(slot.SlotState), "available") || strings.TrimSpace(slot.Activity.ActivityCode) == "" {
		return &checkinResult{Status: "unavailable", Note: "当前没有可参与的签到活动"}, nil
	}
	code := slot.Activity.ActivityCode
	rev := slot.Activity.ConfigRevision

	ctxURL := fmt.Sprintf("%s/api/client-activities/%s/context?configRevision=%d", c.baseURL, code, rev)
	data, err = c.doRaw(ctx, http.MethodGet, ctxURL, sa, nil)
	if err != nil {
		return nil, err
	}
	var actx checkinContext
	if err := json.Unmarshal(data, &actx); err != nil {
		return nil, fmt.Errorf("activity context parse: %w", err)
	}
	if actx.State.ClaimedToday {
		return &checkinResult{Status: "already", Activity: code, Note: "今天已签到"}, nil
	}
	if !containsString(actx.Actions, "check_in") {
		return &checkinResult{Status: "unavailable", Activity: code, Note: "该活动当前不可签到"}, nil
	}

	payload, _ := json.Marshal(map[string]any{
		"configRevision": rev,
		"idempotencyKey": newUUID(),
		"payload":        map[string]any{},
	})
	actionURL := fmt.Sprintf("%s/api/client-activities/%s/actions/check_in", c.baseURL, code)
	data, err = c.doRaw(ctx, http.MethodPost, actionURL, sa, payload)
	if err != nil {
		return nil, err
	}
	var res struct {
		Replayed bool `json:"replayed"`
		Result   struct {
			CreditsGranted float64 `json:"creditsGranted"`
			ClaimedAt      string  `json:"claimedAt"`
		} `json:"result"`
	}
	_ = json.Unmarshal(data, &res)
	out := &checkinResult{Status: "ok", Activity: code, Credits: res.Result.CreditsGranted}
	if res.Replayed {
		out.Status = "already"
		out.Note = "今天已签到（重复提交）"
	}
	if q, errQuota := c.fetchQuota(ctx, sa); errQuota == nil {
		out.Balance = q.TotalCreditsRemaining
	}
	return out, nil
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), want) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Automatic daily check-in
// ---------------------------------------------------------------------------

// checkinGuardEntry remembers the last automatic attempt per credential so the
// scheduler does not re-claim (or hammer) once today's reward is settled.
type checkinGuardEntry struct {
	day         string // YYYY-MM-DD that was successfully claimed
	lastAttempt time.Time
	lastStatus  string
	lastError   string
}

var (
	checkinGuardMu sync.Mutex
	checkinGuard   = map[string]checkinGuardEntry{}
	checkinOnce    sync.Once

	autoCheckinMu     sync.RWMutex
	autoCheckinStatus = autoCheckinState{Enabled: true}
)

// autoCheckinState is the scheduler snapshot surfaced to the Management Center.
type autoCheckinState struct {
	Enabled    bool             `json:"enabled"`
	LastRun    time.Time        `json:"last_run,omitempty"`
	LastResult []map[string]any `json:"last_result,omitempty"`
	NextRun    time.Time        `json:"next_run,omitempty"`
}

// startCheckinScheduler runs the optional daily check-in loop. It attempts a
// catch-up pass shortly after the plugin loads, then keeps retrying on the
// configured interval until every credential has claimed today's reward.
func startCheckinScheduler() {
	checkinOnce.Do(func() {
		go func() {
			// Give the host time to register credentials after plugin load.
			time.Sleep(30 * time.Second)
			for {
				cfg := currentConfig()
				autoCheckinMu.Lock()
				autoCheckinStatus.Enabled = cfg.checkinAuto()
				autoCheckinMu.Unlock()
				if !cfg.checkinAuto() {
					time.Sleep(5 * time.Minute)
					continue
				}
				results := autoCheckinTick(cfg)
				interval := time.Duration(cfg.checkinIntervalMinutes()) * time.Minute
				autoCheckinMu.Lock()
				autoCheckinStatus.LastRun = time.Now()
				autoCheckinStatus.LastResult = results
				autoCheckinStatus.NextRun = time.Now().Add(interval)
				autoCheckinMu.Unlock()
				time.Sleep(interval)
			}
		}()
	})
}

// autoCheckinTick claims the daily reward for every credential that has not
// settled today yet. Results are the per-account summaries (also logged).
func autoCheckinTick(cfg featureConfig) []map[string]any {
	entries, err := hostAuthList()
	if err != nil {
		return []map[string]any{{"error": err.Error()}}
	}
	today := time.Now().Format("2006-01-02")
	interval := time.Duration(cfg.checkinIntervalMinutes()) * time.Minute
	now := time.Now()

	pending := make([]pluginapi.HostAuthFileEntry, 0, len(entries))
	for _, e := range entries {
		if !isLobsterAuthEntry(e) {
			continue
		}
		checkinGuardMu.Lock()
		g := checkinGuard[e.AuthIndex]
		checkinGuardMu.Unlock()
		if g.day == today && (g.lastStatus == "ok" || g.lastStatus == "already") {
			continue
		}
		if !g.lastAttempt.IsZero() && now.Sub(g.lastAttempt) < interval {
			continue
		}
		pending = append(pending, e)
	}
	if len(pending) == 0 {
		return nil
	}

	out := make([]map[string]any, 0, len(pending))
	for _, e := range pending {
		row := map[string]any{"auth_index": e.AuthIndex, "name": e.Name}
		status, errMsg := "", ""
		raw, errGet := hostAuthGet(e.AuthIndex)
		if errGet != nil {
			status, errMsg = "error", errGet.Error()
		} else if sa, errParse := parseStored(raw); errParse != nil {
			status, errMsg = "error", errParse.Error()
		} else if res, errCheckin := checkinWithTimeout(sa); errCheckin != nil {
			status, errMsg = "error", truncate(errCheckin.Error(), 200)
		} else {
			status = res.Status
			row["status"] = status
			row["credits_granted"] = res.Credits
			if res.Note != "" {
				row["note"] = res.Note
			}
		}
		if status == "error" {
			row["status"] = status
			row["error"] = errMsg
		}

		checkinGuardMu.Lock()
		g := checkinGuard[e.AuthIndex]
		g.lastAttempt = now
		g.lastStatus = status
		g.lastError = errMsg
		if status == "ok" || status == "already" {
			g.day = today
		}
		checkinGuard[e.AuthIndex] = g
		checkinGuardMu.Unlock()

		out = append(out, row)
	}

	logInfo("auto check-in finished", map[string]any{
		"attempted": len(out),
		"results":   out,
	})
	return out
}

// autoCheckinSnapshot reports the scheduler state for the management UI.
func autoCheckinSnapshot() autoCheckinState {
	autoCheckinMu.RLock()
	defer autoCheckinMu.RUnlock()
	return autoCheckinStatus
}

func checkinWithTimeout(sa *storedAuth) (*checkinResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	return newClient(currentConfig()).checkin(ctx, sa)
}
