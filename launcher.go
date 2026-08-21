package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func shouldRunLauncher(args []string) bool {
	if binaryRole == "launcher" {
		return true
	}
	if len(args) > 0 {
		base := strings.ToLower(filepath.Base(args[0]))
		if strings.Contains(base, "launcher") {
			return true
		}
	}
	for _, arg := range args[1:] {
		if arg == "--launcher" {
			return true
		}
	}
	return false
}

func runLauncher(args []string) error {
	settings := loadSettings()
	options := parseLaunchRequest(args)
	debugPort := options.debugPort
	if debugPort == 0 {
		debugPort = 9229
	}
	helperPort := options.helperPort
	if helperPort == 0 {
		helperPort = 57321
	}
	appPath := resolveLaunchableCodexApp(options.appPath)
	if appPath == "" {
		appPath = resolveLaunchableCodexApp(settings.CodexAppPath)
	}
	if appPath == "" {
		message := "未找到可直接启动的 ChatGPT 桌面应用；请安装 ChatGPT 桌面应用，或在管理工具中选择 ChatGPT.app / ChatGPT.exe"
		if runtime.GOOS == "windows" {
			message = "未找到 Codex 与 ChatGPT 合并后的 Windows 新版应用；请从 Microsoft Store 更新/安装 ChatGPT，或选择 WindowsApps 外的 ChatGPT.exe / Codex.exe"
		}
		err := errors.New(message)
		writeLaunchFailureStatus("启动 ChatGPT Codex 失败："+err.Error(), debugPort, helperPort, nil)
		appendDiagnosticLog("launcher.codex_app_missing", map[string]any{"debug_port": debugPort, "helper_port": helperPort, "error": err.Error()})
		return err
	}
	var restartLock fileLockGuard
	releaseRestartLock := func() {
		if restartLock != nil {
			_ = restartLock.release()
			restartLock = nil
		}
	}
	if options.restart && currentRuntimeGOOS() == "windows" {
		lock, acquired, lockErr := acquireLauncherRestartLock()
		if lockErr != nil {
			return fmt.Errorf("获取 Windows 重启互斥锁失败：%w", lockErr)
		}
		if !acquired {
			appendDiagnosticLog("launcher.restart_coalesced", map[string]any{"debug_port": debugPort, "helper_port": helperPort})
			return nil
		}
		restartLock = lock
		defer releaseRestartLock()
	}
	restartPrepared := false
	instanceLock, acquired, err := acquireLauncherSingleInstanceLock(debugPort)
	if err != nil {
		writeLaunchFailureStatus("启动 ChatGPT Codex 单实例锁失败："+err.Error(), debugPort, helperPort, &appPath)
		appendDiagnosticLog("launcher.guard_failed", map[string]any{"debug_port": debugPort, "helper_port": helperPort, "guard_port": launcherGuardPort, "error": err.Error()})
		return err
	}
	if !acquired {
		if options.restart {
			appendDiagnosticLog("launcher.restart_existing_launcher", map[string]any{"debug_port": debugPort, "helper_port": helperPort, "guard_port": launcherGuardPort})
			if err := prepareFullRestartBeforeLaunch(appPath, debugPort, helperPort, true); err != nil {
				message := "重启 ChatGPT Codex 失败：" + err.Error()
				writeLaunchFailureStatus(message, debugPort, helperPort, &appPath)
				appendDiagnosticLog("launcher.restart_existing_launcher_failed", map[string]any{"debug_port": debugPort, "helper_port": helperPort, "guard_port": launcherGuardPort, "error": err.Error()})
				return errors.New(message)
			}
			restartPrepared = true
			instanceLock, acquired, err = waitForLauncherSingleInstanceLock(debugPort, 15*time.Second)
			if err != nil {
				message := "重启 ChatGPT Codex 失败：重新获取单实例锁失败：" + err.Error()
				writeLaunchFailureStatus(message, debugPort, helperPort, &appPath)
				appendDiagnosticLog("launcher.restart_guard_reacquire_failed", map[string]any{"debug_port": debugPort, "helper_port": helperPort, "guard_port": launcherGuardPort, "error": err.Error()})
				return errors.New(message)
			}
			if !acquired {
				message := fmt.Sprintf("重启 ChatGPT Codex 失败：旧后端未退出，端口 %d 仍被占用", launcherGuardPort)
				writeLaunchFailureStatus(message, debugPort, helperPort, &appPath)
				appendDiagnosticLog("launcher.restart_guard_still_busy", map[string]any{"debug_port": debugPort, "helper_port": helperPort, "guard_port": launcherGuardPort})
				return errors.New(message)
			}
		} else {
			appendDiagnosticLog("launcher.already_running", map[string]any{"debug_port": debugPort, "helper_port": helperPort, "guard_port": launcherGuardPort})
			return nil
		}
	}
	if instanceLock == nil {
		err := errors.New("ChatGPT Codex 单实例锁不可用")
		writeLaunchFailureStatus("启动 ChatGPT Codex 失败："+err.Error(), debugPort, helperPort, &appPath)
		appendDiagnosticLog("launcher.guard_missing", map[string]any{"debug_port": debugPort, "helper_port": helperPort, "guard_port": launcherGuardPort, "error": err.Error()})
		return err
	}
	defer instanceLock.release()
	if fallbackLockPath := instanceLock.fallbackPath(); fallbackLockPath != "" {
		appendDiagnosticLog("launcher.guard_fallback", map[string]any{"debug_port": debugPort, "helper_port": helperPort, "requested_guard_port": launcherGuardPort, "fallback_lock_path": fallbackLockPath})
	}
	if options.restart && !restartPrepared {
		if err := prepareFullRestartBeforeLaunch(appPath, debugPort, helperPort, false); err != nil {
			message := "重启 ChatGPT Codex 失败：" + err.Error()
			writeLaunchFailureStatus(message, debugPort, helperPort, &appPath)
			appendDiagnosticLog("launcher.restart_prepare_failed", map[string]any{"debug_port": debugPort, "helper_port": helperPort, "guard_port": launcherGuardPort, "error": err.Error()})
			return errors.New(message)
		}
	}
	runtimeState := &launcherRuntime{settings: settings, debugPort: debugPort, codexAppPath: appPath}
	if shouldQuitRunningCodexBeforeLaunch(appPath, debugPort, options.restart) {
		appendDiagnosticLog("launcher.quit_existing_codex", map[string]any{"codex_app": appPath, "debug_port": debugPort, "restart": options.restart})
		if err := quitMacOSApp(appPath); err != nil {
			appendDiagnosticLog("launcher.quit_existing_codex_failed", map[string]any{"codex_app": appPath, "error": err.Error()})
		}
		if !waitForMacOSAppExit(appPath, 8*time.Second) {
			appendDiagnosticLog("launcher.force_kill_existing_codex", map[string]any{"codex_app": appPath})
			_ = forceKillMacOSApp(appPath)
			_ = waitForMacOSAppExit(appPath, 4*time.Second)
		}
		if activeRelayNeedsLocalProxy(settings) {
			waitForTCPPortFree(localRelayProxyPort, 5*time.Second)
		}
	}
	if completed, recoveryErr := runPendingRemoteControlRecoveries(codexHomeDir()); recoveryErr != nil {
		appendDiagnosticLog("remote_control.pending_recovery_failed", map[string]any{"error": recoveryErr.Error()})
	} else if completed > 0 {
		appendDiagnosticLog("remote_control.pending_recovery_complete", map[string]any{"completed": completed})
	}
	if settings.ProviderSync {
		result := runProviderSyncWithHeldLauncherGuard(codexHomeDir())
		appendDiagnosticLog("provider_sync."+result.Status, map[string]any{
			"targetProvider":      result.TargetProvider,
			"changedSessionFiles": result.ChangedSessionFiles,
			"sqliteRowsUpdated":   result.SQLiteRowsUpdated,
			"message":             result.Message,
		})
	}
	if settings.CodexAppPluginMarketplaceUnlock {
		configured, marketplaceErr := ensureOpenAICuratedMarketplaceConfig(codexHomeDir())
		if marketplaceErr != nil {
			appendDiagnosticLog("launcher.plugin_marketplace_config_failed_nonfatal", map[string]any{"home": codexHomeDir(), "error": marketplaceErr.Error()})
		} else if configured {
			appendDiagnosticLog("launcher.plugin_marketplace_configured", map[string]any{"home": codexHomeDir()})
		}
	}
	if helperNeeded(settings) {
		if err := runtimeState.startHelper(helperPort); err != nil {
			failure := launchStatus{
				Status:      "failed",
				Message:     "启动 ChatGPT Codex helper 失败：" + err.Error(),
				StartedAtMS: uint64(time.Now().UnixMilli()),
				DebugPort:   &debugPort,
				HelperPort:  &helperPort,
				CodexApp:    &appPath,
			}
			_ = atomicWriteJSON(latestStatusPath(), failure)
			appendDiagnosticLog("launcher.helper_failed", map[string]any{"helper_port": helperPort, "error": err.Error()})
			return err
		}
		defer runtimeState.shutdownHelper()
	}
	if activeRelayNeedsLocalProxy(settings) {
		if err := runtimeState.startRelayProxy(localRelayProxyPort); err != nil {
			failure := launchStatus{
				Status:      "failed",
				Message:     "启动 ChatGPT Codex 本地中转代理失败：" + err.Error(),
				StartedAtMS: uint64(time.Now().UnixMilli()),
				DebugPort:   &debugPort,
				HelperPort:  &helperPort,
				CodexApp:    &appPath,
			}
			_ = atomicWriteJSON(latestStatusPath(), failure)
			appendDiagnosticLog("launcher.relay_proxy_failed", map[string]any{"port": localRelayProxyPort, "error": err.Error()})
			return err
		}
		defer runtimeState.shutdownRelayProxy()
	}
	if settings.ComputerUseGuardEnabled {
		result, guardErr := ensureComputerUseGuardConfig(codexHomeDir())
		if guardErr != nil {
			appendDiagnosticLog("computer_use_guard.pre_launch_failed", map[string]any{"error": guardErr.Error()})
		} else {
			appendDiagnosticLog("computer_use_guard.pre_launch_ok", map[string]any{"changed": result.Changed, "notify_exe": result.NotifyExe})
		}
	}
	status := launchStatus{
		Status:      "starting",
		Message:     "ChatGPT Codex launcher starting ChatGPT and waiting for injection.",
		StartedAtMS: uint64(time.Now().UnixMilli()),
		DebugPort:   &debugPort,
		HelperPort:  &helperPort,
		CodexApp:    &appPath,
	}
	_ = atomicWriteJSON(latestStatusPath(), status)
	appendDiagnosticLog("launcher.starting", map[string]any{"debug_port": debugPort, "helper_port": helperPort, "codex_app": appPath, "enhancements": settings.Enhancements})

	launch, err := startCodexApp(appPath, debugPort, settings)
	if err != nil {
		detail := launchFailureDetail(appPath, debugPort, helperPort, err)
		failure := launchStatus{
			Status:      "failed",
			Message:     "启动 ChatGPT Codex 失败：" + err.Error(),
			StartedAtMS: uint64(time.Now().UnixMilli()),
			DebugPort:   &debugPort,
			HelperPort:  &helperPort,
			CodexApp:    &appPath,
			Detail:      detail,
		}
		_ = atomicWriteJSON(latestStatusPath(), failure)
		appendDiagnosticLog("launcher.codex_start_failed", detail)
		return err
	}
	ready := launchStatus{
		Status:      "running",
		Message:     "ChatGPT Codex launcher ready.",
		StartedAtMS: uint64(time.Now().UnixMilli()),
		DebugPort:   &debugPort,
		HelperPort:  &helperPort,
		CodexApp:    &appPath,
	}
	if settings.Enhancements {
		if err := runtimeState.retryInjection(helperPort); err != nil {
			ready.Status = "degraded"
			ready.Message = "ChatGPT 已启动，但 ChatGPT Codex 增强菜单暂时注入失败；中转代理会继续运行，并在后台重试注入：" + err.Error()
			appendDiagnosticLog("launcher.inject_degraded", map[string]any{"debug_port": debugPort, "helper_port": helperPort, "error": err.Error()})
		}
		go runtimeState.bridgeWatchdog(helperPort)
	}
	if settings.ComputerUseGuardEnabled {
		go startComputerUseGuardWatchdog(codexHomeDir())
	}
	_ = atomicWriteJSON(latestStatusPath(), ready)
	appendDiagnosticLog("launcher.ready", map[string]any{"debug_port": debugPort, "helper_port": helperPort, "codex_app": appPath, "launch": launch.logPayload()})
	releaseRestartLock()
	return reapLauncherChild(launch, appPath, debugPort, helperPort)
}

