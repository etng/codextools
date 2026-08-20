package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var diagnosticThrottle sync.Map

const (
	diagnosticLogMaxBytes     = 32 << 20
	diagnosticLogReadChunk    = 64 << 10
	diagnosticLogMaxTailBytes = 4 << 20
	diagnosticLogMaxReadLines = 5000
)

var diagnosticLogWriteMu sync.Mutex
var diagnosticLogStates = map[string]*diagnosticLogState{}

type diagnosticLogState struct {
	size        int64
	writes      uint64
	initialized bool
}

func appendDiagnosticLog(event string, detail map[string]any) {
	if detail == nil {
		detail = map[string]any{}
	}
	if shouldThrottleDiagnosticLog(event, detail, time.Now()) {
		return
	}
	redacted := redactForLog(detail)
	record := map[string]any{
		"timestamp_ms": time.Now().UnixMilli(),
		"pid":          os.Getpid(),
		"event":        event,
		"detail":       redacted,
	}
	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	path := diagnosticLogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}

	diagnosticLogWriteMu.Lock()
	defer diagnosticLogWriteMu.Unlock()
	state := diagnosticLogStates[path]
	if state == nil {
		state = &diagnosticLogState{}
		diagnosticLogStates[path] = state
	}
	if !state.initialized {
		if info, statErr := os.Stat(path); statErr == nil {
			state.size = info.Size()
		}
		state.initialized = true
	}
	if state.size+int64(len(data))+1 > diagnosticLogMaxBytes {
		state.size = rotateDiagnosticLog(path, state.size)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	n, _ := file.Write(append(data, '\n'))
	state.size += int64(n)
	state.writes++
	// The manager and launcher are separate processes and share this file.
	// Re-check occasionally so one process notices writes made by the other.
	if state.writes%64 == 0 {
		if info, statErr := file.Stat(); statErr == nil {
			state.size = info.Size()
		}
	}
}

func rotateDiagnosticLog(path string, currentSize int64) int64 {
	// Do not preserve legacy runaway logs: keeping a multi-gigabyte backup
	// would defeat rotation and continue consuming the user's disk.
	if currentSize > 2*diagnosticLogMaxBytes {
		if err := os.Truncate(path, 0); err == nil {
			return 0
		}
	}
	backup := path + ".1"
	_ = os.Remove(backup)
	if err := os.Rename(path, backup); err == nil || os.IsNotExist(err) {
		return 0
	}
	if err := os.Truncate(path, 0); err == nil {
		return 0
	}
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}

func shouldThrottleDiagnosticLog(event string, detail map[string]any, now time.Time) bool {
	if event != "renderer.service_tier_dispatcher_patch_failed" {
		return false
	}
	nestedDetail := mapArg(detail, "detail")
	if stringFromAny(nestedDetail["errorMessage"]) != "Codex dispatcher unavailable" {
		return false
	}
	const interval = time.Minute
	key := event + ":" + stringFromAny(nestedDetail["errorMessage"])
	lastValue, ok := diagnosticThrottle.Load(key)
	if ok {
		if last, ok := lastValue.(time.Time); ok && now.Sub(last) < interval {
			return true
		}
	}
	diagnosticThrottle.Store(key, now)
	return false
}

func redactForLog(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, item := range typed {
			lower := strings.ToLower(key)
			if isSensitiveDiagnosticKey(lower) {
				out[key] = "[redacted]"
			} else {
				out[key] = redactForLog(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = redactForLog(item)
		}
		return out
	case []string:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = redactForLog(item)
		}
		return out
	case string:
		trimmed := strings.TrimSpace(typed)
		lower := strings.ToLower(trimmed)
		if strings.Contains(lower, "bearer ") || strings.Contains(lower, "sk-") || strings.Contains(lower, `"access_token"`) || strings.Contains(lower, `"refresh_token"`) {
			return "[redacted]"
		}
		if parsed, err := url.Parse(trimmed); err == nil && parsed.Scheme != "" && parsed.Host != "" {
			parsed.User = nil
			parsed.RawQuery = ""
			parsed.Fragment = ""
			return parsed.String()
		}
		return typed
	default:
		return typed
	}
}

