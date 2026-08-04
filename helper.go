package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func (r relayProfile) needsLocalRelayProxy() bool {
	return r.Protocol == "responses" && (disablesImageGeneration(r) || usesSeparateImageGenerationAPI(r))
}

func (r *launcherRuntime) runtimeSettingsSnapshot() backendSettings {
	r.settingsMu.RLock()
	defer r.settingsMu.RUnlock()
	return r.settings
}

func (r *launcherRuntime) setRuntimeSettings(settings backendSettings) {
	r.settingsMu.Lock()
	r.settings = settings
	r.settingsMu.Unlock()
}

func (r *launcherRuntime) relaySettingsForRequest() backendSettings {
	settings := r.runtimeSettingsSnapshot()
	return loadRuntimeRelaySettings(settings)
}

func (r *launcherRuntime) startHelper(helperPort uint16) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", r.handleHelperHTTP)
	server := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", helperPort), Handler: mux}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	r.helper = server
	r.helperURL = "http://" + server.Addr
	appendDiagnosticLog("helper.listening", map[string]any{"helper_port": helperPort, "address": r.helperURL})
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			appendDiagnosticLog("helper.failed", map[string]any{"helper_port": helperPort, "error": err.Error()})
		}
	}()
	return nil
}

func (r *launcherRuntime) shutdownHelper() {
	if r.helper == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.helper.Shutdown(ctx)
	appendDiagnosticLog("helper.shutdown", map[string]any{"address": r.helperURL})
}

func (r *launcherRuntime) startRelayProxy(port uint16) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", r.handleRelayProxyHTTP)
	server := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", port), Handler: mux}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	r.relay = server
	r.relayURL = "http://" + server.Addr
	appendDiagnosticLog("relay_proxy.listening", map[string]any{"port": port, "address": r.relayURL})
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			appendDiagnosticLog("relay_proxy.failed", map[string]any{"port": port, "error": err.Error()})
		}
	}()
	return nil
}

func (r *launcherRuntime) handleRelayProxyHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodOptions {
		writeCORSHeaders(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if websocket.IsWebSocketUpgrade(req) {
		if isRealtimeProxyPath(req.URL.Path) {
			r.forwardOfficialRealtimeWebSocket(w, req)
			return
		}
		writeHelperJSON(w, http.StatusNotFound, map[string]any{"status": "failed", "message": "未知 WebSocket 路径"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 32*1024*1024+1))
	_ = req.Body.Close()
	if err != nil {
		writeHelperJSON(w, http.StatusBadRequest, map[string]any{"status": "failed", "message": "读取请求体失败"})
		return
	}
	if len(body) > 32*1024*1024 {
		writeHelperJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"status": "failed", "message": "请求体超过本地代理限制"})
		return
	}
	if isRealtimeProxyPath(req.URL.Path) {
		r.forwardOfficialRealtimeHTTP(w, req, body)
		return
	}
	r.forwardRelayProxy(w, req, body)
}

func (r *launcherRuntime) shutdownRelayProxy() {
	if r.relay == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.relay.Shutdown(ctx)
	appendDiagnosticLog("relay_proxy.shutdown", map[string]any{"address": r.relayURL})
}

func bridgeRelayWebSockets(client, upstream *websocket.Conn) (int, int) {
	type result struct {
		direction string
		messages  int
	}
	done := make(chan result, 2)
	pipe := func(direction string, dst, src *websocket.Conn) {
		messages := 0
		for {
			messageType, data, err := src.ReadMessage()
			if err != nil {
				done <- result{direction: direction, messages: messages}
				return
			}
			if err := dst.WriteMessage(messageType, data); err != nil {
				done <- result{direction: direction, messages: messages}
				return
			}
			messages++
		}
	}
	go pipe("client", upstream, client)
	go pipe("upstream", client, upstream)
	first := <-done
	_ = client.Close()
	_ = upstream.Close()
	second := <-done
	counts := map[string]int{first.direction: first.messages, second.direction: second.messages}
	return counts["client"], counts["upstream"]
}

func (r *launcherRuntime) handleHelperHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodOptions {
		writeCORSHeaders(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if websocket.IsWebSocketUpgrade(req) {
		if isRealtimeProxyPath(req.URL.Path) {
			r.forwardOfficialRealtimeWebSocket(w, req)
			return
		}
		writeHelperJSON(w, http.StatusNotFound, map[string]any{"status": "failed", "message": "未知 WebSocket 路径"})
		return
	}
	bodyLimit := int64(32 * 1024 * 1024)
	if isAudioTranscriptionsProxyPath(req.URL.Path) {
		bodyLimit = 128 * 1024 * 1024
	}
	body, readErr := io.ReadAll(io.LimitReader(req.Body, bodyLimit+1))
	_ = req.Body.Close()
	if readErr != nil {
		writeHelperJSON(w, http.StatusBadRequest, map[string]any{"status": "failed", "message": "读取请求体失败：" + readErr.Error()})
		return
	}
	if int64(len(body)) > bodyLimit {
		writeHelperJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"status": "failed", "message": "请求体超过本地代理限制"})
		return
	}
	appendDiagnosticLog("helper.request", map[string]any{
		"method":     req.Method,
		"path":       req.URL.Path,
		"body_bytes": len(body),
		"remote":     req.RemoteAddr,
	})
	if backendRouteKnown(req.URL.Path) {
		payload := json.RawMessage(body)
		if len(payload) == 0 {
			payload = json.RawMessage(`{}`)
		}
		writeHelperJSON(w, http.StatusOK, r.handleHelperBackendRequest(req.URL.Path, payload))
		return
	}
	if isRealtimeProxyPath(req.URL.Path) {
		r.forwardOfficialRealtimeHTTP(w, req, body)
		return
	}
	switch req.URL.Path {
	case "/overlay/image":
		if req.Method != http.MethodGet {
			writeHelperJSON(w, http.StatusMethodNotAllowed, map[string]any{"status": "failed", "message": "图片覆盖层只支持 GET"})
			break
		}
		r.writeOverlayImage(w)
	case "/mobile":
		if req.Method != http.MethodGet {
			writeHelperJSON(w, http.StatusMethodNotAllowed, map[string]any{"status": "failed", "message": "手机控制页面只支持 GET"})
			break
		}
		r.writeMobilePage(w, req)
	case "/app-server/status":
		r.writeAppServerStatus(w, req)
	case "/app-server/rpc", "/app-server/ws":
		r.proxyAppServerWebSocket(w, req)
	case "/v1/responses", "/responses", "/v1/responses/compact", "/responses/compact", "/v1/models", "/models",
		"/audio/transcriptions", "/v1/audio/transcriptions", "/v1/v1/audio/transcriptions", "/codex/v1/audio/transcriptions":
		r.forwardRelayProxy(w, req, body)
	default:
		writeHelperJSON(w, http.StatusNotFound, map[string]any{"status": "failed", "message": "未知后端路径"})
	}
}

