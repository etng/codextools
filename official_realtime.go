package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	officialRealtimeAvailableReason       = "available"
	officialRealtimeNotBoundReason        = "official_realtime_not_bound"
	officialRealtimeExpiredReason         = "official_realtime_credentials_expired"
	officialRealtimeAccountMismatchReason = "official_realtime_account_mismatch"
	officialRealtimeNotEntitledReason     = "official_realtime_not_entitled"
	officialRealtimeUsageLimitedReason    = "official_realtime_usage_limited"
	officialRealtimeUpstreamFailedReason  = "official_realtime_upstream_failed"
	officialRealtimeLocalBaseURL          = "http://127.0.0.1:57323/v1"
)

var (
	officialRealtimeCallsEndpoint = "https://chatgpt.com/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas"
	officialRealtimeWebSocketURL  = "wss://api.openai.com/v1/realtime"
	officialRealtimeDenialStoreMu sync.Mutex
)

type officialRealtimeCredentials struct {
	AccessToken  string
	AccountID    string
	AccountLabel string
	Fingerprint  string
	Source       string
	ExpiresAt    time.Time
}

type officialRealtimeCapability struct {
	Available   bool
	Reason      string
	Message     string
	Credentials officialRealtimeCredentials
}

type officialRealtimeDenial struct {
	Fingerprint string `json:"fingerprint"`
	Reason      string `json:"reason"`
	Message     string `json:"message"`
	UpdatedAt   string `json:"updatedAt"`
}

