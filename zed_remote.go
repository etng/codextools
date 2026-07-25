package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

type zedRemoteError struct {
	message string
}

func (e zedRemoteError) Error() string {
	return e.message
}

type sshTarget struct {
	User string
	Host string
	Port *uint16
}

type zedRemoteProject struct {
	ID             string    `json:"id"`
	Label          string    `json:"label"`
	HostID         string    `json:"hostId"`
	SSH            sshTarget `json:"ssh"`
	Path           string    `json:"path"`
	URL            string    `json:"url"`
	Source         string    `json:"source"`
	LastOpenedAtMS *int64    `json:"lastOpenedAtMs,omitempty"`
	IsCurrent      bool      `json:"isCurrent"`
}

type zedRemoteProjectRegistry struct {
	Projects []zedRemoteProject `json:"projects"`
}

func zedRemoteStatusValue() map[string]any {
	appPath := findZedAppPath()
	cliPath := executableInPath("zed")
	platformSupported := runtime.GOOS == "darwin" || runtime.GOOS == "windows" || runtime.GOOS == "linux"
	status := "ok"
	if !platformSupported {
		status = "failed"
	}
	return map[string]any{
		"status":            status,
		"platformSupported": platformSupported,
		"zedAppFound":       appPath != "",
		"zedCliFound":       cliPath != "",
		"zedAppPath":        appPath,
		"zedCliPath":        cliPath,
	}
}

func findZedAppPath() string {
	candidates := []string{
		"/Applications/Zed.app",
		"/Applications/Zed Preview.app",
		"/Applications/Zed Nightly.app",
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates,
			filepath.Join(home, "Applications", "Zed.app"),
			filepath.Join(home, "Applications", "Zed Preview.app"),
			filepath.Join(home, "Applications", "Zed Nightly.app"),
		)
	}
	for _, candidate := range candidates {
		if fileExists(candidate) {
			return candidate
		}
	}
	return ""
}

func splitSSHAuthority(value string) (string, string, *uint16, error) {
	authority := strings.TrimSpace(value)
	if authority == "" {
		return "", "", nil, nil
	}
	user := ""
	if index := strings.LastIndex(authority, "@"); index >= 0 {
		user = strings.TrimSpace(authority[:index])
		authority = authority[index+1:]
	}
	if strings.HasPrefix(authority, "[") {
		if closeIndex := strings.Index(authority, "]"); closeIndex >= 0 {
			host := strings.TrimSpace(authority[:closeIndex+1])
			suffix := authority[closeIndex+1:]
			var port *uint16
			if strings.HasPrefix(suffix, ":") {
				parsed, err := parseSSHPort(strings.TrimPrefix(suffix, ":"))
				if err != nil {
					return "", "", nil, err
				}
				port = parsed
			}
			return user, host, port, nil
		}
		return user, strings.TrimSpace(authority), nil, nil
	}
	if strings.Count(authority, ":") == 1 {
		host, rawPort, _ := strings.Cut(authority, ":")
		if rawPort != "" && allASCII(rawPort, func(ch byte) bool { return ch >= '0' && ch <= '9' }) {
			port, err := parseSSHPort(rawPort)
			if err != nil {
				return "", "", nil, err
			}
			return user, strings.TrimSpace(host), port, nil
		}
	}
	return user, strings.TrimSpace(authority), nil, nil
}

func parseSSHPort(value string) (*uint16, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 16)
	if err != nil || parsed == 0 {
		return nil, zedRemoteError{"Invalid SSH port"}
	}
	port := uint16(parsed)
	return &port, nil
}

func parseSSHPortAny(value any) (*uint16, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case float64:
		if typed <= 0 || typed > 65535 || typed != float64(uint16(typed)) {
			return nil, zedRemoteError{"Invalid SSH port"}
		}
		port := uint16(typed)
		return &port, nil
	case int:
		if typed <= 0 || typed > 65535 {
			return nil, zedRemoteError{"Invalid SSH port"}
		}
		port := uint16(typed)
		return &port, nil
	case string:
		return parseSSHPort(typed)
	default:
		return nil, zedRemoteError{"Invalid SSH port"}
	}
}