func backendRouteKnown(path string) bool {
	switch path {
	case "/backend/status", "/backend/repair",
		"/settings/get", "/settings/set", "/realtime/status",
		"/diagnostics/log",
		"/user-scripts/list", "/user-scripts/set-enabled", "/user-scripts/set-script-enabled", "/user-scripts/reload", "/user-scripts/delete",
		"/devtools/open", "/manager/open",
		"/codex-model-catalog", "/codex-config-model",
		"/zed-remote/status", "/zed-remote/resolve-host", "/zed-remote/fallback-request", "/zed-remote/open", "/zed-remote/projects", "/zed-remote/remember-project", "/zed-remote/forget-project",
		"/upstream-worktree/status", "/upstream-worktree/defaults", "/upstream-worktree/prepare", "/upstream-worktree/create",
		"/stepwise/settings", "/stepwise/generate", "/stepwise/test",
		"/delete", "/undo", "/archived-thread", "/move-thread-workspace", "/move-thread-projectless", "/export-markdown", "/thread-sort-key", "/thread-sort-keys":
		return true
	default:
		return false
	}
}

func (r *launcherRuntime) handleHelperBackendRequest(path string, payload json.RawMessage) map[string]any {
	result := r.handleBridgeRequest(path, payload)
	if _, ok := result["transport"]; !ok {
		result["transport"] = "http-helper"
	}
	return result
}

func (r *launcherRuntime) writeMobilePage(w http.ResponseWriter, req *http.Request) {
	writeCORSHeaders(w)
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	helperBase := "http://" + req.Host
	wsBase := "ws://" + req.Host
	page := `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>ChatGPT Codex Mobile</title>
  <style>
    :root { color-scheme: light dark; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: #0f172a; color: #f8fafc; }
    main { width: min(680px, calc(100vw - 32px)); }
    h1 { margin: 0 0 12px; font-size: 28px; }
    p { line-height: 1.6; color: #cbd5e1; }
    code { background: rgba(148, 163, 184, .18); padding: 2px 6px; border-radius: 6px; }
    button { border: 0; border-radius: 8px; padding: 10px 14px; font-weight: 700; background: #f8fafc; color: #0f172a; }
    pre { white-space: pre-wrap; background: rgba(15, 23, 42, .7); border: 1px solid rgba(148, 163, 184, .3); padding: 12px; border-radius: 8px; min-height: 96px; }
  </style>
</head>
<body>
  <main>
    <h1>ChatGPT Codex Mobile</h1>
    <p>本地 helper 已启用内置 mobile 入口。手机控制客户端可以连接 <code id="ws"></code>，HTTP 状态可读取 <code id="status"></code>。</p>
    <button id="check">检查连接</button>
    <pre id="out">ready</pre>
  </main>
  <script>
    const statusUrl = ` + strconv.Quote(helperBase+"/app-server/status") + `;
    const wsUrl = ` + strconv.Quote(wsBase+"/app-server/ws") + `;
    document.getElementById("status").textContent = statusUrl;
    document.getElementById("ws").textContent = wsUrl;
    document.getElementById("check").onclick = async () => {
      const out = document.getElementById("out");
      out.textContent = "checking...";
      try {
        const response = await fetch(statusUrl);
        out.textContent = JSON.stringify(await response.json(), null, 2);
      } catch (error) {
        out.textContent = String(error && error.message || error);
      }
    };
  </script>
</body>
</html>`
	_, _ = w.Write([]byte(page))
}

func (r *launcherRuntime) writeAppServerStatus(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodPost {
		writeHelperJSON(w, http.StatusMethodNotAllowed, map[string]any{"status": "failed", "message": "app-server status 只支持 GET/POST"})
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 12*time.Second)
	defer cancel()
	runtime, err := ensureMobileAppServerRuntime(ctx)
	if err != nil {
		writeHelperJSON(w, http.StatusBadGateway, map[string]any{"status": "failed", "message": err.Error(), "ready": false})
		return
	}
	writeHelperJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"ready":  true,
		"source": runtime.source,
		"port":   runtime.port,
		"url":    fmt.Sprintf("ws://127.0.0.1:%d/rpc", runtime.port),
	})
}

