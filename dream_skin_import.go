package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	dreamSkinSourceImageLimit   = int64(50 << 20)
	dreamSkinPreparedImageLimit = int64(16 << 20)
)

func (s *server) importDreamSkinImage(ctx context.Context, args map[string]any) commandResult {
	path := strings.TrimSpace(firstNonEmpty(stringArg(args, "path"), stringArg(mapArg(args, "request"), "path")))
	managedPath, err := importDreamSkinImage(ctx, path, stateDir())
	if err != nil {
		return failed("导入 DreamSkin 图片失败："+err.Error(), settingsPayloadValue(loadSettings()))
	}
	settings := loadSettings()
	settings.CodexAppDreamSkinEnabled = true
	settings.CodexAppDreamSkinPaused = false
	settings.CodexAppDreamSkinImagePath = managedPath
	if err := saveSettings(settings); err != nil {
		return failed("保存 DreamSkin 图片设置失败："+err.Error(), settingsPayloadValue(loadSettings()))
	}
	payload := settingsPayloadValue(loadSettings())
	payload["managedPath"] = managedPath
	return ok("DreamSkin 背景图片已导入到本地托管目录。", payload)
}

func importDreamSkinImage(ctx context.Context, source, appStateDir string) (string, error) {
	source = filepath.Clean(strings.TrimSpace(source))
	if source == "." || source == "" {
		return "", fmt.Errorf("未选择图片")
	}
	info, err := os.Lstat(source)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("图片必须是普通文件，不能使用符号链接")
	}
	if info.Size() <= 0 {
		return "", fmt.Errorf("图片文件为空")
	}
	if info.Size() > dreamSkinSourceImageLimit {
		return "", fmt.Errorf("原图不能超过 50 MiB")
	}
	extension := strings.ToLower(strings.TrimPrefix(filepath.Ext(source), "."))
	if !supportedDreamSkinImageExtension(extension) {
		return "", fmt.Errorf("不支持的图片格式：%s", extension)
	}
	managedDir := filepath.Join(appStateDir, "dream-skin", "theme")
	if err := os.MkdirAll(managedDir, 0o755); err != nil {
		return "", err
	}
	preparedPath := source
	preparedExtension := extension
	if runtime.GOOS == "darwin" {
		preparedPath = filepath.Join(managedDir, ".dream-skin-import.jpg")
		_ = os.Remove(preparedPath)
		convertCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		command := exec.CommandContext(convertCtx, "/usr/bin/sips", "-s", "format", "jpeg", source, "--out", preparedPath)
		hideSubprocessWindow(command)
		output, commandErr := command.CombinedOutput()
		if commandErr != nil {
			return "", fmt.Errorf("macOS 图片转换失败：%s", strings.TrimSpace(string(output)))
		}
		defer os.Remove(preparedPath)
		preparedExtension = "jpg"
	}
	preparedInfo, err := os.Stat(preparedPath)
	if err != nil {
		return "", err
	}
	if preparedInfo.Size() <= 0 || preparedInfo.Size() > dreamSkinPreparedImageLimit {
		return "", fmt.Errorf("处理后的图片必须介于 1 字节和 16 MiB 之间")
	}
	contents, err := os.ReadFile(preparedPath)
	if err != nil {
		return "", err
	}
	destination := filepath.Join(managedDir, "current."+preparedExtension)
	if err := atomicWrite(destination, contents); err != nil {
		return "", err
	}
	entries, _ := os.ReadDir(managedDir)
	for _, entry := range entries {
		candidate := filepath.Join(managedDir, entry.Name())
		if candidate != destination && !entry.IsDir() && strings.HasPrefix(entry.Name(), "current.") {
			_ = os.Remove(candidate)
		}
	}
	return destination, nil
}

func supportedDreamSkinImageExtension(extension string) bool {
	if runtime.GOOS == "darwin" {
		switch extension {
		case "png", "jpg", "jpeg", "heic", "tif", "tiff", "webp":
			return true
		}
		return false
	}
	switch extension {
	case "png", "jpg", "jpeg", "webp", "gif", "bmp":
		return true
	}
	return false
}