func validateSSHHost(host string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return "", zedRemoteError{"Cannot determine remote SSH host for this file"}
	}
	for _, ch := range host {
		if ch <= 0x20 || strings.ContainsRune("/?#@", ch) {
			return "", zedRemoteError{"Invalid SSH host"}
		}
	}
	if strings.HasPrefix(host, "[") || strings.HasSuffix(host, "]") {
		if !(strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]")) {
			return "", zedRemoteError{"Invalid SSH host"}
		}
		if net.ParseIP(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")) == nil {
			return "", zedRemoteError{"Invalid SSH host"}
		}
		return host, nil
	}
	if strings.ContainsAny(host, "[]") {
		return "", zedRemoteError{"Invalid SSH host"}
	}
	return host, nil
}

func sshTargetFromPayload(payload map[string]any) (sshTarget, error) {
	ssh, _ := payload["ssh"].(map[string]any)
	rawHost := firstString(ssh["host"], ssh["hostname"], ssh["hostName"])
	authorityUser, authorityHost, authorityPort, err := splitSSHAuthority(rawHost)
	if err != nil {
		return sshTarget{}, err
	}
	user := firstString(ssh["user"], ssh["username"], authorityUser)
	host, err := validateSSHHost(authorityHost)
	if err != nil {
		return sshTarget{}, err
	}
	port := authorityPort
	if rawPort, ok := ssh["port"]; ok {
		if text, ok := rawPort.(string); !ok || strings.TrimSpace(text) != "" {
			port, err = parseSSHPortAny(rawPort)
			if err != nil {
				return sshTarget{}, err
			}
		}
	}
	return sshTarget{User: user, Host: host, Port: port}, nil
}

func encodeRemotePath(path string) (string, error) {
	if path == "" {
		return "", zedRemoteError{"Remote path is required"}
	}
	if !strings.HasPrefix(path, "/") {
		return "", zedRemoteError{"Remote path must be absolute"}
	}
	segments := strings.Split(path, "/")
	for index, segment := range segments {
		segments[index] = percentEncodeSegment(segment)
	}
	return strings.Join(segments, "/"), nil
}

func buildZedRemoteURL(target sshTarget, path string) (string, error) {
	host, err := validateSSHHost(target.Host)
	if err != nil {
		return "", err
	}
	encodedPath, err := encodeRemotePath(path)
	if err != nil {
		return "", err
	}
	userPrefix := ""
	if strings.TrimSpace(target.User) != "" {
		userPrefix = percentEncodeSegment(strings.TrimSpace(target.User)) + "@"
	}
	portSuffix := ""
	if target.Port != nil {
		portSuffix = ":" + strconv.Itoa(int(*target.Port))
	}
	return "ssh://" + userPrefix + host + portSuffix + encodedPath, nil
}

func percentEncodeSegment(segment string) string {
	var out strings.Builder
	for _, b := range []byte(segment) {
		ch := rune(b)
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '.' || ch == '_' || ch == '~' {
			out.WriteByte(b)
		} else {
			out.WriteString(fmt.Sprintf("%%%02X", b))
		}
	}
	return out.String()
}

func zedCLIArgsForStrategy(strategy, rawURL string) []string {
	args := []string{}
	switch normalizeZedOpenStrategy(strategy) {
	case "addToFocusedWorkspace":
		args = append(args, "-a")
	case "reuseWindow":
		args = append(args, "-r")
	case "newWindow":
		args = append(args, "-n")
	case "default":
	}
	return append(args, rawURL)
}

func launchZedURL(rawURL string) error {
	return launchZedURLWithStrategy(rawURL, "default")
}

func launchZedURLWithStrategy(rawURL, strategy string) error {
	if cliPath := executableInPath("zed"); cliPath != "" {
		cmd := exec.Command(cliPath, zedCLIArgsForStrategy(strategy, rawURL)...)
		hideSubprocessWindow(cmd)
		return cmd.Start()
	}
	if runtime.GOOS == "darwin" {
		if appPath := findZedAppPath(); appPath != "" {
			cmd := exec.Command("open", "-a", appPath, rawURL)
			hideSubprocessWindow(cmd)
			return cmd.Start()
		}
	}
	return zedRemoteError{"Zed CLI is not installed or not available on PATH"}
}