func (r *launcherRuntime) proxyAppServerWebSocket(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), 12*time.Second)
	defer cancel()
	upstream, err := connectMobileAppServer(ctx)
	if err != nil {
		writeHelperJSON(w, http.StatusBadGateway, map[string]any{"status": "failed", "message": err.Error()})
		return
	}
	defer upstream.Close()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	client, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	defer client.Close()
	done := make(chan struct{}, 2)
	pipe := func(dst, src *websocket.Conn) {
		defer func() { done <- struct{}{} }()
		for {
			messageType, data, err := src.ReadMessage()
			if err != nil {
				return
			}
			if err := dst.WriteMessage(messageType, data); err != nil {
				return
			}
		}
	}
	go pipe(upstream, client)
	go pipe(client, upstream)
	<-done
}

func (r *launcherRuntime) writeOverlayImage(w http.ResponseWriter) {
	settings := normalizeSettings(loadSettings())
	imagePath := strings.TrimSpace(settings.CodexAppImageOverlayPath)
	contentType := overlayImageContentType(imagePath)
	if !settings.CodexAppImageOverlayEnabled || imagePath == "" || contentType == "" {
		writeHelperJSON(w, http.StatusNotFound, map[string]any{"status": "failed", "message": "图片覆盖层未启用或图片不可用"})
		appendDiagnosticLog("helper.overlay_image_not_found", map[string]any{"reason": "disabled_or_invalid_path"})
		return
	}
	bytes, err := os.ReadFile(imagePath)
	if err != nil {
		writeHelperJSON(w, http.StatusNotFound, map[string]any{"status": "failed", "message": "图片覆盖层未启用或图片不可用"})
		appendDiagnosticLog("helper.overlay_image_not_found", map[string]any{"path": imagePath, "error": err.Error()})
		return
	}
	writeCORSHeaders(w)
	w.Header().Set("content-type", contentType)
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(bytes)
	appendDiagnosticLog("helper.overlay_image_ok", map[string]any{"path": imagePath, "bytes": len(bytes), "content_type": contentType})
}

func (r *launcherRuntime) forwardRelayProxy(w http.ResponseWriter, req *http.Request, body []byte) {
	settings := r.relaySettingsForRequest()
	requestJSON := map[string]any{}
	_ = json.Unmarshal(body, &requestJSON)
	profile, selectErr := selectRelayForRequest(settings, rotationContext{ConversationID: conversationIDFromRelayRequest(requestJSON)})
	if selectErr != nil {
		writeHelperJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": selectErr.Error()}})
		return
	}
	profiles := []relayProfile{profile}
	if fallbacks, err := fallbackRelaysAfter(settings, profile.ID); err == nil {
		profiles = append(profiles, fallbacks...)
	}
	var lastErr error
	for attempt, candidate := range profiles {
		if attempt > 0 {
			appendDiagnosticLog("relay_proxy.upstream_failover", map[string]any{
				"from_relay_id":  profile.ID,
				"to_relay_id":    candidate.ID,
				"attempt":        attempt + 1,
				"candidateCount": len(profiles),
			})
		}
		if forwardRelayProxyAttempt(settings, w, req, body, candidate, attempt+1, len(profiles)) {
			return
		}
		lastErr = errors.New("upstream returned failure")
		profile = candidate
	}
	message := "ChatGPT Codex relay proxy request failed"
	if lastErr != nil {
		message += ": " + lastErr.Error()
	}
	writeHelperJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": message}})
}