func isSensitiveDiagnosticKey(lower string) bool {
	for _, marker := range []string{"key", "token", "authorization", "secret", "password", "passphrase", "cookie", "credential", "authcontents"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return lower == "auth" || lower == "headers" || strings.HasSuffix(lower, "header")
}

func diagnosticSettingsValue(settings backendSettings) any {
	relayProfiles := make([]map[string]any, 0, len(settings.RelayProfiles))
	for index, profile := range settings.RelayProfiles {
		relayProfiles = append(relayProfiles, map[string]any{
			"index":                      index,
			"protocol":                   profile.Protocol,
			"relayMode":                  profile.RelayMode,
			"officialMixApiKey":          profile.OfficialMixAPIKey,
			"imageGenerationEnabled":     profile.ImageGenerationEnabled,
			"imageGenerationSeparateApi": profile.ImageGenerationUseSeparateAPI,
			"apiKeyConfigured":           strings.TrimSpace(profile.APIKey) != "",
			"officialAuthConfigured":     strings.TrimSpace(profile.OfficialAuthContents) != "" || strings.TrimSpace(profile.AuthContents) != "",
			"configConfigured":           strings.TrimSpace(profile.ConfigContents) != "",
			"proxyEnabled":               profile.ProxyEnabled,
			"useCommonConfig":            profile.UseCommonConfig,
		})
	}
	return map[string]any{
		"codexAppReference":               diagnosticAppReference(settings.CodexAppPath),
		"language":                        settings.Language,
		"launchMode":                      settings.LaunchMode,
		"providerSyncEnabled":             settings.ProviderSync,
		"providerSyncSavedProviderCount":  len(settings.ProviderSyncSavedProviders),
		"providerSyncManualProviderCount": len(settings.ProviderSyncManualProviders),
		"relayProfilesEnabled":            settings.RelayProfilesEnabled,
		"relayProfileCount":               len(settings.RelayProfiles),
		"relayProfiles":                   relayProfiles,
		"aggregateRelayProfileCount":      len(settings.AggregateRelayProfiles),
		"relayCommonConfigConfigured":     strings.TrimSpace(settings.RelayCommonConfigContents) != "",
		"relayContextConfigConfigured":    strings.TrimSpace(settings.RelayContextConfigContents) != "",
		"ccsLinkEnabled":                  settings.CCSLinkEnabled,
		"enhancementsEnabled":             settings.Enhancements,
		"codexAppPluginMarketplaceUnlock": settings.CodexAppPluginMarketplaceUnlock,
		"codexAppPluginAutoExpand":        settings.CodexAppPluginAutoExpand,
		"codexAppModelWhitelistUnlock":    settings.CodexAppModelWhitelistUnlock,
		"codexAppSessionDelete":           settings.CodexAppSessionDelete,
		"codexAppMarkdownExport":          settings.CodexAppMarkdownExport,
		"codexAppPasteFix":                settings.CodexAppPasteFix,
		"codexAppForceChineseLocale":      settings.CodexAppForceChineseLocale,
		"codexAppFastStartup":             settings.CodexAppFastStartup,
		"codexAppImageOverlayFitMode":     settings.CodexAppImageOverlayFitMode,
		"codexAppProjectMove":             settings.CodexAppProjectMove,
		"codexAppConversationTimeline":    settings.CodexAppConversationTimeline,
		"codexAppThreadIdBadge":           settings.CodexAppThreadIDBadge,
		"codexAppConversationView":        settings.CodexAppConversationView,
		"codexAppThreadScrollRestore":     settings.CodexAppThreadScrollRestore,
		"codexAppZedRemoteOpen":           settings.CodexAppZedRemoteOpen,
		"codexAppUpstreamWorktreeCreate":  settings.CodexAppUpstreamWorktreeCreate,
		"codexAppNativeMenuPlacement":     settings.CodexAppNativeMenuPlacement,
		"codexAppNativeMenuLocalization":  settings.CodexAppNativeMenuLocalization,
		"codexAppServiceTierControls":     settings.CodexAppServiceTierControls,
		"computerUseGuardEnabled":         settings.ComputerUseGuardEnabled,
		"zedRemoteOpenStrategy":           settings.ZedRemoteOpenStrategy,
		"zedRemoteProjectRegistryEnabled": settings.ZedRemoteProjectRegistryEnabled,
		"zedRemoteSyncToZedSettings":      settings.ZedRemoteSyncToZedSettings,
		"codexAppImageOverlayEnabled":     settings.CodexAppImageOverlayEnabled,
		"codexAppImageOverlayOpacity":     settings.CodexAppImageOverlayOpacity,
		"codexGoalsEnabled":               settings.CodexGoalsEnabled,
		"mobileControlEnabled":            settings.MobileControlEnabled,
		"onboardingCompleted":             settings.OnboardingCompleted,
		"onboardingCompletedPlatform":     settings.OnboardingCompletedPlatform,
		"cliWrapperEnabled":               settings.CLIWrapperEnabled,
		"cliWrapperBaseUrlConfigured":     strings.TrimSpace(settings.CLIWrapperBaseURL) != "",
		"cliWrapperApiKeyConfigured":      strings.TrimSpace(settings.CLIWrapperAPIKey) != "",
	}
}

func runtimeDiagnosticsValue() map[string]any {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	logBytes := int64(0)
	if info, err := os.Stat(diagnosticLogPath()); err == nil {
		logBytes = info.Size()
	}
	return map[string]any{
		"status":                "ok",
		"version":               version,
		"heapAllocBytes":        memory.HeapAlloc,
		"heapInuseBytes":        memory.HeapInuse,
		"heapSysBytes":          memory.HeapSys,
		"stackInuseBytes":       memory.StackInuse,
		"sysBytes":              memory.Sys,
		"nextGCBytes":           memory.NextGC,
		"numGC":                 memory.NumGC,
		"goroutines":            runtime.NumGoroutine(),
		"diagnosticLogBytes":    logBytes,
		"diagnosticLogMaxBytes": diagnosticLogMaxBytes,
	}
}

func diagnosticAppReference(path string) string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return ""
	}
	if reference := normalizeWindowsPackagedAppReference(trimmed); reference != "" {
		return reference
	}
	if appUserModelID := windowsAppUserModelIDFromPackagePath(trimmed); appUserModelID != "" {
		return "aumid:" + appUserModelID
	}
	base := filepath.Base(trimmed)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return "configured"
	}
	return base
}