func targetFromManagedRemoteConnection(connection map[string]any) (sshTarget, error) {
	sshHost := firstString(connection["sshHost"], connection["hostname"])
	sshAlias := firstString(connection["sshAlias"], connection["alias"])
	authorityUser, authorityHost, authorityPort, err := splitSSHAuthority(sshHost)
	if err != nil {
		return sshTarget{}, err
	}
	host := firstString(authorityHost, sshAlias)
	user := firstString(connection["sshUser"], connection["user"], authorityUser)
	port := authorityPort
	if rawPort, ok := connection["sshPort"]; ok {
		if text, ok := rawPort.(string); !ok || strings.TrimSpace(text) != "" {
			port, err = parseSSHPortAny(rawPort)
			if err != nil {
				return sshTarget{}, err
			}
		}
	}
	host, err = validateSSHHost(host)
	if err != nil {
		return sshTarget{}, err
	}
	return sshTarget{User: user, Host: host, Port: port}, nil
}

func resolveSSHTargetFromGlobalState(state map[string]any, hostID string) (sshTarget, error) {
	hostID = strings.TrimSpace(hostID)
	if hostID == "" {
		return sshTarget{}, zedRemoteError{"Remote host id is required"}
	}
	connections, _ := state["codex-managed-remote-connections"].([]any)
	for _, item := range connections {
		connection, _ := item.(map[string]any)
		if stringFromAny(connection["hostId"]) != hostID {
			continue
		}
		return targetFromManagedRemoteConnection(connection)
	}
	return sshTarget{}, zedRemoteError{"Cannot resolve remote SSH host for this file"}
}