func forwardRelayProxyAttempt(settings backendSettings, w http.ResponseWriter, req *http.Request, body []byte, profile relayProfile, attempt, candidateCount int) bool {
	proxyBody, protocolContext, protocolErr := convertResponsesRequestForProfile(profile, req.URL.Path, body)
	if protocolErr != nil {
		writeHelperJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": protocolErr.Error(), "type": "invalid_request_error"}})
		recordRelayRequestFailure(settings)
		return true
	}
	body = proxyBody
	if isAudioTranscriptionsProxyPath(req.URL.Path) {
		protocolContext.Audio = true
	} else {
		var visionErr error
		body, visionErr = applyVisionHandling(req.Context(), profile, body)
		if visionErr != nil {
			writeHelperJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": visionErr.Error(), "type": "invalid_request_error"}})
			recordRelayRequestFailure(settings)
			return true
		}
	}
	baseURL := relayProxyBaseURL(effectiveUpstreamBaseURL(profile), profile.Protocol)
	apiKey := strings.TrimSpace(profile.APIKey)
	decision := relayRouteDecision{body: body, route: "text", reason: "default_text"}
	if profile.Protocol == "responses" && profile.needsLocalRelayProxy() {
		decision = decideRelayRouteForPath(req.URL.Path, body, profile)
		body = decision.body
		if decision.useImageAPI && usesSeparateImageGenerationAPI(profile) {
			baseURL = relayProxyBaseURL(profile.ImageGenerationBaseURL, profile.Protocol)
			apiKey = strings.TrimSpace(profile.ImageGenerationAPIKey)
			decision.keySource = "image"
		} else {
			decision.keySource = "default"
		}
	}
	if baseURL == "" || apiKey == "" {
		writeHelperJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": "ChatGPT Codex relay proxy missing base URL or API key"}})
		recordRelayRequestFailure(settings)
		return true
	}
	target := protocolProxyTargetURL(profile, req.URL.Path, protocolContext.Converted)
	if decision.useImageAPI {
		target = relayImageTargetURL(baseURL, req.URL.Path)
	}
	if req.URL.RawQuery != "" {
		if parsedTarget, parseErr := url.Parse(target); parseErr == nil {
			parsedTarget.RawQuery = req.URL.RawQuery
			target = parsedTarget.String()
		}
	}
	startedAt := time.Now()
	method := req.Method
	if method == "" {
		method = http.MethodPost
	}
	upstreamReq, err := http.NewRequestWithContext(req.Context(), method, target, bytes.NewReader(body))
	if err != nil {
		writeHelperJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": err.Error()}})
		recordRelayRequestFailure(settings)
		return true
	}
	upstreamReq.Header.Set("authorization", "Bearer "+apiKey)
	copyProxyHeaders(req.Header, upstreamReq.Header)
	setRelayProxyUserAgent(profile.UserAgent, req.Header, upstreamReq.Header)
	if protocolContext.Audio {
		contentType := strings.TrimSpace(req.Header.Get("content-type"))
		if contentType == "" {
			writeHelperJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "Audio transcriptions 请求缺少 Content-Type", "type": "invalid_request_error"}})
			recordRelayRequestFailure(settings)
			return true
		}
		upstreamReq.Header.Set("content-type", contentType)
	} else {
		upstreamReq.Header.Set("content-type", "application/json")
	}
	if protocolContext.Stream {
		upstreamReq.Header.Set("accept", "text/event-stream")
		upstreamReq.Header.Set("cache-control", "no-cache")
	}
	upstreamReq.Header.Set("accept-encoding", "identity")
	client, err := relayHTTPClient(profile)
	if err != nil {
		appendDiagnosticLog("relay_proxy.proxy_config_invalid", map[string]any{
			"relay_id":       profile.ID,
			"relay_name":     profile.Name,
			"target":         target,
			"attempt":        attempt,
			"candidateCount": candidateCount,
			"willFailover":   attempt < candidateCount,
			"error":          err.Error(),
		})
		recordRelayRequestFailure(settings)
		if attempt < candidateCount {
			return false
		}
		writeHelperJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": relayProxyRequestFailureMessage(err, candidateCount)}})
		return true
	}
	resp, err := client.Do(upstreamReq)
	if err != nil {
		appendDiagnosticLog("relay_proxy.request_failed", map[string]any{
			"relay_id":       profile.ID,
			"relay_name":     profile.Name,
			"target":         target,
			"attempt":        attempt,
			"candidateCount": candidateCount,
			"willFailover":   attempt < candidateCount,
			"error":          err.Error(),
		})
		recordRelayRequestFailure(settings)
		if attempt < candidateCount {
			return false
		}
		writeHelperJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": relayProxyRequestFailureMessage(err, candidateCount)}})
		return true
	}
	defer resp.Body.Close()
	recordRelayRequestEvent(settings, relayRotationEventForStatus(resp.StatusCode))
	if resp.StatusCode >= 400 && attempt < candidateCount {
		appendDiagnosticLog("relay_proxy.upstream_status_failed", map[string]any{
			"relay_id":       profile.ID,
			"relay_name":     profile.Name,
			"target":         target,
			"status":         resp.StatusCode,
			"attempt":        attempt,
			"candidateCount": candidateCount,
			"willFailover":   true,
		})
		return false
	}
	writeCORSHeaders(w)
	for _, name := range []string{"cache-control", "openai-request-id", "x-request-id"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	responseBytes, copyErr := writeProtocolProxyResponse(w, resp, protocolContext)
	logDetail := map[string]any{
		"path":                req.URL.Path,
		"status":              resp.StatusCode,
		"target":              target,
		"route":               decision.route,
		"reason":              decision.reason,
		"key_source":          decision.keySource,
		"stripped_image_tool": decision.strippedImageTool,
		"wire_api":            map[bool]string{true: "chatCompletions", false: profile.Protocol}[protocolContext.Converted],
		"relay_id":            profile.ID,
		"relay_name":          profile.Name,
		"attempt":             attempt,
		"candidateCount":      candidateCount,
		"body_bytes":          responseBytes,
		"duration_ms":         time.Since(startedAt).Milliseconds(),
	}
	if copyErr != nil {
		logDetail["copy_error"] = copyErr.Error()
	}
	appendDiagnosticLog("relay_proxy.response", logDetail)
	return true
}

func relayProxyRequestFailureMessage(err error, candidateCount int) string {
	detail := err.Error()
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		detail = "upstream disconnected before returning an HTTP response (EOF)"
	}
	message := "ChatGPT Codex relay proxy request failed: " + detail
	if candidateCount <= 1 {
		return message + "; no failover candidate is configured"
	}
	return message + "; all configured failover candidates failed"
}

func relayRotationEventForStatus(statusCode int) rotationEvent {
	if statusCode >= 200 && statusCode < 300 {
		return rotationEventSuccess
	}
	return rotationEventFailure
}

func conversationIDFromRelayRequest(body map[string]any) string {
	for _, key := range []string{"conversation", "conversation_id", "previous_response_id"} {
		if value := strings.TrimSpace(stringFromAny(body[key])); value != "" {
			return value
		}
	}
	return ""
}

func effectiveUpstreamBaseURL(profile relayProfile) string {
	return strings.TrimSpace(firstNonEmpty(profile.UpstreamBaseURL, profile.BaseURL))
}

func relayProxyBaseURL(baseURL, protocol string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if protocol == "responses" {
		return normalizeResponsesBaseURL(trimmed)
	}
	return trimmed
}

func relayTargetURL(baseURL, path string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	path = relayUpstreamPath(path)
	if path == "" {
		return baseURL
	}
	if strings.HasSuffix(baseURL, path) {
		return baseURL
	}
	return baseURL + path
}

func relayImageTargetURL(baseURL, path string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if relayURLHasCompleteImageEndpoint(baseURL) {
		return baseURL
	}
	return relayTargetURL(baseURL, path)
}

func relayUpstreamPath(path string) string {
	path = "/" + strings.TrimLeft(strings.TrimSpace(path), "/")
	if path == "/" || path == "/v1" {
		return ""
	}
	if strings.HasPrefix(path, "/v1/") {
		return strings.TrimPrefix(path, "/v1")
	}
	return path

}