func diagnosticLaunchStatusValue(status launchStatus) map[string]any {
	value := map[string]any{
		"status":       diagnosticLaunchStatusName(status.Status),
		"startedAtMs":  status.StartedAtMS,
		"debugPort":    status.DebugPort,
		"helperPort":   status.HelperPort,
		"appReference": "",
		"failureKind":  diagnosticLaunchFailureKind(status),
	}
	if status.CodexApp != nil {
		value["appReference"] = diagnosticAppReference(*status.CodexApp)
	}
	if detail := status.Detail; detail != nil {
		if appUserModelID := normalizeDiagnosticAUMID(stringFromAny(detail["appUserModelId"])); appUserModelID != "" {
			value["appUserModelId"] = appUserModelID
		}
		if method := diagnosticActivationMethod(stringFromAny(detail["activation_method"])); method != "" {
			value["activationMethod"] = method
		}
		if available, ok := detail["cdp_port_available"].(bool); ok {
			value["cdpPortAvailable"] = available
		}
		if stringFromAny(detail["executionAlias"]) != "" {
			value["executionAliasAvailable"] = true
		}
		if available, ok := detail["packagedExecutableAvailable"].(bool); ok {
			value["packagedExecutableAvailable"] = available
		}
		if attempts := diagnosticWindowsLaunchAttempts(detail["windows_launch_attempts"]); len(attempts) > 0 {
			value["windowsLaunchAttempts"] = attempts
		}
	}
	return value
}

func diagnosticWindowsLaunchAttempts(value any) []map[string]string {
	var raw []windowsPackagedLaunchAttempt
	switch typed := value.(type) {
	case []windowsPackagedLaunchAttempt:
		raw = typed
	case []any:
		for _, item := range typed {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			raw = append(raw, windowsPackagedLaunchAttempt{
				Method:  stringFromAny(entry["method"]),
				Outcome: stringFromAny(entry["outcome"]),
			})
		}
	}
	attempts := make([]map[string]string, 0, len(raw))
	for _, attempt := range raw {
		method := diagnosticWindowsLaunchMethod(attempt.Method)
		outcome := diagnosticWindowsLaunchOutcome(attempt.Outcome)
		if method != "" && outcome != "" {
			attempts = append(attempts, map[string]string{"method": method, "outcome": outcome})
		}
	}
	return attempts
}

func diagnosticWindowsLaunchMethod(method string) string {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "app_execution_alias", "msix_full_trust_executable", "application_activation_manager":
		return strings.ToLower(strings.TrimSpace(method))
	default:
		return ""
	}
}

func diagnosticWindowsLaunchOutcome(outcome string) string {
	switch strings.ToLower(strings.TrimSpace(outcome)) {
	case "success", "not_available", "activation_incorrect_function", "debug_port_unavailable", "process_start_failed", "activation_failed":
		return strings.ToLower(strings.TrimSpace(outcome))
	default:
		return ""
	}
}

func diagnosticLaunchStatusName(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "starting", "running", "degraded", "restarting", "exited", "failed":
		return strings.ToLower(strings.TrimSpace(status))
	default:
		return "unknown"
	}
}

func diagnosticActivationMethod(method string) string {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "executable", "packaged_activation", "app_execution_alias", "msix_full_trust_executable":
		return strings.ToLower(strings.TrimSpace(method))
	default:
		return ""
	}
}

func normalizeDiagnosticAUMID(value string) string {
	reference := normalizeWindowsPackagedAppReference("aumid:" + strings.TrimSpace(value))
	return strings.TrimPrefix(reference, "aumid:")
}