func resolveSSHTargetForHostID(hostID string) (sshTarget, error) {
	data, err := os.ReadFile(codexGlobalStatePath(codexHomeDir()))
	if err != nil {
		return sshTarget{}, fmt.Errorf("Cannot read Codex remote connection state: %w", err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		return sshTarget{}, fmt.Errorf("Cannot parse Codex remote connection state: %w", err)
	}
	return resolveSSHTargetFromGlobalState(state, hostID)
}

func resolveSSHTargetResponse(payload map[string]any) map[string]any {
	target, err := resolveSSHTargetForHostID(stringFromAny(payload["hostId"]))
	if err != nil {
		return map[string]any{"status": "failed", "message": err.Error()}
	}
	return map[string]any{
		"status": "ok",
		"ssh": map[string]any{
			"user": target.User,
			"host": target.Host,
			"port": target.Port,
		},
	}
}

func zedOpenRemote(payload map[string]any) map[string]any {
	target, err := sshTargetFromPayload(payload)
	if err == nil {
		var url string
		url, err = buildZedRemoteURL(target, stringFromAny(payload["path"]))
		if err == nil {
			strategy := normalizeZedOpenStrategy(firstString(payload["strategy"], loadSettings().ZedRemoteOpenStrategy))
			err = launchZedURLWithStrategy(url, strategy)
			if err == nil {
				settings := loadSettings()
				if settings.ZedRemoteProjectRegistryEnabled && payload["remember"] != false {
					if _, rememberErr := rememberZedRemoteProject(payload, &target, url); rememberErr != nil {
						appendDiagnosticLog("zed_remote.remember_failed", map[string]any{"error": rememberErr.Error()})
					}
				}
				return map[string]any{"status": "ok", "url": url, "strategy": strategy}
			}
		}
	}
	return map[string]any{"status": "failed", "message": err.Error()}
}

func zedRemoteProjectsResponse(payload map[string]any) map[string]any {
	settings := loadSettings()
	if !settings.ZedRemoteProjectRegistryEnabled {
		return map[string]any{"status": "ok", "projects": []zedRemoteProject{}, "message": "Zed 项目记录已关闭"}
	}
	projects, err := listZedRemoteProjects(payload, zedRegistryPath(), "")
	if err != nil {
		return map[string]any{"status": "failed", "message": err.Error()}
	}
	return map[string]any{"status": "ok", "projects": projects}
}

func zedRememberRemoteProjectResponse(payload map[string]any) map[string]any {
	project, err := rememberZedRemoteProject(payload, nil, "")
	if err != nil {
		return map[string]any{"status": "failed", "message": err.Error()}
	}
	return map[string]any{"status": "ok", "project": project}
}

func zedForgetRemoteProjectResponse(payload map[string]any) map[string]any {
	removed, err := forgetZedRemoteProject(payload)
	if err != nil {
		return map[string]any{"status": "failed", "message": err.Error()}
	}
	return map[string]any{"status": "ok", "removed": removed}
}

func (s *server) zedRemoteProjects(args map[string]any) commandResult {
	result := zedRemoteProjectsResponse(mapArgOrSelf(args))
	status := stringFromAny(result["status"])
	if status == "ok" {
		result["message"] = "Zed 远程项目已读取。"
		return commandResult(result)
	}
	if result["message"] == nil {
		result["message"] = "读取 Zed 远程项目失败。"
	}
	return commandResult(result)
}

func (s *server) zedRemoteOpen(args map[string]any) commandResult {
	result := zedOpenRemote(mapArgOrSelf(args))
	if stringFromAny(result["status"]) == "ok" {
		result["message"] = "已请求 Zed 打开远程项目。"
		return commandResult(result)
	}
	if result["message"] == nil {
		result["message"] = "打开 Zed 远程项目失败。"
	}
	return commandResult(result)
}

func (s *server) zedRemoteForgetProject(args map[string]any) commandResult {
	result := zedForgetRemoteProjectResponse(mapArgOrSelf(args))
	if stringFromAny(result["status"]) == "ok" {
		projects := zedRemoteProjectsResponse(map[string]any{})
		projects["removed"] = result["removed"]
		projects["message"] = "Zed 最近项目记录已移除。"
		return commandResult(projects)
	}
	if result["message"] == nil {
		result["message"] = "移除 Zed 最近项目失败。"
	}
	return commandResult(result)
}

func mapArgOrSelf(args map[string]any) map[string]any {
	if request := mapArg(args, "request"); len(request) > 0 {
		return request
	}
	return args
}

func zedFallbackRequestResponse(payload map[string]any) map[string]any {
	data, err := os.ReadFile(codexGlobalStatePath(codexHomeDir()))
	if err != nil {
		return map[string]any{"status": "failed", "message": "Cannot read Codex remote connection state: " + err.Error()}
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		return map[string]any{"status": "failed", "message": "Cannot parse Codex remote connection state: " + err.Error()}
	}
	request, err := fallbackOpenRequestFromGlobalStateWithContext(
		state,
		stringFromAny(payload["hostId"]),
		stringFromAny(payload["threadId"]),
		stringFromAny(payload["workspaceRoot"]),
		stringFromAny(payload["remoteProjectId"]),
	)
	if err != nil {
		return map[string]any{"status": "failed", "message": err.Error()}
	}
	request["status"] = "ok"
	return request
}

func fallbackOpenRequestFromGlobalStateWithContext(state map[string]any, hostID, threadID, workspaceRoot, remoteProjectID string) (map[string]any, error) {
	hint := threadWorkspaceHint(state, threadID)
	selectedHostID := firstString(hostID, hostIDFromHint(hint), state["selected-remote-host-id"])
	hintedPath := firstString(workspaceRoot, workspacePathFromHint(hint), workspaceRootFromSQLite(threadID, ""))
	if strings.HasPrefix(hintedPath, "/") {
		resolvedHostID := hostIDForRemotePath(state, selectedHostID, hintedPath)
		return openRequestForRemotePath(state, resolvedHostID, hintedPath)
	}

	requestedProjectID := strings.TrimSpace(remoteProjectID)
	if requestedProjectID != "" {
		if strings.HasPrefix(requestedProjectID, "/") {
			return openRequestForRemotePath(state, selectedHostID, requestedProjectID)
		}
		for _, project := range orderedRemoteProjectsFromGlobalState(state) {
			if stringFromAny(project["id"]) != requestedProjectID {
				continue
			}
			projectHostID := stringFromAny(project["hostId"])
			if selectedHostID != "" && projectHostID != selectedHostID {
				continue
			}
			return openRequestForRemotePath(state, projectHostID, stringFromAny(project["remotePath"]))
		}
	}

	for _, project := range orderedRemoteProjectsFromGlobalState(state) {
		projectHostID := stringFromAny(project["hostId"])
		remotePath := stringFromAny(project["remotePath"])
		if (selectedHostID == "" || projectHostID == selectedHostID) && strings.HasPrefix(remotePath, "/") {
			return openRequestForRemotePath(state, firstString(selectedHostID, projectHostID), remotePath)
		}
	}
	return nil, zedRemoteError{"Cannot determine remote workspace or file for Zed"}
}

func workspaceRootFromSQLite(threadID, statePath string) string {
	threadID = normalizeThreadID(threadID)
	if threadID == "" {
		return ""
	}
	if statePath == "" {
		for _, path := range codexSessionDBPaths(codexHomeDir()) {
			if root := workspaceRootFromSQLite(threadID, path); root != "" {
				return root
			}
		}
		return ""
	}
	if !fileExists(statePath) {
		return ""
	}
	db, err := sql.Open("sqlite", statePath)
	if err != nil {
		return ""
	}
	defer db.Close()
	var cwd string
	if err := db.QueryRow("SELECT cwd FROM threads WHERE id = ?1 LIMIT 1", threadID).Scan(&cwd); err != nil {
		return ""
	}
	return strings.TrimSpace(cwd)
}

func orderedRemoteProjectsFromGlobalState(state map[string]any) []map[string]any {
	rawProjects, _ := state["remote-projects"].([]any)
	var projects []map[string]any
	for _, item := range rawProjects {
		project, _ := item.(map[string]any)
		if project != nil {
			projects = append(projects, project)
		}
	}
	rawOrder, _ := state["project-order"].([]any)
	var ordered []map[string]any
	used := map[string]bool{}
	for _, item := range rawOrder {
		id := stringFromAny(item)
		for _, project := range projects {
			if stringFromAny(project["id"]) == id {
				ordered = append(ordered, project)
				used[id] = true
				break
			}
		}
	}
	for _, project := range projects {
		id := stringFromAny(project["id"])
		if !used[id] {
			ordered = append(ordered, project)
		}
	}
	return ordered
}

func workspacePathFromHint(hint any) string {
	switch typed := hint.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		return firstString(typed["remotePath"], typed["remoteWorkspaceRoot"], typed["workspaceRoot"], typed["path"], typed["cwd"])
	default:
		return ""
	}
}

