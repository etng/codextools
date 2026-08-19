package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const (
	llmProxyRequestLimit  = 1 << 20
	llmProxyResponseLimit = 4 << 20
)

var llmProxyAllowedHeaders = map[string]bool{
	"accept":              true,
	"api-key":             true,
	"anthropic-beta":      true,
	"anthropic-version":   true,
	"authorization":       true,
	"content-type":        true,
	"openai-organization": true,
	"openai-project":      true,
	"x-api-key":           true,
}

var newLLMProxyHTTPClient = func(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func llmProxyValue(payload map[string]any) map[string]any {
	target, err := validateLLMProxyURL(stringFromAny(payload["url"]))
	if err != nil {
		return llmProxyFailure("llm_proxy_invalid_url", err)
	}
	method := strings.TrimSpace(stringFromAny(payload["method"]))
	if method == "" {
		method = http.MethodPost
	}
	if !strings.EqualFold(method, http.MethodPost) {
		return llmProxyFailure("llm_proxy_method_not_allowed", errors.New("LLM Bridge 仅支持 POST 请求"))
	}
	body := stringFromAny(payload["body"])
	if len(body) > llmProxyRequestLimit {
		return llmProxyFailure("llm_proxy_request_too_large", errors.New("LLM Bridge 请求体过大"))
	}
	timeoutMS := int64FromFlexible(payload["timeout_ms"])
	if timeoutMS == 0 {
		timeoutMS = 60000
	}
	if timeoutMS < 1000 {
		timeoutMS = 1000
	}
	if timeoutMS > 60000 {
		timeoutMS = 60000
	}
	request, err := http.NewRequest(http.MethodPost, target.String(), bytes.NewBufferString(body))
	if err != nil {
		return llmProxyFailure("llm_proxy_request_failed", err)
	}
	if rawHeaders, ok := payload["headers"].(map[string]any); ok {
		for name, rawValue := range rawHeaders {
			if llmProxyAllowedHeaders[strings.ToLower(strings.TrimSpace(name))] {
				if value, ok := rawValue.(string); ok {
					request.Header.Set(name, value)
				}
			}
		}
	}
	client := newLLMProxyHTTPClient(time.Duration(timeoutMS) * time.Millisecond)
	response, err := client.Do(request)
	if err != nil {
		return llmProxyFailure("llm_proxy_upstream_failed", err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, llmProxyResponseLimit+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil {
		return llmProxyFailure("llm_proxy_upstream_failed", err)
	}
	if len(responseBody) > llmProxyResponseLimit {
		return llmProxyFailure("llm_proxy_response_too_large", errors.New("LLM Bridge 响应体过大"))
	}
	result := map[string]any{
		"status":      "ok",
		"http_status": response.StatusCode,
		"ok":          response.StatusCode >= 200 && response.StatusCode < 300,
		"body_text":   string(responseBody),
	}
	if strings.EqualFold(strings.TrimSpace(strings.Split(response.Header.Get("content-type"), ";")[0]), "application/json") {
		var value any
		if json.Unmarshal(responseBody, &value) == nil {
			result["body_json"] = value
		}
	}
	return result
}

func llmProxyFailure(code string, err error) map[string]any {
	return map[string]any{"status": "failed", "code": code, "message": err.Error()}
}

func validateLLMProxyURL(raw string) (*url.URL, error) {
	target, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || target.Scheme == "" || target.Host == "" {
		return nil, errors.New("Base URL 格式无效")
	}
	if !strings.EqualFold(target.Scheme, "https") {
		return nil, errors.New("Base URL 必须使用 HTTPS")
	}
	if target.User != nil {
		return nil, errors.New("Base URL 不得包含用户名或密码")
	}
	if blockedLLMProxyHost(target.Hostname()) {
		return nil, errors.New("Base URL 不得指向本机或私有网络")
	}
	return target, nil
}

func blockedLLMProxyHost(host string) bool {
	host = strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return true
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	address = address.Unmap()
	if address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast() || address.IsMulticast() || address.IsUnspecified() {
		return true
	}
	blockedPrefixes := []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32",
	}
	for _, rawPrefix := range blockedPrefixes {
		if prefix, parseErr := netip.ParsePrefix(rawPrefix); parseErr == nil && prefix.Contains(address) {
			return true
		}
	}
	return false
}