func writeLaunchFailureStatus(message string, debugPort, helperPort uint16, appPath *string) {
	failure := launchStatus{
		Status:      "failed",
		Message:     message,
		StartedAtMS: uint64(time.Now().UnixMilli()),
		DebugPort:   &debugPort,
		HelperPort:  &helperPort,
		CodexApp:    appPath,
	}
	if appPath != nil {
		failure.Detail = launchFailureDetail(*appPath, debugPort, helperPort, errors.New(message))
	}
	_ = atomicWriteJSON(latestStatusPath(), failure)
}

func writeLaunchRestartingStatus(message string, debugPort, helperPort uint16, appPath *string) {
	status := launchStatus{
		Status:      "restarting",
		Message:     message,
		StartedAtMS: uint64(time.Now().UnixMilli()),
		DebugPort:   &debugPort,
		HelperPort:  &helperPort,
		CodexApp:    appPath,
	}
	_ = atomicWriteJSON(latestStatusPath(), status)
}

func prepareFullRestartBeforeLaunch(appPath string, debugPort, helperPort uint16, waitForGuard bool) error {
	writeLaunchRestartingStatus("正在关闭旧 ChatGPT Codex 后端并释放端口。", debugPort, helperPort, &appPath)
	appendDiagnosticLog("launcher.restart_prepare", map[string]any{
		"codex_app":      appPath,
		"debug_port":     debugPort,
		"helper_port":    helperPort,
		"relay_port":     localRelayProxyPort,
		"guard_port":     launcherGuardPort,
		"wait_for_guard": waitForGuard,
	})
	windowsRestart := currentRuntimeGOOS() == "windows"
	closeViaCDP := cdpTargetsAvailable(debugPort, 800*time.Millisecond)
	if windowsRestart {
		chatGPTTargetAvailable := chatGPTCDPTargetAvailable(debugPort, 800*time.Millisecond)
		ownedByTargetApp := windowsCDPPortOwnedByTargetApp(debugPort, appPath)
		closeViaCDP = chatGPTTargetAvailable && ownedByTargetApp
		if chatGPTTargetAvailable && !ownedByTargetApp {
			appendDiagnosticLog("launcher.restart_skip_unowned_cdp", map[string]any{"debug_port": debugPort})
		}
	}
	if closeViaCDP {
		appendDiagnosticLog("launcher.restart_close_cdp", map[string]any{"debug_port": debugPort, "codex_app": appPath})
		shutdownTimeout := 10 * time.Second
		if windowsRestart {
			shutdownTimeout = 4 * time.Second
		}
		if err := requestCodexShutdownViaCDP(debugPort, shutdownTimeout); err != nil {
			if !windowsRestart {
				return fmt.Errorf("关闭旧 ChatGPT 调试端口失败：%w", err)
			}
			appendDiagnosticLog("launcher.restart_close_cdp_failed", map[string]any{
				"debug_port": debugPort,
				"codex_app":  appPath,
				"error":      err.Error(),
			})
		}
	}
	if runtime.GOOS == "darwin" && macOSAppRunning(appPath) {
		appendDiagnosticLog("launcher.restart_quit_macos_app", map[string]any{"codex_app": appPath})
		if err := quitMacOSApp(appPath); err != nil {
			appendDiagnosticLog("launcher.restart_quit_macos_app_failed", map[string]any{"codex_app": appPath, "error": err.Error()})
		}
		if !waitForMacOSAppExit(appPath, 8*time.Second) {
			appendDiagnosticLog("launcher.restart_force_kill_macos_app", map[string]any{"codex_app": appPath})
			_ = forceKillMacOSApp(appPath)
			if !waitForMacOSAppExit(appPath, 4*time.Second) {
				return errors.New("旧 ChatGPT 应用未退出")
			}
		}
	}
	if windowsRestart {
		if err := stopWindowsTargetsBeforeRestart(appPath, debugPort, 8*time.Second); err != nil {
			return err
		}
	}
	if err := waitForRequiredDebugPortFree(debugPort, 12*time.Second); err != nil {
		return err
	}
	if err := waitForRequiredTCPPortFree(helperPort, "Helper", 12*time.Second); err != nil {
		return err
	}
	if err := waitForRequiredTCPPortFree(localRelayProxyPort, "本地中转代理", 12*time.Second); err != nil {
		return err
	}
	if waitForGuard && !windowsRestart {
		if err := waitForRequiredTCPPortFree(launcherGuardPort, "ChatGPT Codex 后端单实例", 12*time.Second); err != nil {
			return err
		}
	}
	writeLaunchRestartingStatus("旧 ChatGPT Codex 后端已退出，正在启动新实例。", debugPort, helperPort, &appPath)
	return nil
}