func relayURLHasCompleteImageEndpoint(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	path := strings.TrimRight(parsed.Path, "/")
	return path == "/responses" || strings.HasSuffix(path, "/responses") ||
		path == "/images/generations" || strings.HasSuffix(path, "/images/generations") ||
		path == "/images/edits" || strings.HasSuffix(path, "/images/edits") ||
		path == "/images/variations" || strings.HasSuffix(path, "/images/variations")
}

func copyProxyHeaders(source http.Header, target http.Header) {
	for name, values := range source {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "host" || lower == "connection" || lower == "content-length" || lower == "user-agent" || lower == "accept-encoding" {
			continue
		}
		target.Del(name)
		for _, value := range values {
			target.Add(name, value)
		}
	}
}

func setRelayProxyUserAgent(configured string, source http.Header, target http.Header) {
	userAgent := strings.TrimSpace(configured)
	if userAgent == "" {
		userAgent = strings.TrimSpace(source.Get("user-agent"))
	}
	if userAgent == "" {
		userAgent = "Codex"
	}
	target.Set("user-agent", userAgent)
}

func flushRelayResponseHeaders(w http.ResponseWriter) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	flusher.Flush()
}

func copyRelayResponseBody(w http.ResponseWriter, body io.Reader) (int64, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return io.Copy(w, body)
	}
	buffer := make([]byte, 32*1024)
	var written int64
	for {
		read, readErr := body.Read(buffer)
		if read > 0 {
			copied, writeErr := w.Write(buffer[:read])
			if copied > 0 {
				written += int64(copied)
				flusher.Flush()
			}
			if writeErr != nil {
				return written, writeErr
			}
			if copied != read {
				return written, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, readErr
		}
	}
}

func decideRelayRoute(body []byte, profile relayProfile) relayRouteDecision {
	return decideRelayRouteForPath("", body, profile)
}

func decideRelayRouteForPath(path string, body []byte, profile relayProfile) relayRouteDecision {
	decision := relayRouteDecision{body: body, route: "text", reason: "default_text", keySource: "default"}
	var value map[string]any
	if json.Unmarshal(body, &value) != nil {
		if len(bytes.TrimSpace(body)) == 0 {
			decision.reason = "empty_body"
		} else {
			decision.reason = "invalid_json"
		}
		if usesSeparateImageGenerationAPI(profile) && relayPathRequestsImageGeneration(path) {
			decision.useImageAPI = true
			decision.route = "image"
			decision.reason = "image_endpoint"
			decision.keySource = "image"
		}
		return decision
	}

	if !profile.ImageGenerationEnabled {
		decision.body, decision.strippedImageTool = stripImageGenerationTools(value, body)
		if decision.strippedImageTool {
			decision.reason = "image_disabled"
		}
		return decision
	}

	if usesSeparateImageGenerationAPI(profile) {
		if relayPathRequestsImageGeneration(path) {
			decision.useImageAPI = true
			decision.route = "image"
			decision.reason = "image_endpoint"
			decision.keySource = "image"
			return decision
		}
		if relayToolChoiceRequestsImage(value["tool_choice"]) {
			decision.useImageAPI = true
			decision.route = "image"
			decision.reason = "tool_choice_image"
			decision.keySource = "image"
			return decision
		}
		latestUserTexts := relayLatestUserTextFragments(value["input"])
		if len(latestUserTexts) == 0 && relayBodyContainsImageGenerationCall(value) {
			decision.useImageAPI = true
			decision.route = "image"
			decision.reason = "image_generation_call"
			decision.keySource = "image"
			return decision
		}
		if relayBodyDeclaresImageGenerationTool(value) && (relayTextFragmentsRequestImage(latestUserTexts, value["input"]) ||
			relayLatestUserInputContainsImage(value["input"]) && relayTextFragmentsRequestImageEdit(latestUserTexts, value["input"])) {
			decision.useImageAPI = true
			decision.route = "image"
			decision.reason = "latest_user_image_intent"
			decision.keySource = "image"
			return decision
		}
	}

	decision.body, decision.strippedImageTool = stripImageGenerationTools(value, body)
	if decision.strippedImageTool {
		decision.reason = "text_with_image_tool_stripped"
	}
	return decision
}

func relayPathRequestsImageGeneration(path string) bool {
	path = strings.ToLower(strings.TrimRight("/"+strings.TrimLeft(strings.TrimSpace(path), "/"), "/"))
	return strings.HasSuffix(path, "/images/generations") || strings.HasSuffix(path, "/images/edits") || strings.HasSuffix(path, "/images/variations")
}

func relayBodyDeclaresImageGenerationTool(value map[string]any) bool {
	tools, ok := value["tools"].([]any)
	if !ok {
		return false
	}
	for _, tool := range tools {
		if relayToolIsImageGeneration(tool) {
			return true
		}
	}
	return false
}

func stripImageGenerationTools(value map[string]any, fallback []byte) ([]byte, bool) {
	stripped := false
	if tools, ok := value["tools"].([]any); ok && len(tools) > 0 {
		filtered := make([]any, 0, len(tools))
		for _, tool := range tools {
			if relayToolIsImageGeneration(tool) {
				stripped = true
				continue
			}
			filtered = append(filtered, tool)
		}
		if stripped {
			value["tools"] = filtered
		}
	}
	if relayToolChoiceRequestsImage(value["tool_choice"]) {
		delete(value, "tool_choice")
		stripped = true
	}
	if !stripped {
		return fallback, false
	}
	updated, err := json.Marshal(value)
	if err != nil {
		return fallback, false
	}
	return updated, true
}

func relayToolIsImageGeneration(tool any) bool {
	object, ok := tool.(map[string]any)
	if !ok {
		return false
	}
	return relayImageKind(stringFromAny(firstNonNil(object["type"], object["name"])))
}

