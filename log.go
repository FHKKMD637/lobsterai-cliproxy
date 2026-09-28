package main

import (
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// logInfo / logWarn push a structured log line to the CPA host log. Failures are
// ignored: logging must never break request handling.
func logInfo(message string, fields map[string]any) {
	logWith("info", message, fields)
}

func logWarn(message string, fields map[string]any) {
	logWith("warn", message, fields)
}

func logWith(level, message string, fields map[string]any) {
	if hostAPI == nil || hostAPI.call == nil {
		return
	}
	body, err := json.Marshal(map[string]any{
		"level":   level,
		"message": message,
		"fields":  fields,
	})
	if err != nil {
		return
	}
	_, _ = hostCall(pluginabi.MethodHostLog, body)
}