func stopWindowsTargetsBeforeRestart(appPath string, debugPort uint16, timeout time.Duration) error {
	processIDs, err := terminateWindowsRestartTargets(appPath, debugPort, timeout)
	if len(processIDs) > 0 {
		appendDiagnosticLog("launcher.restart_windows_targets_stopped", map[string]any{
			"process_ids": processIDs,
			"codex_app":   appPath,
		})
	}
	if err != nil {
		return fmt.Errorf("关闭旧 Windows ChatGPT/Codex 进程失败：%w", err)
	}
	return nil
}

func waitForRequiredTCPPortFree(port uint16, label string, timeout time.Duration) error {
	if port == 0 {
		return nil
	}
	if waitForTCPPortFree(port, timeout) {
		return nil
	}
	return fmt.Errorf("%s端口 %d 未释放，可能仍被旧后端占用", label, port)
}

type debugPortReleaseState struct {
	BindError        error
	Accepting        bool
	ListenerPIDs     []uint32
	ListenerPIDKnown bool
	BoundPIDs        []uint32
	BoundPIDKnown    bool
}

func waitForRequiredDebugPortFree(port uint16, timeout time.Duration) error {
	if port == 0 {
		return nil
	}
	if currentRuntimeGOOS() != "windows" {
		return waitForRequiredTCPPortFree(port, "调试", timeout)
	}
	free, state := waitForDebugPortFree(port, timeout)
	if free {
		return nil
	}
	detail := map[string]any{
		"port":               port,
		"accepting":          state.Accepting,
		"listener_pid_known": state.ListenerPIDKnown,
		"listener_pids":      state.ListenerPIDs,
		"bound_pid_known":    state.BoundPIDKnown,
		"bound_pids":         state.BoundPIDs,
	}
	if state.BindError != nil {
		detail["bind_error"] = state.BindError.Error()
	}
	appendDiagnosticLog("launcher.restart_debug_port_unavailable", detail)
	if len(state.ListenerPIDs) > 0 {
		return fmt.Errorf("调试端口 %d 仍有 Windows LISTENING 进程 %v，请退出占用该端口的应用后重试", port, state.ListenerPIDs)
	}
	if state.Accepting {
		return fmt.Errorf("调试端口 %d 仍在接受连接，但 Windows 未能取得监听进程 PID，请退出占用该端口的应用后重试", port)
	}
	if len(state.BoundPIDs) > 0 {
		return fmt.Errorf("调试端口 %d 仍被 Windows BOUND 进程 %v 占用（尚未 LISTENING），请退出占用该端口的应用后重试", port, state.BoundPIDs)
	}
	if state.BindError != nil {
		return fmt.Errorf("调试端口 %d 没有监听进程，但 Windows 无法复用该端口：%v；该端口可能被系统保留或权限策略限制，请改用其他调试端口", port, state.BindError)
	}
	return fmt.Errorf("调试端口 %d 状态未知，Windows 暂时无法确认该端口可复用", port)
}