func normalizeThreadID(threadID string) string {
	return strings.TrimPrefix(strings.TrimSpace(threadID), "local:")
}

func hostIDFromHint(hint any) string {
	object, _ := hint.(map[string]any)
	if object == nil {
		return ""
	}
	return firstString(object["hostId"], object["remoteHostId"])
}

func threadWorkspaceHint(state map[string]any, threadID string) any {
	if strings.TrimSpace(threadID) == "" {
		return nil
	}
	hints, _ := state["thread-workspace-root-hints"].(map[string]any)
	if hints == nil {
		return nil
	}
	bare := normalizeThreadID(threadID)
	for _, key := range []string{threadID, bare, "local:" + bare} {
		if value, ok := hints[key]; ok {
			return value
		}
	}
	return nil
}

func hostIDForRemotePath(state map[string]any, preferredHostID, remotePath string) string {
	if strings.TrimSpace(preferredHostID) != "" {
		return strings.TrimSpace(preferredHostID)
	}
	for _, project := range orderedRemoteProjectsFromGlobalState(state) {
		projectPath := strings.TrimRight(stringFromAny(project["remotePath"]), "/")
		if projectPath != "" && (remotePath == projectPath || strings.HasPrefix(remotePath, projectPath+"/")) {
			return stringFromAny(project["hostId"])
		}
	}
	return ""
}

func openRequestForRemotePath(state map[string]any, hostID, remotePath string) (map[string]any, error) {
	if !strings.HasPrefix(remotePath, "/") {
		return nil, zedRemoteError{"Cannot determine remote workspace or file for Zed"}
	}
	if strings.TrimSpace(hostID) == "" {
		return nil, zedRemoteError{"Remote host id is required"}
	}
	target, err := resolveSSHTargetFromGlobalState(state, hostID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"hostId": hostID,
		"ssh": map[string]any{
			"user": target.User,
			"host": target.Host,
			"port": target.Port,
		},
		"path": remotePath,
	}, nil
}