func isRealtimeProxyPath(path string) bool {
	path = strings.SplitN(path, "?", 2)[0]
	for _, prefix := range []string{
		"/live", "/v1/live", "/v1/v1/live", "/codex/v1/live",
		"/realtime", "/v1/realtime", "/v1/v1/realtime", "/codex/v1/realtime",
	} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func isRealtimeCallCreatePath(path string) bool {
	path = "/" + strings.TrimLeft(strings.TrimSpace(path), "/")
	for _, candidate := range []string{
		"/live", "/v1/live", "/v1/v1/live", "/codex/v1/live",
		"/realtime/calls", "/v1/realtime/calls", "/v1/v1/realtime/calls", "/codex/v1/realtime/calls",
	} {
		if path == candidate {
			return true
		}
	}
	return false
}

func officialRealtimeCapabilityForSettings(settings backendSettings, home string, now time.Time) officialRealtimeCapability {
	profile := activeRelayProfile(normalizeSettings(settings))
	boundContents := []string{
		strings.TrimSpace(profile.OfficialAuthContents),
		strings.TrimSpace(profile.AuthContents),
	}
	var bound officialRealtimeCredentials
	var boundReason string
	hasBinding := false
	for _, contents := range boundContents {
		if contents == "" {
			continue
		}
		hasBinding = true
		candidate, reason := parseOfficialRealtimeCredentials(contents, "settings:"+profile.ID, now)
		if candidate.AccountID != "" && bound.AccountID == "" {
			bound = candidate
			boundReason = reason
		}
		if reason == officialRealtimeAvailableReason {
			bound = candidate
			boundReason = reason
			break
		}
	}
	if !hasBinding {
		return officialRealtimeCapability{
			Reason:  officialRealtimeNotBoundReason,
			Message: "当前供应商未绑定官方账号，请先在管理工具中绑定官方登录。",
		}
	}
	if bound.AccountID == "" {
		return officialRealtimeCapability{
			Reason:  officialRealtimeExpiredReason,
			Message: "当前供应商绑定的官方凭据不完整，请重新绑定官方登录。",
		}
	}

	currentPath := filepath.Join(home, "auth.json")
	if contents, err := os.ReadFile(currentPath); err == nil {
		current, currentReason := parseOfficialRealtimeCredentials(string(contents), currentPath, now)
		if current.AccountID != "" && current.AccountID == bound.AccountID && currentReason == officialRealtimeAvailableReason {
			return availableOfficialRealtimeCapability(current)
		}
		if current.AccountID != "" && current.AccountID != bound.AccountID && boundReason != officialRealtimeAvailableReason {
			return officialRealtimeCapability{
				Reason:  officialRealtimeAccountMismatchReason,
				Message: "当前 ChatGPT 登录账号与此供应商绑定账号不一致，请切换账号或重新绑定。",
			}
		}
	}
	if boundReason == officialRealtimeAvailableReason {
		return availableOfficialRealtimeCapability(bound)
	}
	return officialRealtimeCapability{
		Reason:  officialRealtimeExpiredReason,
		Message: "当前供应商绑定的官方凭据已过期，请重新登录并绑定官方账号。",
	}
}

func availableOfficialRealtimeCapability(credentials officialRealtimeCredentials) officialRealtimeCapability {
	return officialRealtimeCapability{
		Available:   true,
		Reason:      officialRealtimeAvailableReason,
		Message:     "官方语音可用，将使用当前供应商绑定的官方账号。",
		Credentials: credentials,
	}
}

func parseOfficialRealtimeCredentials(contents, source string, now time.Time) (officialRealtimeCredentials, string) {
	var root map[string]any
	if json.Unmarshal([]byte(contents), &root) != nil {
		return officialRealtimeCredentials{}, officialRealtimeExpiredReason
	}
	authMode := strings.ToLower(strings.TrimSpace(stringFromAny(root["auth_mode"])))
	if authMode != "" && authMode != "chatgpt" && authMode != "openai" {
		return officialRealtimeCredentials{}, officialRealtimeExpiredReason
	}
	tokens, _ := root["tokens"].(map[string]any)
	if tokens == nil {
		return officialRealtimeCredentials{}, officialRealtimeExpiredReason
	}
	accessToken := strings.TrimSpace(stringFromAny(tokens["access_token"]))
	accountID := strings.TrimSpace(stringFromAny(tokens["account_id"]))
	idClaims := jwtClaims(stringFromAny(tokens["id_token"]))
	if accountID == "" {
		if authClaims, ok := idClaims["https://api.openai.com/auth"].(map[string]any); ok {
			accountID = firstString(
				stringFromAny(authClaims["chatgpt_account_id"]),
				stringFromAny(authClaims["account_id"]),
			)
		}
	}
	credentials := officialRealtimeCredentials{
		AccessToken:  accessToken,
		AccountID:    accountID,
		AccountLabel: accountLabelFromTokens(tokens),
		Source:       source,
	}
	if accessClaims := jwtClaims(accessToken); accessClaims != nil {
		if exp := int64FromFlexible(accessClaims["exp"]); exp > 0 {
			credentials.ExpiresAt = time.Unix(exp, 0)
		}
	}
	if accessToken == "" || accountID == "" {
		return credentials, officialRealtimeExpiredReason
	}
	if !credentials.ExpiresAt.IsZero() && !now.Add(30*time.Second).Before(credentials.ExpiresAt) {
		return credentials, officialRealtimeExpiredReason
	}
	hash := sha256.Sum256([]byte(accountID + "\x00" + accessToken))
	credentials.Fingerprint = hex.EncodeToString(hash[:])
	return credentials, officialRealtimeAvailableReason
}

func jwtClaims(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return nil
	}
	return claims
}

func (r *launcherRuntime) officialRealtimeCapability(settings backendSettings) officialRealtimeCapability {
	capability := applyStoredOfficialRealtimeDenial(officialRealtimeCapabilityForSettings(settings, codexHomeDir(), time.Now()))
	r.realtimeMu.Lock()
	defer r.realtimeMu.Unlock()
	if !capability.Available {
		return capability
	}
	if r.realtimeDenied.Fingerprint == "" {
		return capability
	}
	if r.realtimeDenied.Fingerprint != capability.Credentials.Fingerprint {
		r.realtimeDenied = officialRealtimeDenial{}
		return capability
	}
	capability.Available = false
	capability.Reason = r.realtimeDenied.Reason
	capability.Message = r.realtimeDenied.Message
	capability.Credentials = officialRealtimeCredentials{}
	return capability
}