func diagnosticLaunchFailureKind(status launchStatus) string {
	detailError := ""
	if status.Detail != nil {
		detailError = stringFromAny(status.Detail["error"])
	}
	text := strings.ToLower(status.Message + " " + detailError)
	switch {
	case strings.Contains(text, "incorrect function"):
		return "application_activation_incorrect_function"
	case strings.Contains(text, "端口") && strings.Contains(text, "占用"):
		return "port_in_use"
	case strings.Contains(text, "调试端口") || strings.Contains(text, "remote-debugging-port") || strings.Contains(text, "cdp"):
		return "debug_port_unavailable"
	case strings.Contains(text, "未找到") && (strings.Contains(text, "chatgpt") || strings.Contains(text, "codex")):
		return "app_not_found"
	case strings.Contains(text, "已存在") || strings.Contains(text, "already running"):
		return "app_already_running"
	case diagnosticLaunchStatusName(status.Status) == "failed":
		return "launch_failed"
	default:
		return ""
	}
}

func diagnosticOverviewValue(overview commandResult) map[string]any {
	value := map[string]any{
		"status":         stringFromAny(overview["status"]),
		"currentVersion": stringFromAny(overview["current_version"]),
		"updateStatus":   stringFromAny(overview["update_status"]),
	}
	if codexApp := mapArg(map[string]any(overview), "codex_app"); len(codexApp) > 0 {
		appValue := map[string]any{
			"status":              stringFromAny(codexApp["status"]),
			"appKind":             stringFromAny(codexApp["appKind"]),
			"executableAvailable": stringFromAny(codexApp["executable"]) != "",
		}
		if appUserModelID := normalizeDiagnosticAUMID(stringFromAny(codexApp["appUserModelId"])); appUserModelID != "" {
			appValue["appUserModelId"] = appUserModelID
		}
		if path := stringFromAny(codexApp["path"]); path != "" {
			appValue["appReference"] = diagnosticAppReference(path)
		}
		value["codexApp"] = appValue
	}
	for _, key := range []string{"silent_shortcut", "management_shortcut"} {
		if shortcut := mapArg(map[string]any(overview), key); len(shortcut) > 0 {
			value[key+"Status"] = stringFromAny(shortcut["status"])
		}
	}
	return value
}

func (s *server) readLatestLogs(args map[string]any) commandResult {
	request := mapArg(args, "request")
	lines := intArg(request, "lines", 200)
	path := diagnosticLogPath()
	text, err := tailFile(path, lines)
	payload := map[string]any{"path": path, "text": text, "lines": lines}
	if err != nil {
		return failed("读取日志失败："+err.Error(), payload)
	}
	return ok("日志已读取。", payload)
}

func tailFile(path string, maxLines int) (string, error) {
	if maxLines <= 0 || maxLines > diagnosticLogMaxReadLines {
		maxLines = diagnosticLogMaxReadLines
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() == 0 {
		return "", nil
	}

	// Read backwards in bounded chunks. Diagnostics can be large, and the UI
	// only needs the most recent lines; loading the entire file caused avoidable
	// multi-hundred-megabyte allocations when the log had grown unchecked.
	var chunks [][]byte
	readBytes := 0
	newlines := 0
	offset := info.Size()
	for offset > 0 && readBytes < diagnosticLogMaxTailBytes && newlines <= maxLines {
		start := offset - diagnosticLogReadChunk
		if start < 0 {
			start = 0
		}
		if int64(readBytes)+(offset-start) > diagnosticLogMaxTailBytes {
			start = offset - int64(diagnosticLogMaxTailBytes-readBytes)
		}
		chunk := make([]byte, offset-start)
		if _, readErr := file.ReadAt(chunk, start); readErr != nil && readErr != io.EOF {
			return "", readErr
		}
		chunks = append(chunks, chunk)
		readBytes += len(chunk)
		newlines += bytes.Count(chunk, []byte{'\n'})
		offset = start
	}
	data := make([]byte, readBytes)
	cursor := 0
	for index := len(chunks) - 1; index >= 0; index-- {
		cursor += copy(data[cursor:], chunks[index])
	}
	if offset > 0 {
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			data = data[newline+1:]
		}
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n"), nil
}

func (s *server) diagnosticsReport() string {
	overview := s.loadOverview()
	settings := diagnosticSettingsValue(loadSettings())
	var latestLaunch launchStatus
	latestLaunchPath := latestStatusPath()
	latestLaunchLoaded := readJSON(latestLaunchPath, &latestLaunch) == nil
	report := map[string]any{
		"generatedAtMs": time.Now().UnixMilli(),
		"version":       version,
		"overview":      diagnosticOverviewValue(overview),
		"settings":      settings,
		"latestLaunch": map[string]any{
			"loaded": latestLaunchLoaded,
			"status": diagnosticLaunchStatusValue(latestLaunch),
		},
		"platform": map[string]any{"os": runtime.GOOS, "arch": runtime.GOARCH},
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "诊断报告序列化失败：" + err.Error()
	}
	return string(data)
}