func zedRegistryPath() string {
	return filepath.Join(stateDir(), "zed_remote_projects.json")
}

func listZedRemoteProjects(payload map[string]any, registryPath, sqlitePath string) ([]zedRemoteProject, error) {
	var projects []zedRemoteProject
	state, stateErr := readCodexGlobalStateMap()
	if stateErr == nil {
		collectCurrentZedProject(state, payload, &projects)
		collectCodexRemoteProjects(state, &projects)
		collectThreadWorkspaceHintProjects(state, &projects)
		if sqlitePath != "" {
			collectSQLiteThreadCWDProjects(state, sqlitePath, &projects)
		} else {
			for _, path := range codexSessionDBPaths(codexHomeDir()) {
				collectSQLiteThreadCWDProjects(state, path, &projects)
			}
		}
	} else if !os.IsNotExist(stateErr) {
		return nil, stateErr
	}
	recent, err := readZedRegistryProjects(registryPath)
	if err != nil {
		return nil, err
	}
	for _, project := range recent {
		project.Source = "recent"
		project.IsCurrent = false
		pushZedProject(&projects, project)
	}
	sort.SliceStable(projects, func(i, j int) bool {
		if projects[i].IsCurrent != projects[j].IsCurrent {
			return projects[i].IsCurrent
		}
		pi, pj := zedProjectSourcePriority(projects[i].Source), zedProjectSourcePriority(projects[j].Source)
		if pi != pj {
			return pi < pj
		}
		li, lj := int64(0), int64(0)
		if projects[i].LastOpenedAtMS != nil {
			li = *projects[i].LastOpenedAtMS
		}
		if projects[j].LastOpenedAtMS != nil {
			lj = *projects[j].LastOpenedAtMS
		}
		if li != lj {
			return li > lj
		}
		return projects[i].Label < projects[j].Label
	})
	return projects, nil
}

func readCodexGlobalStateMap() (map[string]any, error) {
	data, err := os.ReadFile(codexGlobalStatePath(codexHomeDir()))
	if err != nil {
		return nil, err
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("Cannot parse Codex remote connection state: %w", err)
	}
	return state, nil
}

func collectCurrentZedProject(state map[string]any, payload map[string]any, projects *[]zedRemoteProject) {
	hostID := stringFromAny(payload["hostId"])
	threadID := firstString(payload["threadId"], payload["sessionId"], payload["session_id"])
	workspaceRoot := firstString(payload["remoteWorkspaceRoot"], payload["workspaceRoot"], payload["cwd"], payload["path"])
	remoteProjectID := firstString(payload["remoteProjectId"], payload["projectId"])
	if hostID == "" && threadID == "" && workspaceRoot == "" && remoteProjectID == "" && stringFromAny(state["selected-remote-host-id"]) == "" {
		return
	}
	request, err := fallbackOpenRequestFromGlobalStateWithContext(state, hostID, threadID, workspaceRoot, remoteProjectID)
	if err != nil {
		return
	}
	project, err := zedProjectFromPayload(request, "currentThread", true, nil)
	if err == nil {
		pushZedProject(projects, project)
	}
}

func collectCodexRemoteProjects(state map[string]any, projects *[]zedRemoteProject) {
	for _, item := range orderedRemoteProjectsFromGlobalState(state) {
		hostID := stringFromAny(item["hostId"])
		remotePath := stringFromAny(item["remotePath"])
		if hostID == "" || !strings.HasPrefix(remotePath, "/") {
			continue
		}
		target, err := resolveSSHTargetFromGlobalState(state, hostID)
		if err != nil {
			continue
		}
		url, err := buildZedRemoteURL(target, remotePath)
		if err != nil {
			continue
		}
		project := zedProjectFromParts(hostID, target, remotePath, url, firstString(item["label"], item["name"]), "codexRemoteProject", nil, false)
		pushZedProject(projects, project)
	}
}

