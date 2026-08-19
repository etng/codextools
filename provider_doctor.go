package main

import (
	"context"
	"fmt"
	"strings"
)

type providerDoctorCheck struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func (s *server) diagnoseRelayProfile(ctx context.Context, args map[string]any) commandResult {
	var profile relayProfile
	if err := remarshal(args["profile"], &profile); err != nil {
		return providerDoctorResult("failed", "Provider Doctor：供应商参数错误。", "", "", "供应商参数错误。", "请重新打开供应商详情后再试。", nil)
	}
	profileName := displayRelayName(profile)
	testModel := strings.TrimSpace(firstNonEmpty(profile.TestModel, profile.Model, loadSettings().RelayTestModel, defaultRelayTestModel))
	checks := []providerDoctorCheck{}
	if profile.RelayMode == "official" && !profile.OfficialMixAPIKey {
		checks = append(checks, providerDoctorCheck{ID: "config", Title: "配置完整性", Status: "ok", Detail: "官方登录供应商不需要 Base URL / API Key。"})
		return providerDoctorResult("ok", "Provider Doctor：官方登录供应商无需 API 诊断。", profileName, testModel, "官方登录供应商无需 API 诊断。", "如果 ChatGPT 官方账号可用，直接使用官方登录模式即可。", checks)
	}
	baseURL := strings.TrimSpace(firstNonEmpty(profile.UpstreamBaseURL, profile.BaseURL))
	if baseURL == "" || strings.TrimSpace(profile.APIKey) == "" {
		checks = append(checks, providerDoctorCheck{ID: "config", Title: "配置完整性", Status: "failed", Detail: "Base URL 或 API Key 为空。"})
		return providerDoctorResult("failed", "Provider Doctor：配置不完整。", profileName, testModel, "配置不完整，无法发起上游诊断。", "先填写 Base URL 和 API Key；如果是官方账号，请切换到官方登录模式。", checks)
	}
	checks = append(checks, providerDoctorCheck{
		ID: "config", Title: "配置完整性", Status: "ok",
		Detail: safeStatusURL(baseURL) + " / " + map[bool]string{true: "Chat Completions", false: "Responses API"}[profile.Protocol == "chatCompletions"],
	})

	modelsResult := s.fetchRelayProfileModels(ctx, map[string]any{"profile": profile})
	if modelsResult["status"] == "ok" {
		var models []string
		_ = remarshal(modelsResult["models"], &models)
		containsModel := false
		for _, model := range models {
			if model == testModel {
				containsModel = true
				break
			}
		}
		modelStatus := "ok"
		detail := fmt.Sprintf("%s 返回 %d 个模型。", stringFromAny(modelsResult["endpoint"]), len(models))
		if len(models) == 0 {
			modelStatus = "failed"
		} else if testModel != "" && !containsModel {
			modelStatus = "warning"
			detail = fmt.Sprintf("%s 返回 %d 个模型，但未看到测试模型「%s」。", stringFromAny(modelsResult["endpoint"]), len(models), testModel)
		}
		checks = append(checks, providerDoctorCheck{ID: "models", Title: "模型列表", Status: modelStatus, Detail: detail})
	} else {
		checks = append(checks, providerDoctorCheck{ID: "models", Title: "模型列表", Status: "failed", Detail: strings.TrimSpace(stringFromAny(modelsResult["message"]))})
	}

	requestProfile := profile
	requestProfile.TestModel = testModel
	requestResult := s.testRelayProfile(ctx, map[string]any{"profile": requestProfile})
	requestStatus := "failed"
	if requestResult["status"] == "ok" {
		requestStatus = "ok"
	}
	requestDetail := strings.TrimSpace(stringFromAny(requestResult["message"]))
	if requestDetail == "" {
		requestDetail = "真实请求没有返回诊断信息。"
	}
	checks = append(checks, providerDoctorCheck{ID: "request", Title: "真实请求", Status: requestStatus, Detail: requestDetail})

	failedCount, warningCount := 0, 0
	for _, check := range checks {
		switch check.Status {
		case "failed":
			failedCount++
		case "warning":
			warningCount++
		}
	}
	status := "ok"
	summary := "供应商基础诊断通过。"
	if failedCount > 0 {
		status = "failed"
		summary = fmt.Sprintf("发现 %d 项失败，ChatGPT Codex 可能无法使用该供应商。", failedCount)
	} else if warningCount > 0 {
		summary = fmt.Sprintf("基础连接可用，但有 %d 项需要确认。", warningCount)
	}
	recommendation := providerDoctorRecommendation(checks)
	return providerDoctorResult(status, "Provider Doctor："+summary, profileName, testModel, summary, recommendation, checks)
}

func providerDoctorResult(status, message, profileName, model, summary, recommendation string, checks []providerDoctorCheck) commandResult {
	if checks == nil {
		checks = []providerDoctorCheck{}
	}
	return commandResult{
		"status": status, "message": message, "profileName": profileName, "model": model,
		"summary": summary, "recommendation": recommendation, "checks": checks,
	}
}

func providerDoctorRecommendation(checks []providerDoctorCheck) string {
	for _, check := range checks {
		if check.ID == "config" && check.Status == "failed" {
			return "先补齐 Base URL 和 API Key；如果使用官方账号，请切换到官方登录模式。"
		}
	}
	for _, check := range checks {
		if check.ID == "models" && check.Status == "failed" {
			return "优先检查 Base URL 是否包含正确的 /v1 前缀，以及供应商是否支持 /v1/models。"
		}
	}
	for _, check := range checks {
		if check.ID == "request" && check.Status == "failed" {
			return "优先检查测试模型名称、上游协议选择和 Key 权限；如果 Chat Completions 可用，请切到对应协议。"
		}
	}
	for _, check := range checks {
		if check.Status == "warning" {
			return "连接可用，但测试模型没有出现在模型列表里；建议改用上游返回的模型名。"
		}
	}
	return "可以作为 ChatGPT Codex 供应商使用；如果真实对话仍失败，请查看协议代理日志里的上游响应。"
}