func relayToolChoiceRequestsImage(choice any) bool {
	switch value := choice.(type) {
	case string:
		return relayImageKind(value)
	case map[string]any:
		return relayImageKind(stringFromAny(firstNonNil(value["type"], value["name"])))
	default:
		return false
	}
}

func relayBodyContainsImageGenerationCall(value map[string]any) bool {
	for key, item := range value {
		if key == "tools" || key == "tool_choice" {
			continue
		}
		if relayNodeContainsImageGenerationCall(item) {
			return true
		}
	}
	return false
}

func relayNodeContainsImageGenerationCall(node any) bool {
	switch value := node.(type) {
	case map[string]any:
		kind := strings.ToLower(stringFromAny(firstNonNil(value["type"], value["name"])))
		if strings.Contains(kind, "image_generation_call") {
			return true
		}
		for key, item := range value {
			if key == "tools" || key == "tool_choice" {
				continue
			}
			if relayNodeContainsImageGenerationCall(item) {
				return true
			}
		}
	case []any:
		for _, item := range value {
			if relayNodeContainsImageGenerationCall(item) {
				return true
			}
		}
	}
	return false
}

func relayLatestUserInputRequestsImage(input any) bool {
	texts := relayLatestUserTextFragments(input)
	return relayTextFragmentsRequestImage(texts, input)
}

func relayTextFragmentsRequestImage(texts []string, fallback any) bool {
	if len(texts) == 0 {
		texts = relayTextFragments(fallback)
	}
	for _, text := range texts {
		if relayTextRequestsImage(text) {
			return true
		}
	}
	return false
}

func relayTextFragmentsRequestImageEdit(texts []string, fallback any) bool {
	if len(texts) == 0 {
		texts = relayTextFragments(fallback)
	}
	for _, text := range texts {
		if relayTextRequestsImageEdit(text) {
			return true
		}
	}
	return false
}

func relayLatestUserTextFragments(input any) []string {
	messages, ok := input.([]any)
	if !ok {
		return nil
	}
	for index := len(messages) - 1; index >= 0; index-- {
		message, ok := messages[index].(map[string]any)
		if !ok {
			continue
		}
		if strings.ToLower(stringFromAny(message["role"])) != "user" {
			continue
		}
		texts := relayTextFragments(firstNonNil(message["content"], message["text"], message["input"], message["prompt"]))
		if len(texts) > 0 {
			return texts
		}
	}
	return nil
}

func relayLatestUserInputContainsImage(input any) bool {
	messages, ok := input.([]any)
	if !ok {
		return relayNodeContainsImageInput(input)
	}
	for index := len(messages) - 1; index >= 0; index-- {
		message, ok := messages[index].(map[string]any)
		if !ok || strings.ToLower(stringFromAny(message["role"])) != "user" {
			continue
		}
		return relayNodeContainsImageInput(firstNonNil(message["content"], message["input"]))
	}
	return relayNodeContainsImageInput(input)
}

func relayNodeContainsImageInput(node any) bool {
	switch value := node.(type) {
	case []any:
		for _, child := range value {
			if relayNodeContainsImageInput(child) {
				return true
			}
		}
	case map[string]any:
		kind := strings.ToLower(stringFromAny(value["type"]))
		if kind == "input_image" || kind == "image_url" {
			return true
		}
		if stringFromAny(value["image_url"]) != "" && !strings.Contains(kind, "output") {
			return true
		}
		for _, key := range []string{"content", "input"} {
			if relayNodeContainsImageInput(value[key]) {
				return true
			}
		}
	}
	return false
}

func relayTextFragments(node any) []string {
	var fragments []string
	var walk func(any)
	walk = func(item any) {
		switch value := item.(type) {
		case string:
			fragments = append(fragments, value)
		case []any:
			for _, child := range value {
				walk(child)
			}
		case map[string]any:
			if kind := strings.ToLower(stringFromAny(value["type"])); kind != "" && strings.Contains(kind, "image") && !strings.Contains(kind, "text") {
				return
			}
			for key, child := range value {
				switch key {
				case "text", "content", "input", "prompt":
					walk(child)
				}
			}
		}
	}
	walk(node)
	return fragments
}

func relayTextRequestsImage(text string) bool {
	normalized := strings.ToLower(strings.TrimSpace(text))
	if normalized == "" {
		return false
	}
	for _, negative := range []string{
		"不要生成图片", "不生成图片", "无需生成图片", "不用生成图片", "不要画图", "无需画图", "不用画图",
		"do not generate an image", "do not generate images", "don't generate an image", "don't generate images",
		"without generating an image", "without generating images", "no image generation",
	} {
		if strings.Contains(normalized, negative) {
			return false
		}
	}
	return relayChineseTextRequestsImage(normalized) || relayEnglishTextRequestsImage(normalized)
}

func relayTextRequestsImageEdit(text string) bool {
	normalized := strings.ToLower(strings.TrimSpace(text))
	if normalized == "" {
		return false
	}
	for _, negative := range []string{
		"不要编辑图片", "不修改图片", "无需修改图片", "不用修改图片",
		"do not edit the image", "don't edit the image", "do not modify the image", "without editing the image",
	} {
		if strings.Contains(normalized, negative) {
			return false
		}
	}
	return relayChineseTextRequestsImageEdit(normalized) || relayEnglishTextRequestsImageEdit(normalized)
}