func waitForDebugPortFree(port uint16, timeout time.Duration) (bool, debugPortReleaseState) {
	deadline := time.Now().Add(timeout)
	state := debugPortReleaseState{}
	for {
		state.BindError = probeTCPPortBind(port)
		state.Accepting = tcpPortAccepting(port)
		state.ListenerPIDs, state.BoundPIDs, state.ListenerPIDKnown, state.BoundPIDKnown = windowsTCPPortStatus(port)
		if debugPortConsideredFree(currentRuntimeGOOS(), state) {
			return true, state
		}
		if time.Now().After(deadline) {
			return false, state
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func debugPortConsideredFree(goos string, state debugPortReleaseState) bool {
	if state.BindError != nil {
		return false
	}
	if goos != "windows" {
		return true
	}
	return !state.Accepting && len(state.ListenerPIDs) == 0 && len(state.BoundPIDs) == 0
}

func probeTCPPortBind(port uint16) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	_ = listener.Close()
	return nil
}

func waitForLauncherSingleInstanceLock(debugPort uint16, timeout time.Duration) (launcherSingleInstanceLock, bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		lock, acquired, err := acquireLauncherSingleInstanceLock(debugPort)
		if err != nil || acquired || time.Now().After(deadline) {
			return lock, acquired, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func parseLaunchRequest(args []string) launchRequest {
	var request launchRequest
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--app-path":
			if i+1 < len(args) {
				request.appPath = strings.TrimSpace(args[i+1])
				i++
			}
		case "--debug-port":
			if i+1 < len(args) {
				if value, err := strconv.ParseUint(args[i+1], 10, 16); err == nil {
					request.debugPort = uint16(value)
				}
				i++
			}
		case "--helper-port":
			if i+1 < len(args) {
				if value, err := strconv.ParseUint(args[i+1], 10, 16); err == nil {
					request.helperPort = uint16(value)
				}
				i++
			}
		case "--restart":
			request.restart = true
		}
	}
	return request
}

func buildCodexLaunchCommand(appPath string, debugPort uint16, extraArgs []string) []string {
	return buildCodexLaunchCommandForSettings(appPath, debugPort, extraArgs, backendSettings{})
}

func buildCodexLaunchCommandForSettings(appPath string, debugPort uint16, extraArgs []string, settings backendSettings) []string {
	args := buildCodexArgumentsForSettings(debugPort, extraArgs, settings)
	if runtime.GOOS == "darwin" && strings.EqualFold(filepath.Ext(appPath), ".app") {
		command := []string{"open", "-W", "-a", appPath, "--args"}
		return append(command, args...)
	}
	executable := buildCodexExecutable(appPath)
	return append([]string{executable}, args...)
}

func buildCodexArguments(debugPort uint16, extraArgs []string) []string {
	return buildCodexArgumentsForSettings(debugPort, extraArgs, backendSettings{})
}

func buildCodexArgumentsForSettings(debugPort uint16, extraArgs []string, settings backendSettings) []string {
	args := []string{
		fmt.Sprintf("--remote-debugging-port=%d", debugPort),
		fmt.Sprintf("--remote-allow-origins=http://127.0.0.1:%d", debugPort),
	}
	if settings.CodexAppFastStartup {
		args = append(args, "--disable-features=CalculateNativeWinOcclusion")
	}
	if settings.CodexAppForceChineseLocale {
		args = append(args, "--lang=zh-CN")
	}
	args = appendUniqueLaunchArguments(args, proxyChromiumArguments(settings)...)
	return append(args, normalizeExtraArgs(extraArgs)...)
}

func appendUniqueLaunchArguments(args []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(args))
	for _, arg := range args {
		if key := strings.TrimSpace(strings.SplitN(arg, "=", 2)[0]); key != "" {
			seen[key] = struct{}{}
		}
	}
	for _, arg := range additions {
		key := strings.TrimSpace(strings.SplitN(arg, "=", 2)[0])
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		args = append(args, arg)
		seen[key] = struct{}{}
	}
	return args
}

func buildCodexExecutable(appPath string) string {
	if runtime.GOOS == "windows" {
		if isWindowsPackagedAppReference(appPath) {
			return ""
		}
		if isWindowsTargetAppExecutableName(filepath.Base(appPath)) {
			return appPath
		}
		if packageRoot := windowsPackageRootFromPath(appPath); packageRoot != "" {
			if metadata, ok := readWindowsPackageManifestMetadata(packageRoot); ok && metadata.Executable != "" {
				candidate := joinPathLike(packageRoot, metadata.Executable)
				if fileExists(candidate) {
					return candidate
				}
			}
		}
		var candidates []string
		for _, subdir := range []string{
			"",
			"app",
			filepath.Join("VFS", "ProgramFilesX64", "ChatGPT"),
			filepath.Join("VFS", "ProgramFilesX64", "OpenAI", "ChatGPT"),
			filepath.Join("VFS", "ProgramFilesX64", "Codex"),
			filepath.Join("VFS", "ProgramFilesX64", "OpenAI", "Codex"),
		} {
			for _, name := range windowsTargetAppExecutableNames() {
				candidates = append(candidates, filepath.Join(appPath, subdir, name))
			}
		}
		for _, candidate := range candidates {
			if fileExists(candidate) {
				return candidate
			}
		}
		if packagedWindowsAppUserModelID(appPath) != "" {
			return ""
		}
		return ""
	}
	if strings.EqualFold(filepath.Ext(appPath), ".app") {
		name := strings.TrimSuffix(filepath.Base(appPath), ".app")
		candidates := []string{
			filepath.Join(appPath, "Contents", "MacOS", name),
			filepath.Join(appPath, "Contents", "MacOS", "ChatGPT"),
		}
		for _, candidate := range candidates {
			if fileExists(candidate) {
				return candidate
			}
		}
	}
	return appPath
}

func startCodexApp(appPath string, debugPort uint16, settings backendSettings) (codexLaunchHandle, error) {
	command := buildCodexLaunchCommandForSettings(appPath, debugPort, settings.CodexExtraArgs, settings)
	if runtime.GOOS == "windows" {
		directLaunchBlocked := len(command) > 0 && isWindowsProtectedCodexDirectLaunchPath(appPath, command[0])
		activation := buildWindowsPackagedActivationForSettings(appPath, debugPort, settings.CodexExtraArgs, settings)
		if len(command) > 0 && strings.TrimSpace(command[0]) != "" && fileExists(command[0]) && !directLaunchBlocked {
			handle, err := startCodexProcess(command, settings)
			if err == nil {
				return handle, nil
			}
			if activation == nil {
				return nil, err
			}
			appendDiagnosticLog("launcher.windows_direct_start_failed", map[string]any{"command": safeCommandForLog(command), "error": err.Error()})
		}
		if activation != nil {
			if tcpPortAccepting(debugPort) {
				return nil, fmt.Errorf("调试端口 %d 已被其他进程占用；请完全退出现有 ChatGPT/Codex 和其他使用该端口的 Chromium 应用后重试", debugPort)
			}
			if processIDs := windowsTargetAppProcessIDs(); len(processIDs) > 0 {
				return nil, fmt.Errorf("检测到未启用调试端口的 ChatGPT/Codex 进程 %v；请完全退出应用后重试", processIDs)
			}
			var attemptErrors []string
			var attempts []windowsPackagedLaunchAttempt
			if alias := windowsPackagedExecutionAlias(activation.appUserModelID); alias != "" {
				aliasCommand := append([]string{alias}, buildCodexArgumentsForSettings(debugPort, settings.CodexExtraArgs, settings)...)
				aliasHandle, aliasErr := startVerifiedWindowsPackagedProcess(aliasCommand, activation.appUserModelID, debugPort, true, "App Execution Alias", "launcher.windows_execution_alias", 20*time.Second, settings)
				if aliasErr == nil {
					return aliasHandle, nil
				}
				attemptErrors = append(attemptErrors, "App Execution Alias: "+aliasErr.Error())
				attempts = append(attempts, windowsPackagedLaunchAttempt{Method: "app_execution_alias", Outcome: windowsPackagedAttemptOutcome(aliasErr)})
			} else {
				attempts = append(attempts, windowsPackagedLaunchAttempt{Method: "app_execution_alias", Outcome: "not_available"})
			}
			if executable := windowsPackagedDirectExecutable(activation.appUserModelID); executable != "" {
				executableCommand := append([]string{executable}, buildCodexArgumentsForSettings(debugPort, settings.CodexExtraArgs, settings)...)
				executableHandle, executableErr := startVerifiedWindowsPackagedProcess(executableCommand, activation.appUserModelID, debugPort, false, "已注册 MSIX 桌面可执行文件", "launcher.windows_packaged_executable", 20*time.Second, settings)
				if executableErr == nil {
					return executableHandle, nil
				}
				attemptErrors = append(attemptErrors, "MSIX 桌面可执行文件: "+executableErr.Error())
				attempts = append(attempts, windowsPackagedLaunchAttempt{Method: "msix_full_trust_executable", Outcome: windowsPackagedAttemptOutcome(executableErr)})
			} else {
				attempts = append(attempts, windowsPackagedLaunchAttempt{Method: "msix_full_trust_executable", Outcome: "not_available"})
			}
			processID, activationErr := activateWindowsPackagedAppWithEnvironment(activation.appUserModelID, activation.arguments, codexLaunchEnvironment(settings))
			if activationErr == nil {
				activation.processID = processID
				if waitForWindowsPackagedCDPPortAvailable(debugPort, processID, activation.appUserModelID, true, 15*time.Second) {
					return activation, nil
				}
				cleanupFailedWindowsPackagedLaunch(processID, debugPort)
				err := packagedCodexDebugPortError(activation.appUserModelID, debugPort, "ApplicationActivationManager")
				appendDiagnosticLog("launcher.windows_packaged_activation_no_cdp", map[string]any{
					"appUserModelId": activation.appUserModelID,
					"debug_port":     debugPort,
					"processId":      processID,
					"error":          err.Error(),
				})
				attemptErrors = append(attemptErrors, "ApplicationActivationManager: "+err.Error())
				attempts = append(attempts, windowsPackagedLaunchAttempt{Method: "application_activation_manager", Outcome: "debug_port_unavailable"})
			} else {
				attemptErrors = append(attemptErrors, "ApplicationActivationManager: "+activationErr.Error())
				attempts = append(attempts, windowsPackagedLaunchAttempt{Method: "application_activation_manager", Outcome: windowsPackagedAttemptOutcome(activationErr)})
			}
			if len(attemptErrors) == 0 {
				attemptErrors = append(attemptErrors, "没有发现清单声明的 App Execution Alias、FullTrust 可执行文件或可用兼容激活方式")
			}
			return nil, &windowsPackagedLaunchError{
				appUserModelID: activation.appUserModelID,
				attempts:       attempts,
				message:        fmt.Sprintf("无法启动 Windows 新版 ChatGPT %s；%s", activation.appUserModelID, strings.Join(attemptErrors, "；")),
			}
		}
		if directLaunchBlocked {
			return nil, windowsProtectedMSIXLaunchError(appPath, command[0])
		}
	}
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return nil, fmt.Errorf("未找到 ChatGPT.exe 或 Codex.exe：%s", appPath)
	}
	if runtime.GOOS == "windows" && !isWindowsAppsExecutionAlias(command[0]) && !fileExists(command[0]) {
		return nil, fmt.Errorf("未找到 ChatGPT.exe 或 Codex.exe：%s", appPath)
	}
	handle, err := startCodexProcess(command, settings)
	if err != nil {
		return nil, err
	}
	return handle, nil
}

