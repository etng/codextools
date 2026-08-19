package main

import (
	"net/http"
	"strings"
)

// Remote control is an official ChatGPT capability. It must never be routed
// through a configured model provider or have model-specific patches applied.
func isRemoteControlRequestText(values ...string) bool {
	for _, value := range values {
		text := strings.ToLower(strings.TrimSpace(value))
		if text == "" {
			continue
		}
		for _, marker := range []string{
			"remote-control",
			"remote_control",
			"remotecontrol",
			"remote control",
			"remote-control-websocket",
			"remote_control_token",
			"remote-control-device-key",
			"device-key",
			"browser-use-peer-authorization",
			"authorize-remote-control-connections",
			"set-remote-control-connections-enabled",
			"/codex/remote/control/",
			"/remote/control/",
			"/remote-control/",
			"/remote_control/",
			"enroll/start",
			"enroll/finish",
			"refresh/start",
			"refresh/finish",
			"remote_control.enroll",
		} {
			if strings.Contains(text, marker) {
				return true
			}
		}
	}
	return false
}

func isRemoteControlHTTPPath(path string) bool {
	return isRemoteControlRequestText(path)
}

func isRemoteControlHTTPRequest(req *http.Request) bool {
	if req == nil {
		return false
	}
	return isRemoteControlRequestText(req.URL.Path, req.URL.RawQuery, req.Header.Get("upgrade"))
}