func collectThreadWorkspaceHintProjects(state map[string]any, projects *[]zedRemoteProject) {
	hints, _ := state["thread-workspace-root-hints"].(map[string]any)
	for _, hint := range hints {
		remotePath := workspacePathFromHint(hint)
		if !strings.HasPrefix(remotePath, "/") {
			continue
		}
		hostID := hostIDForRemotePath(state, hostIDFromHint(hint), remotePath)
		if hostID == "" {
			continue
		}
		target, err := resolveSSHTargetFromGlobalState(state, hostID)
		if err != nil {
			continue
		}
		url, err := buildZedRemoteURL(target, remotePath)
		if err != nil {
			continue
		}
		project := zedProjectFromParts(hostID, target, remotePath, url, "", "threadWorkspaceHint", nil, false)
		pushZedProject(projects, project)
	}
}

func collectSQLiteThreadCWDProjects(state map[string]any, sqlitePath string, projects *[]zedRemoteProject) {
	for _, cwd := range sqliteThreadCWDs(sqlitePath) {
		if !strings.HasPrefix(cwd, "/") {
			continue
		}
		hostID := hostIDForRemotePath(state, "", cwd)
		if hostID == "" {
			continue
		}
		target, err := resolveSSHTargetFromGlobalState(state, hostID)
		if err != nil {
			continue
		}
		url, err := buildZedRemoteURL(target, cwd)
		if err != nil {
			continue
		}
		project := zedProjectFromParts(hostID, target, cwd, url, "", "sqliteThreadCwd", nil, false)
		pushZedProject(projects, project)
	}
}

func sqliteThreadCWDs(path string) []string {
	if path == "" || !fileExists(path) {
		return nil
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query("SELECT DISTINCT cwd FROM threads WHERE cwd IS NOT NULL AND cwd != '' LIMIT 80")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cwd string
		if rows.Scan(&cwd) == nil && strings.TrimSpace(cwd) != "" {
			out = append(out, strings.TrimSpace(cwd))
		}
	}
	return out
}

func zedProjectFromPayload(payload map[string]any, source string, current bool, lastOpenedAtMS *int64) (zedRemoteProject, error) {
	target, err := sshTargetFromPayload(payload)
	if err != nil {
		return zedRemoteProject{}, err
	}
	path := stringFromAny(payload["path"])
	url := stringFromAny(payload["url"])
	if url == "" {
		url, err = buildZedRemoteURL(target, path)
		if err != nil {
			return zedRemoteProject{}, err
		}
	}
	return zedProjectFromParts(stringFromAny(payload["hostId"]), target, path, url, stringFromAny(payload["label"]), source, lastOpenedAtMS, current), nil
}

func zedProjectFromParts(hostID string, target sshTarget, path, url, label, source string, lastOpenedAtMS *int64, current bool) zedRemoteProject {
	path = strings.TrimSpace(path)
	if strings.TrimSpace(label) == "" {
		label = zedLabelFromPath(path)
	}
	return zedRemoteProject{
		ID:             zedProjectID(target, path),
		Label:          strings.TrimSpace(label),
		HostID:         strings.TrimSpace(hostID),
		SSH:            target,
		Path:           path,
		URL:            url,
		Source:         source,
		LastOpenedAtMS: lastOpenedAtMS,
		IsCurrent:      current,
	}
}

func rememberZedRemoteProject(payload map[string]any, resolvedTarget *sshTarget, resolvedURL string) (zedRemoteProject, error) {
	target := sshTarget{}
	var err error
	if resolvedTarget != nil {
		target = *resolvedTarget
	} else {
		target, err = sshTargetFromPayload(payload)
		if err != nil {
			return zedRemoteProject{}, err
		}
	}
	path := stringFromAny(payload["path"])
	url := resolvedURL
	if url == "" {
		url, err = buildZedRemoteURL(target, path)
		if err != nil {
			return zedRemoteProject{}, err
		}
	}
	now := time.Now().UnixMilli()
	project := zedProjectFromParts(stringFromAny(payload["hostId"]), target, path, url, stringFromAny(payload["label"]), "recent", &now, false)
	registryPath := zedRegistryPath()
	projects, err := readZedRegistryProjects(registryPath)
	if err != nil {
		return zedRemoteProject{}, err
	}
	pushZedProject(&projects, project)
	sort.SliceStable(projects, func(i, j int) bool {
		li, lj := int64(0), int64(0)
		if projects[i].LastOpenedAtMS != nil {
			li = *projects[i].LastOpenedAtMS
		}
		if projects[j].LastOpenedAtMS != nil {
			lj = *projects[j].LastOpenedAtMS
		}
		return li > lj
	})
	if len(projects) > 100 {
		projects = projects[:100]
	}
	if err := writeZedRegistryProjects(registryPath, projects); err != nil {
		return zedRemoteProject{}, err
	}
	return project, nil
}