func relayChineseTextRequestsImage(text string) bool {
	actions := []string{"生成", "画", "绘制", "创建", "设计", "做", "制作"}
	targets := []string{"架构图", "示意图", "缩略图", "图片", "图像", "图标", "插画", "海报", "照片", "头像", "封面", "logo", "图"}
	for _, action := range actions {
		for search := 0; search < len(text); {
			actionIndex := strings.Index(text[search:], action)
			if actionIndex < 0 {
				break
			}
			actionIndex += search
			if relayChineseImageActionPrefixAllowed(text[:actionIndex]) {
				targetIndex, target := relayFirstTextTarget(text, actionIndex+len(action), targets, false)
				if targetIndex >= 0 && relayChineseImageObjectGapAllowed(text[actionIndex+len(action):targetIndex]) && relayChineseImageTargetSuffixAllowed(text[targetIndex+len(target):]) {
					return true
				}
				if targetIndex < 0 && (action == "画" || action == "绘制") && relayChineseDrawObjectAllowed(text[:actionIndex], text[actionIndex+len(action):]) {
					return true
				}
			}
			search = actionIndex + len(action)
		}
	}
	return false
}

func relayChineseTextRequestsImageEdit(text string) bool {
	actions := []string{"编辑", "修改", "修复"}
	targets := []string{"架构图", "示意图", "缩略图", "图片", "图像", "图标", "插画", "海报", "照片", "头像", "封面", "logo", "图"}
	for _, action := range actions {
		for search := 0; search < len(text); {
			actionIndex := strings.Index(text[search:], action)
			if actionIndex < 0 {
				break
			}
			actionIndex += search
			if relayChineseImageActionPrefixAllowed(text[:actionIndex]) {
				targetIndex, target := relayFirstTextTarget(text, actionIndex+len(action), targets, false)
				if targetIndex >= 0 && relayChineseImageObjectGapAllowed(text[actionIndex+len(action):targetIndex]) && relayChineseImageTargetSuffixAllowed(text[targetIndex+len(target):]) {
					return true
				}
			}
			search = actionIndex + len(action)
		}
	}
	return false
}

func relayEnglishTextRequestsImage(text string) bool {
	actions := []string{"generate", "create", "draw", "make", "design"}
	targets := []string{
		"illustrations", "illustration", "thumbnails", "thumbnail", "pictures", "picture", "diagrams", "diagram",
		"graphics", "graphic", "artworks", "artwork", "posters", "poster", "avatars", "avatar",
		"images", "image", "icons", "icon", "logos", "logo", "photo", "photos",
	}
	for _, action := range actions {
		for search := 0; search < len(text); {
			actionIndex := relayASCIIWordIndex(text, action, search)
			if actionIndex < 0 {
				break
			}
			if relayEnglishImageActionPrefixAllowed(text[:actionIndex]) {
				targetIndex, target := relayFirstTextTarget(text, actionIndex+len(action), targets, true)
				if targetIndex >= 0 && relayEnglishImageObjectGapAllowed(text[actionIndex+len(action):targetIndex]) && relayEnglishImageTargetSuffixAllowed(text[targetIndex+len(target):]) {
					return true
				}
				if targetIndex < 0 && action == "draw" && relayEnglishDrawObjectAllowed(text[:actionIndex], text[actionIndex+len(action):]) {
					return true
				}
			}
			search = actionIndex + len(action)
		}
	}
	return false
}

func relayEnglishTextRequestsImageEdit(text string) bool {
	actions := []string{"edit", "modify", "transform", "fix"}
	targets := []string{
		"illustrations", "illustration", "thumbnails", "thumbnail", "pictures", "picture", "diagrams", "diagram",
		"graphics", "graphic", "artworks", "artwork", "posters", "poster", "avatars", "avatar",
		"images", "image", "icons", "icon", "logos", "logo", "photo", "photos",
	}
	for _, action := range actions {
		for search := 0; search < len(text); {
			actionIndex := relayASCIIWordIndex(text, action, search)
			if actionIndex < 0 {
				break
			}
			if relayEnglishImageActionPrefixAllowed(text[:actionIndex]) {
				targetIndex, target := relayFirstTextTarget(text, actionIndex+len(action), targets, true)
				if targetIndex >= 0 && relayEnglishImageObjectGapAllowed(text[actionIndex+len(action):targetIndex]) && relayEnglishImageTargetSuffixAllowed(text[targetIndex+len(target):]) {
					return true
				}
			}
			search = actionIndex + len(action)
		}
	}
	return false
}

func relayFirstTextTarget(text string, start int, targets []string, asciiWords bool) (int, string) {
	bestIndex := -1
	bestTarget := ""
	for _, target := range targets {
		index := strings.Index(text[start:], target)
		if index < 0 {
			continue
		}
		index += start
		if asciiWords && !relayASCIIWordAt(text, index, len(target)) {
			continue
		}
		if bestIndex < 0 || index < bestIndex || index == bestIndex && len(target) > len(bestTarget) {
			bestIndex = index
			bestTarget = target
		}
	}
	return bestIndex, bestTarget
}

func relayEnglishImageActionPrefixAllowed(text string) bool {
	prefix := relayImageRequestClausePrefix(text)
	for _, allowed := range []string{
		"", "please", "please help me", "help me", "can you", "can you please", "could you", "could you please",
		"would you", "will you", "i want you to", "i'd like you to", "use imagegen to", "please use imagegen to",
	} {
		if prefix == allowed {
			return true
		}
	}
	return false
}

func relayChineseImageActionPrefixAllowed(text string) bool {
	prefix := relayImageRequestClausePrefix(text)
	for _, allowed := range []string{
		"", "请", "请你", "帮我", "请帮我", "给我", "请给我", "能否", "可以", "可以帮我", "麻烦", "麻烦帮我",
		"我想", "我要", "我希望你", "使用imagegen", "请使用imagegen", "使用技能imagegen", "请使用技能imagegen",
	} {
		if prefix == allowed {
			return true
		}
	}
	return false
}