type windowsPackagedLaunchAttempt struct {
	Method  string `json:"method"`
	Outcome string `json:"outcome"`
}

type windowsPackagedLaunchError struct {
	appUserModelID string
	attempts       []windowsPackagedLaunchAttempt
	message        string
}

func (err *windowsPackagedLaunchError) Error() string {
	return err.message
}

func windowsPackagedAttemptOutcome(err error) string {
	if err == nil {
		return "success"
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "incorrect function"):
		return "activation_incorrect_function"
	case strings.Contains(text, "调试端口") || strings.Contains(text, "remote-debugging-port") || strings.Contains(text, "cdp"):
		return "debug_port_unavailable"
	case strings.Contains(text, "无法启动") || strings.Contains(text, "executable file not found") || strings.Contains(text, "file not found") || strings.Contains(text, "cannot find the file") || strings.Contains(text, "cannot find the path"):
		return "process_start_failed"
	default:
		return "activation_failed"
	}
}

func startVerifiedWindowsPackagedProcess(command []string, appUserModelID string, debugPort uint16, requirePackageIdentity bool, method, eventPrefix string, timeout time.Duration, settings backendSettings) (codexLaunchHandle, error) {
	handle, err := startCodexProcess(command, settings)
	if err != nil {
		appendDiagnosticLog(eventPrefix+"_failed", map[string]any{
			"appUserModelId": appUserModelID,
			"executable":     filepath.Base(command[0]),
			"error":          err.Error(),
		})
		return nil, err
	}
	processLaunch, ok := handle.(*codexProcessLaunch)
	if !ok || processLaunch.cmd == nil || processLaunch.cmd.Process == nil {
		_ = handle.terminate()
		return nil, errors.New("Windows 打包应用启动后没有可验证的进程")
	}
	processID := uint32(processLaunch.cmd.Process.Pid)
	if waitForWindowsPackagedCDPPortAvailable(debugPort, processID, appUserModelID, requirePackageIdentity, timeout) {
		processLaunch.windowsPackageFamily = windowsPackageFamilyFromAppUserModelID(appUserModelID)
		processLaunch.windowsRootProcessID = processID
		processLaunch.windowsRequirePackageIdentity = requirePackageIdentity
		processLaunch.debugPort = debugPort
		return handle, nil
	}
	cleanupFailedWindowsPackagedLaunch(processID, debugPort)
	err = packagedCodexDebugPortError(appUserModelID, debugPort, method)
	appendDiagnosticLog(eventPrefix+"_no_cdp", map[string]any{
		"appUserModelId": appUserModelID,
		"debug_port":     debugPort,
		"executable":     filepath.Base(command[0]),
		"processId":      processID,
		"error":          err.Error(),
	})
	return nil, err
}