func forgetZedRemoteProject(payload map[string]any) (int, error) {
	id := stringFromAny(payload["id"])
	if id == "" {
		target, err := sshTargetFromPayload(payload)
		if err != nil {
			return 0, err
		}
		id = zedProjectID(target, stringFromAny(payload["path"]))
	}
	path := zedRegistryPath()
	projects, err := readZedRegistryProjects(path)
	if err != nil {
		return 0, err
	}
	before := len(projects)
	filtered := projects[:0]
	for _, project := range projects {
		if project.ID != id {
			filtered = append(filtered, project)
		}
	}
	if err := writeZedRegistryProjects(path, filtered); err != nil {
		return 0, err
	}
	return before - len(filtered), nil
}

func readZedRegistryProjects(path string) ([]zedRemoteProject, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("Cannot read ChatGPT Codex Tools Zed remote project registry: %w", err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return nil, nil
	}
	var registry zedRemoteProjectRegistry
	if err := json.Unmarshal(data, &registry); err == nil {
		return registry.Projects, nil
	}
	var projects []zedRemoteProject
	if err := json.Unmarshal(data, &projects); err != nil {
		return nil, fmt.Errorf("Cannot parse ChatGPT Codex Tools Zed remote project registry: %w", err)
	}
	return projects, nil
}

func writeZedRegistryProjects(path string, projects []zedRemoteProject) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("Cannot write ChatGPT Codex Tools Zed remote project registry: %w", err)
	}
	data, err := json.MarshalIndent(zedRemoteProjectRegistry{Projects: projects}, "", "  ")
	if err != nil {
		return fmt.Errorf("Cannot write ChatGPT Codex Tools Zed remote project registry: %w", err)
	}
	if err := atomicWrite(path, data); err != nil {
		return fmt.Errorf("Cannot write ChatGPT Codex Tools Zed remote project registry: %w", err)
	}
	return nil
}

func pushZedProject(projects *[]zedRemoteProject, project zedRemoteProject) {
	if project.ID == "" || project.Path == "" || project.URL == "" {
		return
	}
	for index := range *projects {
		existing := &(*projects)[index]
		if existing.ID != project.ID {
			continue
		}
		if zedProjectSourcePriority(project.Source) < zedProjectSourcePriority(existing.Source) {
			existing.Source = project.Source
			existing.Label = project.Label
			existing.HostID = project.HostID
		}
		if existing.LastOpenedAtMS == nil || (project.LastOpenedAtMS != nil && *project.LastOpenedAtMS > *existing.LastOpenedAtMS) {
			existing.LastOpenedAtMS = project.LastOpenedAtMS
		}
		existing.IsCurrent = existing.IsCurrent || project.IsCurrent
		return
	}
	*projects = append(*projects, project)
}

func zedProjectSourcePriority(source string) int {
	switch source {
	case "currentThread":
		return 0
	case "codexRemoteProject":
		return 1
	case "threadWorkspaceHint":
		return 2
	case "sqliteThreadCwd":
		return 3
	case "recent":
		return 4
	default:
		return 9
	}
}

func zedProjectID(target sshTarget, path string) string {
	port := ""
	if target.Port != nil {
		port = strconv.Itoa(int(*target.Port))
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(strings.TrimSpace(target.User) + "|" + strings.TrimSpace(target.Host) + "|" + port + "|" + strings.TrimSpace(path)))
	return fmt.Sprintf("zed-remote-project:%016x", hash.Sum64())
}

func zedLabelFromPath(path string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(path), "/")
	if trimmed == "" {
		return path
	}
	base := filepath.Base(trimmed)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return trimmed
	}
	return base
}

func allASCII(value string, pred func(byte) bool) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if !pred(value[i]) {
			return false
		}
	}
	return true
}