func relayImageRequestClausePrefix(text string) string {
	if index := strings.LastIndexAny(text, ".!?;\n。！？；"); index >= 0 {
		text = text[index+1:]
	}
	return strings.Trim(strings.TrimSpace(text), " ,，:-")
}

func relayEnglishImageTargetSuffixAllowed(text string) bool {
	suffix := strings.TrimSpace(strings.TrimLeft(text, " ,.!?;:)]}'\""))
	if suffix == "" {
		return true
	}
	for _, technical := range []string{"in code", "in react", "in html", "in css", "in svg", "using code", "using canvas", "with code"} {
		if strings.HasPrefix(suffix, technical) {
			return false
		}
	}
	for _, prefix := range []string{
		"of ", "for ", "showing ", "depicting ", "that shows ", "which shows ", "with ", "in the style of ",
		"based on ", "at ", "please", "for me", "now",
	} {
		if strings.HasPrefix(suffix, prefix) {
			return true
		}
	}
	return false
}

func relayEnglishImageObjectGapAllowed(gap string) bool {
	if strings.ContainsAny(gap, ".!?;:\n") {
		return false
	}
	words := strings.Fields(strings.Trim(gap, " ,()[]{}'\""))
	if len(words) > 8 {
		return false
	}
	for _, word := range words {
		switch strings.Trim(word, " ,()[]{}'\"") {
		case "for", "to", "of", "from", "that", "which", "where", "whose", "into", "about",
			"docker", "container", "oci", "vm", "virtual", "machine", "disk", "system", "base", "iso":
			return false
		}
	}
	return true
}

func relayChineseImageObjectGapAllowed(gap string) bool {
	if strings.ContainsAny(gap, "。！？；：.!?;:\n") {
		return false
	}
	for _, relation := range []string{"用于", "为了", "给", "为", "关于", "针对", "支持", "处理", "加载", "上传", "写入"} {
		if strings.Contains(gap, relation) {
			return false
		}
	}
	return true
}

func relayEnglishDrawObjectAllowed(prefix, text string) bool {
	object := strings.TrimSpace(strings.Trim(text, " ,.!?;:()[]{}'\""))
	if object == "" {
		return false
	}
	requestPrefix := relayImageRequestClausePrefix(prefix)
	if !strings.Contains(requestPrefix, "imagegen") && !strings.HasPrefix(object, "me ") {
		return false
	}
	for _, technical := range []string{"data", "record", "information", "code", "canvas", "svg", "html", "css", "react", "component", "function", "api"} {
		if strings.Contains(object, technical) {
			return false
		}
	}
	return true
}

func relayChineseDrawObjectAllowed(prefix, text string) bool {
	object := strings.TrimSpace(strings.Trim(text, " ，。！？；：,.!?;:()[]{}'\""))
	if object == "" {
		return false
	}
	requestPrefix := relayImageRequestClausePrefix(prefix)
	if !strings.Contains(requestPrefix, "imagegen") && requestPrefix != "帮我" && requestPrefix != "请帮我" && requestPrefix != "给我" && requestPrefix != "请给我" {
		return false
	}
	for _, technical := range []string{"数据", "记录", "信息", "代码", "画布", "组件", "函数", "接口", "api", "svg", "html", "css", "react"} {
		if strings.Contains(object, technical) {
			return false
		}
	}
	return true
}

func relayChineseImageTargetSuffixAllowed(text string) bool {
	suffix := strings.TrimSpace(strings.TrimLeft(text, " ，。！？；：,.!?;:"))
	if suffix == "" {
		return true
	}
	for _, prefix := range []string{
		"展示", "描绘", "表现", "用于", "作为", "关于", "包含", "带有", "风格", "比例", "尺寸", "分辨率", "背景", "颜色", "请", "给我", "一下", "吧",
	} {
		if strings.HasPrefix(suffix, prefix) {
			return true
		}
	}
	return false
}

func relayASCIIWordIndex(text, word string, start int) int {
	for start < len(text) {
		index := strings.Index(text[start:], word)
		if index < 0 {
			return -1
		}
		index += start
		if relayASCIIWordAt(text, index, len(word)) {
			return index
		}
		start = index + len(word)
	}
	return -1
}

func relayASCIIWordAt(text string, index, length int) bool {
	if index > 0 && relayASCIIWordCharacter(text[index-1]) {
		return false
	}
	end := index + length
	return end >= len(text) || !relayASCIIWordCharacter(text[end])
}

func relayASCIIWordCharacter(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || value == '_'
}

func relayImageKind(kind string) bool {
	normalized := strings.ToLower(strings.TrimSpace(kind))
	return normalized == "image_generation" || normalized == "image_generation_call"
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func (r *launcherRuntime) logRendererDiagnostic(raw json.RawMessage) {
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		detail = map[string]any{"parse_error": err.Error(), "raw_bytes": len(raw)}
	}
	event := sanitizeDiagnosticEvent(stringFromAny(detail["event"]))
	appendDiagnosticLog("renderer."+event, detail)
}

func writeCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("access-control-allow-origin", "*")
	w.Header().Set("access-control-allow-methods", "GET, POST, OPTIONS")
	w.Header().Set("access-control-allow-headers", "Content-Type, Authorization, X-Requested-With, Access-Control-Request-Private-Network")
	w.Header().Set("access-control-allow-private-network", "true")
}

func writeHelperJSON(w http.ResponseWriter, status int, value any) {
	writeCORSHeaders(w)
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func sanitizeDiagnosticEvent(event string) string {
	var b strings.Builder
	for _, ch := range event {
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-' || ch == '.' {
			b.WriteRune(ch)
		} else {
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" {
		return "event"
	}
	return out
}