func cleanupFailedWindowsPackagedLaunch(processID uint32, debugPort uint16) {
	_ = terminateWindowsProcessTree(processID)
	terminateWindowsTargetAppProcesses()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(windowsTargetAppProcessIDs()) == 0 && !tcpPortAccepting(debugPort) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func windowsProtectedMSIXLaunchError(appPath, executable string) error {
	target := strings.TrimSpace(executable)
	if target == "" {
		target = strings.TrimSpace(appPath)
	}
	appUserModelID := packagedWindowsAppUserModelID(appPath)
	if appUserModelID != "" {
		return fmt.Errorf("当前选择的是 Windows Store/MSIX 版新版 ChatGPT（%s），不能直接执行受保护的 WindowsApps 文件，且未能通过应用标识启动。已跳过：%s", appUserModelID, target)
	}
	return fmt.Errorf("当前选择的是 Windows Store/MSIX 版新版 ChatGPT，不能直接执行受保护的 WindowsApps 文件，且未解析到可用的应用启动标识。已跳过：%s", target)
}

func buildWindowsPackagedActivation(appPath string, debugPort uint16, extraArgs []string) *windowsPackagedActivation {
	return buildWindowsPackagedActivationForSettings(appPath, debugPort, extraArgs, backendSettings{})
}

func buildWindowsPackagedActivationForSettings(appPath string, debugPort uint16, extraArgs []string, settings backendSettings) *windowsPackagedActivation {
	if runtime.GOOS != "windows" {
		return nil
	}
	appUserModelID := packagedWindowsAppUserModelID(appPath)
	return newWindowsPackagedActivation(appUserModelID, debugPort, extraArgs, settings)
}

func newWindowsPackagedActivation(appUserModelID string, debugPort uint16, extraArgs []string, settings backendSettings) *windowsPackagedActivation {
	reference := normalizeWindowsPackagedAppReference("aumid:" + strings.TrimSpace(appUserModelID))
	if reference == "" {
		return nil
	}
	appUserModelID = strings.TrimPrefix(reference, "aumid:")
	if appUserModelID == "" {
		return nil
	}
	return &windowsPackagedActivation{
		appUserModelID:    appUserModelID,
		packageFamilyName: windowsPackageFamilyFromAppUserModelID(appUserModelID),
		arguments:         commandLineArguments(buildCodexArgumentsForSettings(debugPort, extraArgs, settings)),
		debugPort:         debugPort,
	}
}

func windowsPackagedExplorerCommand(appUserModelID string, args []string) []string {
	return []string{"explorer.exe", `shell:AppsFolder\` + appUserModelID}
}

func packagedCodexDebugPortError(appUserModelID string, debugPort uint16, method string) error {
	return fmt.Errorf("%s 已启动 Windows Store/MSIX 新版 ChatGPT %s，但未检测到调试端口 %d；应用已安装，请完全退出已有 ChatGPT 进程后重试；若仍无端口，则当前版本可能不接受 --remote-debugging-port，增强注入暂不可用", method, appUserModelID, debugPort)
}

func launchFailureDetail(appPath string, debugPort, helperPort uint16, err error) map[string]any {
	detail := map[string]any{
		"codex_app":          appPath,
		"debug_port":         debugPort,
		"helper_port":        helperPort,
		"error":              err.Error(),
		"cdp_port_available": cdpTargetsAvailable(debugPort, 800*time.Millisecond),
		"recommended_action": "确认新版 ChatGPT 可从开始菜单启动，并完全退出已有 ChatGPT 进程后重试；若 MSIX 激活后仍没有调试端口，请使用支持该新版应用的工具版本，或选择 WindowsApps 外可直接执行的 ChatGPT.exe / Codex.exe。",
	}
	var packagedError *windowsPackagedLaunchError
	if errors.As(err, &packagedError) {
		detail["windows_launch_attempts"] = packagedError.attempts
	}
	if runtime.GOOS == "windows" {
		if activation := buildWindowsPackagedActivation(appPath, debugPort, nil); activation != nil {
			detail["appUserModelId"] = activation.appUserModelID
			detail["activation_method"] = "packaged_activation"
			if alias := windowsPackagedExecutionAlias(activation.appUserModelID); alias != "" {
				detail["activation_method"] = "app_execution_alias"
				detail["executionAlias"] = alias
			} else if windowsPackagedDirectExecutable(activation.appUserModelID) != "" {
				detail["activation_method"] = "msix_full_trust_executable"
				detail["packagedExecutableAvailable"] = true
			}
		} else if executable := buildCodexExecutable(appPath); executable != "" {
			detail["executable"] = executable
			detail["activation_method"] = "executable"
		}
	}
	return detail
}

func commandLineArguments(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, quoteWindowsArgument(arg))
	}
	return strings.Join(quoted, " ")
}

func quoteWindowsArgument(arg string) string {
	if arg != "" && !strings.ContainsAny(arg, " \t\"") {
		return arg
	}
	var output strings.Builder
	output.WriteByte('"')
	backslashes := 0
	for _, ch := range arg {
		switch ch {
		case '\\':
			backslashes++
		case '"':
			output.WriteString(strings.Repeat("\\", backslashes*2+1))
			output.WriteRune(ch)
			backslashes = 0
		default:
			output.WriteString(strings.Repeat("\\", backslashes))
			output.WriteRune(ch)
			backslashes = 0
		}
	}
	output.WriteString(strings.Repeat("\\", backslashes*2))
	output.WriteByte('"')
	return output.String()
}

func codexLaunchEnvironment(settingsValues ...backendSettings) []string {
	var environment []string
	switch runtime.GOOS {
	case "darwin":
		environment = append(environment, "PATH="+defaultGUIPath)
	}
	if len(settingsValues) > 0 {
		settings := settingsValues[0]
		if nativeDesktopProxyEnabled(settings) {
			environment = append(environment, proxyEnvironmentForSettings(settings)...)
		}
	}
	// Image relay variables are only valid for a dedicated CLI process;
	// inheriting them here would affect native modules such as remote control.
	return environment
}

func imageRelayCLIEnvironment(settings backendSettings) []string {
	profile := activeRelayProfile(normalizeSettings(settings))
	if profile.Protocol != "responses" || !usesSeparateImageGenerationAPI(profile) {
		return nil
	}
	return []string{
		"OPENAI_API_KEY=" + imageRelayCLIAPIKey,
		fmt.Sprintf("OPENAI_BASE_URL=http://127.0.0.1:%d/v1", localRelayProxyPort),
	}
}

func mergeLaunchEnvironment(base, overrides []string) []string {
	if len(overrides) == 0 {
		return append([]string(nil), base...)
	}
	keys := map[string]struct{}{}
	for _, entry := range overrides {
		key, _, ok := strings.Cut(entry, "=")
		if ok && strings.TrimSpace(key) != "" {
			keys[strings.ToUpper(key)] = struct{}{}
		}
	}
	merged := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, replaced := keys[strings.ToUpper(key)]; replaced {
				continue
			}
		}
		merged = append(merged, entry)
	}
	return append(merged, overrides...)
}

func shouldQuitRunningCodexBeforeLaunch(appPath string, debugPort uint16, restart bool) bool {
	if runtime.GOOS != "darwin" || !strings.EqualFold(filepath.Ext(appPath), ".app") {
		return false
	}
	if !macOSAppRunning(appPath) {
		return false
	}
	if restart {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	if _, err := listCDPTargets(ctx, debugPort); err == nil {
		return false
	}
	return true
}

func cdpTargetsAvailable(debugPort uint16, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	targets, err := listCDPTargets(ctx, debugPort)
	if err != nil {
		return false
	}
	for _, target := range targets {
		if isPrimaryCodexCDPPageTarget(target) {
			return true
		}
	}
	return false
}

func chatGPTCDPTargetAvailable(debugPort uint16, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	targets, err := listCDPTargets(ctx, debugPort)
	if err != nil {
		return false
	}
	for _, target := range targets {
		if isPrimaryCodexCDPPageTarget(target) {
			return true
		}
	}
	return false
}

func requestCodexShutdownViaCDP(debugPort uint16, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	targets, err := listCDPTargets(ctx, debugPort)
	if err != nil {
		return fmt.Errorf("无法连接现有 Codex 调试端口 %d：%w", debugPort, err)
	}
	target, err := pickCDPPageTarget(targets)
	if err != nil {
		return err
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, target.WebSocketDebuggerURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"id": 1, "method": "Browser.close", "params": map[string]any{}}); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !tcpPortAccepting(debugPort) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("等待 Codex 调试端口 %d 关闭超时", debugPort)
}

func waitForTCPPortFree(port uint16, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	address := fmt.Sprintf("127.0.0.1:%d", port)
	for {
		listener, err := net.Listen("tcp", address)
		if err == nil {
			_ = listener.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func macOSAppRunning(appPath string) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	name := strings.TrimSuffix(filepath.Base(appPath), ".app")
	if strings.TrimSpace(name) == "" {
		name = "Codex"
	}
	cmd := exec.Command("osascript", "-e", fmt.Sprintf(`application "%s" is running`, strings.ReplaceAll(name, `"`, `\"`)))
	hideSubprocessWindow(cmd)
	out, err := cmd.Output()
	return err == nil && strings.EqualFold(strings.TrimSpace(string(out)), "true")
}

func quitMacOSApp(appPath string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	name := strings.TrimSuffix(filepath.Base(appPath), ".app")
	if strings.TrimSpace(name) == "" {
		name = "Codex"
	}
	cmd := exec.Command("osascript", "-e", fmt.Sprintf(`tell application "%s" to quit`, strings.ReplaceAll(name, `"`, `\"`)))
	hideSubprocessWindow(cmd)
	return cmd.Run()
}

func waitForMacOSAppExit(appPath string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !macOSAppRunning(appPath) {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return !macOSAppRunning(appPath)
}

func forceKillMacOSApp(appPath string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	executable := buildCodexExecutable(appPath)
	if executable != "" {
		cmd := exec.Command("pkill", "-x", filepath.Base(executable))
		hideSubprocessWindow(cmd)
		_ = cmd.Run()
	}
	name := strings.TrimSuffix(filepath.Base(appPath), ".app")
	if strings.TrimSpace(name) != "" {
		cmd := exec.Command("pkill", "-x", name)
		hideSubprocessWindow(cmd)
		_ = cmd.Run()
	}
	return nil
}

func terminateLaunchedCodex(launch codexLaunchHandle, appPath string) {
	if launch != nil {
		_ = launch.terminate()
	}
}

func safeCommandForLog(command []string) []string {
	if len(command) == 0 {
		return []string{}
	}
	return []string{command[0], fmt.Sprintf("[arguments omitted: %d]", len(command)-1)}
}

func reapLauncherChild(launch codexLaunchHandle, appPath string, debugPort, helperPort uint16) error {
	err := launch.wait()
	if launchCDPAvailable(launch, debugPort, 1200*time.Millisecond) {
		status := launchStatus{
			Status:      "running",
			Message:     "ChatGPT 主窗口仍在运行，ChatGPT Codex helper 继续驻留。",
			StartedAtMS: uint64(time.Now().UnixMilli()),
			DebugPort:   &debugPort,
			HelperPort:  &helperPort,
			CodexApp:    &appPath,
		}
		_ = atomicWriteJSON(latestStatusPath(), status)
		detail := map[string]any{"debug_port": debugPort, "helper_port": helperPort, "codex_app": appPath}
		if err != nil {
			detail["child_error"] = err.Error()
		}
		appendDiagnosticLog("launcher.child_exited_cdp_alive", detail)
		waitForLaunchCDPClosed(launch, debugPort)
		err = nil
	} else {
		detail := map[string]any{"debug_port": debugPort, "helper_port": helperPort, "codex_app": appPath}
		if err != nil {
			detail["child_error"] = err.Error()
		}
		appendDiagnosticLog("launcher.child_exited_cdp_missing", detail)
	}
	message := "Codex exited."
	statusText := "exited"
	if err != nil {
		message = "Codex exited with error: " + err.Error()
		statusText = "failed"
	}
	status := launchStatus{
		Status:      statusText,
		Message:     message,
		StartedAtMS: uint64(time.Now().UnixMilli()),
		DebugPort:   &debugPort,
		HelperPort:  &helperPort,
		CodexApp:    &appPath,
	}
	_ = atomicWriteJSON(latestStatusPath(), status)
	appendDiagnosticLog("launcher."+statusText, map[string]any{"debug_port": debugPort, "helper_port": helperPort, "codex_app": appPath, "message": message})
	return err
}

func waitForLaunchCDPClosed(launch codexLaunchHandle, debugPort uint16) {
	for launchCDPAvailable(launch, debugPort, 1200*time.Millisecond) {
		time.Sleep(time.Second)
	}
}

func launchCDPAvailable(launch codexLaunchHandle, debugPort uint16, timeout time.Duration) bool {
	switch typed := launch.(type) {
	case *codexProcessLaunch:
		if typed.windowsRootProcessID != 0 && typed.windowsPackageFamily != "" {
			if typed.windowsRequirePackageIdentity {
				return chatGPTCDPTargetAvailable(debugPort, timeout) &&
					windowsCDPPortOwnedByPackage(debugPort, typed.windowsPackageFamily)
			}
			return chatGPTCDPTargetAvailable(debugPort, timeout) && windowsCDPPortOwnedByProcess(debugPort, typed.windowsRootProcessID)
		}
	case *windowsPackagedActivation:
		if typed.processID != 0 && typed.packageFamilyName != "" {
			return chatGPTCDPTargetAvailable(debugPort, timeout) &&
				windowsCDPPortOwnedByPackage(debugPort, typed.packageFamilyName)
		}
	}
	return cdpTargetsAvailable(debugPort, timeout)
}

func helperNeeded(settings backendSettings) bool {
	imageOverlayNeeded := settings.CodexAppImageOverlayEnabled && strings.TrimSpace(settings.CodexAppImageOverlayPath) != ""
	return settings.Enhancements || imageOverlayNeeded || settings.ProxyRealtimeEnabled || activeRelayUsesProtocolProxy(settings) || activeRelayNeedsLocalProxy(settings)
}

func activeRelayNeedsLocalProxy(settings backendSettings) bool {
	active := activeRelayProfile(settings)
	// Remote Control starts on the official ChatGPT transport, but its model
	// session follows openai_base_url in official-mix mode. Keep the local
	// proxy alive for that path as well, matching the upstream launcher.
	if settings.RelayProfilesEnabled && active.RelayMode == "official" && active.OfficialMixAPIKey {
		return true
	}
	if active.RelayMode == "official" {
		return false
	}
	return true
}

type codexLaunchHandle interface {
	wait() error
	terminate() error
	logPayload() map[string]any
}

type codexProcessLaunch struct {
	cmd                           *exec.Cmd
	command                       []string
	windowsPackageFamily          string
	windowsRootProcessID          uint32
	windowsRequirePackageIdentity bool
	debugPort                     uint16
}

func startCodexProcess(command []string, settingsValues ...backendSettings) (codexLaunchHandle, error) {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = mergeLaunchEnvironment(os.Environ(), codexLaunchEnvironment(settingsValues...))
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	hideSubprocessWindow(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("无法启动 ChatGPT 可执行文件 %s：%w", command[0], err)
	}
	return &codexProcessLaunch{cmd: cmd, command: append([]string(nil), command...)}, nil
}

func (launch *codexProcessLaunch) wait() error {
	return launch.cmd.Wait()
}

func (launch *codexProcessLaunch) terminate() error {
	if launch.cmd != nil && launch.cmd.Process != nil {
		if launch.windowsRootProcessID != 0 {
			return terminateWindowsProcessTree(launch.windowsRootProcessID)
		}
		return launch.cmd.Process.Kill()
	}
	return nil
}

func (launch *codexProcessLaunch) logPayload() map[string]any {
	return map[string]any{
		"type":    "process",
		"command": safeCommandForLog(launch.command),
	}
}

type windowsPackagedActivation struct {
	appUserModelID    string
	packageFamilyName string
	arguments         string
	debugPort         uint16
	processID         uint32
}

type windowsPackagedExplorerLaunch struct {
	appUserModelID string
	command        []string
	debugPort      uint16
}

func (launch *windowsPackagedExplorerLaunch) wait() error {
	for {
		if !tcpPortAccepting(launch.debugPort) {
			return nil
		}
		time.Sleep(time.Second)
	}
}

func (launch *windowsPackagedExplorerLaunch) terminate() error {
	return nil
}

func (launch *windowsPackagedExplorerLaunch) logPayload() map[string]any {
	return map[string]any{
		"type":           "windows_packaged_explorer",
		"appUserModelId": launch.appUserModelID,
		"command":        safeCommandForLog(launch.command),
	}
}

func waitForCDPPortAvailable(port uint16, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cdpTargetsAvailable(port, 700*time.Millisecond) {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return cdpTargetsAvailable(port, 700*time.Millisecond)
}

func waitForWindowsPackagedCDPPortAvailable(port uint16, processID uint32, appUserModelID string, requirePackageIdentity bool, timeout time.Duration) bool {
	packageFamily := windowsPackageFamilyFromAppUserModelID(appUserModelID)
	if packageFamily == "" {
		return false
	}
	owned := func() bool {
		if requirePackageIdentity {
			return windowsCDPPortOwnedByPackagedProcess(port, processID, packageFamily)
		}
		return windowsCDPPortOwnedByProcess(port, processID)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if chatGPTCDPTargetAvailable(port, 700*time.Millisecond) && owned() {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return chatGPTCDPTargetAvailable(port, 700*time.Millisecond) && owned()
}

func tcpPortAccepting(port uint16) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 400*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (activation *windowsPackagedActivation) wait() error {
	if activation.processID == 0 || activation.packageFamilyName == "" {
		return packagedCodexDebugPortError(activation.appUserModelID, activation.debugPort, "MSIX")
	}
	for {
		if !launchCDPAvailable(activation, activation.debugPort, 1200*time.Millisecond) {
			return nil
		}
		time.Sleep(time.Second)
	}
}

func (activation *windowsPackagedActivation) terminate() error {
	return terminateWindowsProcessTree(activation.processID)
}

func (activation *windowsPackagedActivation) logPayload() map[string]any {
	return map[string]any{
		"type":           "windows_packaged_activation",
		"appUserModelId": activation.appUserModelID,
		"argumentCount":  len(strings.Fields(activation.arguments)),
		"processId":      activation.processID,
	}
}