func (r *launcherRuntime) markOfficialRealtimeDenied(credentials officialRealtimeCredentials, status int, responseBody []byte) {
	denial := officialRealtimeDenial{Fingerprint: credentials.Fingerprint, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	switch {
	case status == http.StatusUnauthorized:
		denial.Reason = officialRealtimeExpiredReason
		denial.Message = "官方语音凭据已失效，请重新登录并绑定官方账号。"
	case status == http.StatusTooManyRequests || officialRealtimeUsageLimitResponse(responseBody):
		denial.Reason = officialRealtimeUsageLimitedReason
		denial.Message = localizedOfficialRealtimeUsageMessage(responseBody)
	case status == http.StatusForbidden:
		denial.Reason = officialRealtimeNotEntitledReason
		denial.Message = "该官方账号当前未开放语音聊天，请检查账号权限或重新绑定。"
	default:
		return
	}
	r.realtimeMu.Lock()
	r.realtimeDenied = denial
	r.realtimeMu.Unlock()
	storeOfficialRealtimeDenial(denial)
}

func officialRealtimeDenialPath() string {
	return filepath.Join(stateDir(), "official-realtime-denial.json")
}

func applyStoredOfficialRealtimeDenial(capability officialRealtimeCapability) officialRealtimeCapability {
	if !capability.Available || capability.Credentials.Fingerprint == "" {
		return capability
	}
	officialRealtimeDenialStoreMu.Lock()
	defer officialRealtimeDenialStoreMu.Unlock()
	denial := loadOfficialRealtimeDenialUnlocked()
	if denial.Fingerprint == "" {
		return capability
	}
	if denial.Fingerprint != capability.Credentials.Fingerprint {
		_ = os.Remove(officialRealtimeDenialPath())
		return capability
	}
	capability.Available = false
	capability.Reason = denial.Reason
	capability.Message = denial.Message
	capability.Credentials = officialRealtimeCredentials{}
	return capability
}

func loadOfficialRealtimeDenialUnlocked() officialRealtimeDenial {
	data, err := os.ReadFile(officialRealtimeDenialPath())
	if err != nil {
		return officialRealtimeDenial{}
	}
	var denial officialRealtimeDenial
	if json.Unmarshal(data, &denial) != nil || len(denial.Fingerprint) != sha256.Size*2 {
		return officialRealtimeDenial{}
	}
	switch denial.Reason {
	case officialRealtimeExpiredReason, officialRealtimeNotEntitledReason, officialRealtimeUsageLimitedReason:
		return denial
	default:
		return officialRealtimeDenial{}
	}
}

func storeOfficialRealtimeDenial(denial officialRealtimeDenial) {
	if denial.Fingerprint == "" {
		return
	}
	officialRealtimeDenialStoreMu.Lock()
	defer officialRealtimeDenialStoreMu.Unlock()
	path := officialRealtimeDenialPath()
	if atomicWriteJSON(path, denial) == nil {
		_ = os.Chmod(path, 0o600)
	}
}

func officialRealtimeCapabilityValue(capability officialRealtimeCapability) map[string]any {
	return map[string]any{
		"status":    "ok",
		"available": capability.Available,
		"reason":    capability.Reason,
		"message":   capability.Message,
	}
}

func (r *launcherRuntime) officialRealtimeStatusValue() map[string]any {
	return officialRealtimeCapabilityValue(r.officialRealtimeCapability(r.relaySettingsForRequest()))
}

func (r *launcherRuntime) forwardOfficialRealtimeHTTP(w http.ResponseWriter, req *http.Request, body []byte) {
	if req.Method != http.MethodPost || !isRealtimeCallCreatePath(req.URL.Path) {
		writeOfficialRealtimeFailure(w, http.StatusMethodNotAllowed, officialRealtimeUpstreamFailedReason, "官方语音信令只支持 POST call creation。")
		return
	}
	settings := r.relaySettingsForRequest()
	capability := r.officialRealtimeCapability(settings)
	if !capability.Available {
		writeOfficialRealtimeFailure(w, http.StatusForbidden, capability.Reason, capability.Message)
		return
	}
	payload, err := officialRealtimeCallPayload(req.Header.Get("content-type"), body)
	if err != nil {
		writeOfficialRealtimeFailure(w, http.StatusBadRequest, officialRealtimeUpstreamFailedReason, err.Error())
		return
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		writeOfficialRealtimeFailure(w, http.StatusBadRequest, officialRealtimeUpstreamFailedReason, "语音信令请求编码失败。")
		return
	}
	upstreamReq, err := http.NewRequestWithContext(req.Context(), http.MethodPost, officialRealtimeCallsEndpoint, bytes.NewReader(encoded))
	if err != nil {
		writeOfficialRealtimeFailure(w, http.StatusBadGateway, officialRealtimeUpstreamFailedReason, "创建官方语音请求失败。")
		return
	}
	copyOfficialRealtimeHeaders(req.Header, upstreamReq.Header)
	upstreamReq.Header.Set("authorization", "Bearer "+capability.Credentials.AccessToken)
	upstreamReq.Header.Set("chatgpt-account-id", capability.Credentials.AccountID)
	upstreamReq.Header.Set("content-type", "application/json")
	upstreamReq.Header.Set("accept", "application/sdp")
	profile := activeRelayProfile(settings)
	client, err := officialRealtimeHTTPClientForSettings(settings, profile)
	if err != nil {
		writeOfficialRealtimeFailure(w, http.StatusBadGateway, officialRealtimeUpstreamFailedReason, err.Error())
		return
	}
	startedAt := time.Now()
	resp, err := client.Do(upstreamReq)
	if err != nil {
		appendDiagnosticLog("official_realtime.http_failed", map[string]any{
			"duration_ms": time.Since(startedAt).Milliseconds(),
			"error":       err.Error(),
		})
		writeOfficialRealtimeFailure(w, http.StatusBadGateway, officialRealtimeUpstreamFailedReason, "连接 OpenAI 官方语音服务失败。")
		return
	}
	defer resp.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024+1))
	if readErr != nil || len(responseBody) > 8*1024*1024 {
		writeOfficialRealtimeFailure(w, http.StatusBadGateway, officialRealtimeUpstreamFailedReason, "读取官方语音响应失败。")
		return
	}
	usageLimited := resp.StatusCode == http.StatusTooManyRequests || officialRealtimeUsageLimitResponse(responseBody)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || usageLimited {
		r.markOfficialRealtimeDenied(capability.Credentials, resp.StatusCode, responseBody)
	}
	if usageLimited {
		responseBody = localizedOfficialRealtimeUsageBody(responseBody)
		resp.Header.Set("content-type", "application/json; charset=utf-8")
	}
	writeCORSHeaders(w)
	copyOfficialRealtimeResponseHeaders(resp.Header, w.Header())
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
	appendDiagnosticLog("official_realtime.http_completed", map[string]any{
		"status":      resp.StatusCode,
		"duration_ms": time.Since(startedAt).Milliseconds(),
		"body_bytes":  len(responseBody),
	})
}

