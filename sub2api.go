package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

const sub2APIResponseLimit = 1 << 20

type sub2APIBillingResponse struct {
	Object                  string   `json:"object"`
	SchemaVersion           uint8    `json:"schema_version"`
	BillingScope            string   `json:"billing_scope"`
	GroupRateMultiplier     float64  `json:"group_rate_multiplier"`
	UserRateMultiplier      *float64 `json:"user_rate_multiplier"`
	ResolvedRateMultiplier  float64  `json:"resolved_rate_multiplier"`
	PeakRateEnabled         bool     `json:"peak_rate_enabled"`
	PeakRateMultiplier      *float64 `json:"peak_rate_multiplier"`
	AppliedPeakMultiplier   *float64 `json:"applied_peak_multiplier"`
	EffectiveRateMultiplier float64  `json:"effective_rate_multiplier"`
	ObservedAt              string   `json:"observed_at"`
}

func sub2APIBillingEndpoint(baseURL string) string {
	cleaned := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if cleaned == "" {
		return ""
	}
	if strings.HasSuffix(strings.ToLower(cleaned), "/sub2api/billing") {
		return cleaned
	}
	if hasVersionSuffix(cleaned) {
		return cleaned + "/sub2api/billing"
	}
	return cleaned + "/v1/sub2api/billing"
}

func (s *server) fetchSub2APIBilling(ctx context.Context, args map[string]any) commandResult {
	var profile relayProfile
	if err := remarshal(args["profile"], &profile); err != nil {
		return sub2APIBillingFailure("供应商参数错误："+err.Error(), "")
	}
	baseURL := strings.TrimSpace(firstNonEmpty(profile.UpstreamBaseURL, profile.BaseURL))
	if baseURL == "" {
		return sub2APIBillingFailure("从「"+displayRelayName(profile)+"」获取 Sub2API 倍率失败：Base URL 不能为空", "")
	}
	endpoint := sub2APIBillingEndpoint(baseURL)
	apiKey := strings.TrimSpace(profile.APIKey)
	if apiKey == "" {
		return sub2APIBillingFailure("从「"+displayRelayName(profile)+"」获取 Sub2API 倍率失败：API Key 不能为空", endpoint)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return sub2APIBillingFailure("从「"+displayRelayName(profile)+"」获取 Sub2API 倍率失败："+err.Error(), endpoint)
	}
	request.Header.Set("accept", "application/json")
	request.Header.Set("authorization", "Bearer "+apiKey)
	request.Header.Set("x-api-key", apiKey)
	request.Header.Set("user-agent", firstNonEmpty(strings.TrimSpace(profile.UserAgent), "Codex"))
	client, err := relayHTTPClient(profile)
	if err != nil {
		return sub2APIBillingFailure("从「"+displayRelayName(profile)+"」获取 Sub2API 倍率失败："+err.Error(), endpoint)
	}
	billingClient := *client
	billingClient.Timeout = 15 * time.Second
	response, err := billingClient.Do(request)
	if err != nil {
		return sub2APIBillingFailure("从「"+displayRelayName(profile)+"」获取 Sub2API 倍率失败："+err.Error(), endpoint)
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, sub2APIResponseLimit+1))
	if readErr != nil {
		return sub2APIBillingFailure("从「"+displayRelayName(profile)+"」获取 Sub2API 倍率失败：读取响应失败", endpoint)
	}
	if len(body) > sub2APIResponseLimit {
		return sub2APIBillingFailure("从「"+displayRelayName(profile)+"」获取 Sub2API 倍率失败：响应超过 1 MiB 限制", endpoint)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := sub2APIErrorMessage(body)
		if message == "" {
			message = "上游拒绝读取倍率"
		}
		return sub2APIBillingFailure(fmt.Sprintf("从「%s」获取 Sub2API 倍率失败：HTTP %d：%s", displayRelayName(profile), response.StatusCode, message), endpoint)
	}
	var parsed sub2APIBillingResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return sub2APIBillingFailure("从「"+displayRelayName(profile)+"」获取 Sub2API 倍率失败：倍率接口没有返回有效 JSON", endpoint)
	}
	if err := validateSub2APIBillingResponse(parsed); err != nil {
		return sub2APIBillingFailure("从「"+displayRelayName(profile)+"」获取 Sub2API 倍率失败："+err.Error(), endpoint)
	}
	return ok(fmt.Sprintf("已从「%s」获取倍率：%sx。", displayRelayName(profile), formatSub2APIMultiplier(parsed.EffectiveRateMultiplier)), map[string]any{
		"endpoint":                endpoint,
		"groupRateMultiplier":     parsed.GroupRateMultiplier,
		"userRateMultiplier":      parsed.UserRateMultiplier,
		"resolvedRateMultiplier":  parsed.ResolvedRateMultiplier,
		"peakRateEnabled":         parsed.PeakRateEnabled,
		"peakRateMultiplier":      parsed.PeakRateMultiplier,
		"appliedPeakMultiplier":   parsed.AppliedPeakMultiplier,
		"effectiveRateMultiplier": parsed.EffectiveRateMultiplier,
		"observedAt":              parsed.ObservedAt,
	})
}

func sub2APIBillingFailure(message, endpoint string) commandResult {
	return failed(message, map[string]any{
		"endpoint":                endpoint,
		"groupRateMultiplier":     0,
		"userRateMultiplier":      nil,
		"resolvedRateMultiplier":  0,
		"peakRateEnabled":         false,
		"peakRateMultiplier":      nil,
		"appliedPeakMultiplier":   nil,
		"effectiveRateMultiplier": 0,
		"observedAt":              "",
	})
}

func validateSub2APIBillingResponse(response sub2APIBillingResponse) error {
	if response.Object != "sub2api.key_billing" || response.SchemaVersion != 1 || response.BillingScope != "token" {
		return errors.New("倍率接口返回结构不是 sub2api.key_billing")
	}
	for _, value := range []float64{response.GroupRateMultiplier, response.ResolvedRateMultiplier, response.EffectiveRateMultiplier} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return errors.New("倍率接口返回了无效倍率")
		}
	}
	if response.UserRateMultiplier != nil && (math.IsNaN(*response.UserRateMultiplier) || math.IsInf(*response.UserRateMultiplier, 0) || *response.UserRateMultiplier < 0) {
		return errors.New("倍率接口返回了无效用户倍率")
	}
	if strings.TrimSpace(response.ObservedAt) == "" {
		return errors.New("倍率接口缺少观测时间")
	}
	return nil
}

func sub2APIErrorMessage(body []byte) string {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	if nested, ok := payload["error"].(map[string]any); ok {
		if message := strings.TrimSpace(stringFromAny(nested["message"])); message != "" {
			return message
		}
	}
	for _, key := range []string{"message", "error"} {
		if message := strings.TrimSpace(stringFromAny(payload[key])); message != "" {
			return message
		}
	}
	return ""
}

func formatSub2APIMultiplier(value float64) string {
	text := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", value), "0"), ".")
	if text == "" {
		return "0"
	}
	return text
}