func officialRealtimeUsageLimitResponse(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	lower := strings.ToLower(string(body))
	for _, marker := range []string{
		"usage limit",
		"usage_limit",
		"insufficient_quota",
		"purchase more credits",
		"upgrade to pro",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func localizedOfficialRealtimeUsageBody(upstreamBody []byte) []byte {
	message := localizedOfficialRealtimeUsageMessage(upstreamBody)
	body, err := json.Marshal(map[string]any{
		"message": message,
		"error": map[string]any{
			"message": message,
			"type":    officialRealtimeUsageLimitedReason,
			"code":    officialRealtimeUsageLimitedReason,
		},
	})
	if err != nil {
		return []byte(`{"error":{"message":"语音无法开启：当前官方账号的语音额度已耗尽。"}}`)
	}
	return body
}

func localizedOfficialRealtimeUsageMessage(upstreamBody []byte) string {
	message := "语音无法开启：当前官方账号的语音额度已耗尽。"
	if retryAt := officialRealtimeRetryAt(upstreamBody); retryAt != "" {
		message += " 官方预计恢复时间：" + retryAt + "。"
	} else {
		message += " 请在官方额度恢复后重试。"
	}
	return message + " 可前往 ChatGPT 用量页面查看或购买额度：https://chatgpt.com/codex/settings/usage"
}

func officialRealtimeRetryAt(body []byte) string {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	message := stringFromAny(payload["message"])
	if errorValue, ok := payload["error"].(map[string]any); ok {
		message = firstString(stringFromAny(errorValue["message"]), message)
	}
	const marker = "try again at "
	index := strings.Index(strings.ToLower(message), marker)
	if index < 0 {
		return ""
	}
	retryAt := strings.TrimSpace(message[index+len(marker):])
	return strings.TrimSpace(strings.TrimRight(retryAt, ".。"))
}

func officialRealtimeCallPayload(contentType string, body []byte) (map[string]any, error) {
	mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		return nil, errors.New("语音信令请求缺少有效的 Content-Type。")
	}
	var sdp string
	var session any = map[string]any{}
	switch strings.ToLower(mediaType) {
	case "multipart/form-data":
		boundary := strings.TrimSpace(params["boundary"])
		if boundary == "" {
			return nil, errors.New("语音信令 multipart boundary 缺失。")
		}
		reader := multipart.NewReader(bytes.NewReader(body), boundary)
		for {
			part, nextErr := reader.NextPart()
			if errors.Is(nextErr, io.EOF) {
				break
			}
			if nextErr != nil {
				return nil, errors.New("语音信令 multipart 内容无效。")
			}
			value, readErr := io.ReadAll(io.LimitReader(part, 8*1024*1024+1))
			_ = part.Close()
			if readErr != nil || len(value) > 8*1024*1024 {
				return nil, errors.New("语音信令 multipart 字段过大。")
			}
			switch part.FormName() {
			case "sdp":
				sdp = string(value)
			case "session":
				if len(bytes.TrimSpace(value)) > 0 && json.Unmarshal(value, &session) != nil {
					return nil, errors.New("语音信令 session JSON 无效。")
				}
			}
		}
	case "application/json":
		var value map[string]any
		if json.Unmarshal(body, &value) != nil {
			return nil, errors.New("语音信令 JSON 无效。")
		}
		sdp = stringFromAny(value["sdp"])
		if value["session"] != nil {
			session = value["session"]
		}
	case "application/sdp":
		sdp = string(body)
	default:
		return nil, fmt.Errorf("不支持的语音信令 Content-Type：%s", mediaType)
	}
	if strings.TrimSpace(sdp) == "" {
		return nil, errors.New("语音信令请求缺少 SDP。")
	}
	if _, ok := session.(map[string]any); !ok {
		return nil, errors.New("语音信令 session 必须是 JSON object。")
	}
	return map[string]any{"sdp": sdp, "session": session}, nil
}

func officialRealtimeHTTPClient(profile relayProfile) (*http.Client, error) {
	client, err := relayHTTPClient(profile)
	if err != nil {
		return nil, err
	}
	copy := *client
	copy.Timeout = 30 * time.Second
	return &copy, nil
}

func officialRealtimeHTTPClientForSettings(settings backendSettings, profile relayProfile) (*http.Client, error) {
	client, err := relayHTTPClientForSettings(settings, profile, proxyPurposeRealtime)
	if err != nil {
		return nil, err
	}
	copy := *client
	copy.Timeout = 30 * time.Second
	return &copy, nil
}

func copyOfficialRealtimeHeaders(source, target http.Header) {
	for _, name := range []string{
		"openai-alpha", "session-id", "thread-id", "x-session-id", "originator", "x-oai-attestation", "user-agent",
	} {
		for _, value := range source.Values(name) {
			target.Add(name, value)
		}
	}
}

func copyOfficialRealtimeResponseHeaders(source, target http.Header) {
	for _, name := range []string{"content-type", "location", "openai-request-id", "x-request-id", "cache-control"} {
		if value := source.Get(name); value != "" {
			target.Set(name, value)
		}
	}
}

func writeOfficialRealtimeFailure(w http.ResponseWriter, status int, code, message string) {
	writeHelperJSON(w, status, map[string]any{
		"status":  "failed",
		"code":    code,
		"message": message,
		"error": map[string]any{
			"type":    code,
			"code":    code,
			"message": message,
		},
	})
}

func (r *launcherRuntime) forwardOfficialRealtimeWebSocket(w http.ResponseWriter, req *http.Request) {
	settings := r.relaySettingsForRequest()
	capability := r.officialRealtimeCapability(settings)
	if !capability.Available {
		writeOfficialRealtimeFailure(w, http.StatusForbidden, capability.Reason, capability.Message)
		return
	}
	target, err := officialRealtimeWebSocketTarget(req.URL)
	if err != nil {
		writeOfficialRealtimeFailure(w, http.StatusBadGateway, officialRealtimeUpstreamFailedReason, err.Error())
		return
	}
	profile := activeRelayProfile(settings)
	proxyURL, err := effectiveProxyURL(settings, profile, proxyPurposeRealtime)
	if err != nil {
		writeOfficialRealtimeFailure(w, http.StatusBadGateway, officialRealtimeUpstreamFailedReason, err.Error())
		return
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		Proxy:            http.ProxyFromEnvironment,
		Subprotocols:     websocket.Subprotocols(req),
	}
	if proxyURL != nil {
		dialer.Proxy = http.ProxyURL(proxyURL)
	}
	headers := make(http.Header)
	copyOfficialRealtimeHeaders(req.Header, headers)
	headers.Del("sec-websocket-protocol")
	headers.Set("authorization", "Bearer "+capability.Credentials.AccessToken)
	headers.Set("chatgpt-account-id", capability.Credentials.AccountID)
	setRelayProxyUserAgent(profile.UserAgent, req.Header, headers)
	startedAt := time.Now()
	upstream, response, err := dialer.DialContext(req.Context(), target, headers)
	if err != nil {
		status := 0
		var responseBody []byte
		if response != nil {
			status = response.StatusCode
			responseBody, _ = io.ReadAll(io.LimitReader(response.Body, 4*1024*1024))
			_ = response.Body.Close()
			usageLimited := status == http.StatusTooManyRequests || officialRealtimeUsageLimitResponse(responseBody)
			if status == http.StatusUnauthorized || status == http.StatusForbidden || usageLimited {
				r.markOfficialRealtimeDenied(capability.Credentials, status, responseBody)
			}
			if usageLimited {
				responseBody = localizedOfficialRealtimeUsageBody(responseBody)
				response.Header.Set("content-type", "application/json; charset=utf-8")
			}
			copyOfficialRealtimeResponseHeaders(response.Header, w.Header())
			w.WriteHeader(status)
			_, _ = w.Write(responseBody)
		} else {
			writeOfficialRealtimeFailure(w, http.StatusBadGateway, officialRealtimeUpstreamFailedReason, "连接 OpenAI 官方语音 WebSocket 失败。")
		}
		appendDiagnosticLog("official_realtime.websocket_handshake_failed", map[string]any{
			"status":      status,
			"duration_ms": time.Since(startedAt).Milliseconds(),
			"error":       err.Error(),
		})
		return
	}
	defer upstream.Close()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	if subprotocol := upstream.Subprotocol(); subprotocol != "" {
		upgrader.Subprotocols = []string{subprotocol}
	}
	client, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		appendDiagnosticLog("official_realtime.websocket_client_upgrade_failed", map[string]any{"error": err.Error()})
		return
	}
	defer client.Close()
	clientToUpstream, upstreamToClient := bridgeRelayWebSockets(client, upstream)
	appendDiagnosticLog("official_realtime.websocket_closed", map[string]any{
		"client_to_upstream_messages": clientToUpstream,
		"upstream_to_client_messages": upstreamToClient,
		"duration_ms":                 time.Since(startedAt).Milliseconds(),
	})
}

func officialRealtimeWebSocketTarget(requestURL *url.URL) (string, error) {
	target, err := url.Parse(officialRealtimeWebSocketURL)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return "", errors.New("OpenAI 官方语音 WebSocket 地址无效。")
	}
	if target.Scheme != "ws" && target.Scheme != "wss" {
		return "", errors.New("OpenAI 官方语音 WebSocket scheme 无效。")
	}
	target.RawQuery = requestURL.RawQuery
	return target.String(), nil
}

func officialRealtimeContextWithTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 30*time.Second)
}
