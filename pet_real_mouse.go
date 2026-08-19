package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const petV2SpriteDetectionScript = `
  const isV2Sprite = async (mascot) => {
    if (!mascot) return false;
    if (Array.from(mascot.querySelectorAll("img")).some((image) =>
      image.naturalWidth === 1536 && image.naturalHeight === 2288
    )) return true;
    for (const element of [mascot, ...mascot.querySelectorAll("*")]) {
      const background = getComputedStyle(element).backgroundImage || "";
      const match = background.match(/url\(["']?([^"')]+)/i);
      if (!match) continue;
      const source = match[1];
      const cacheKey = "__codexPlusPetV2SpriteProbe";
      let probe = window[cacheKey];
      if (!probe || probe.source !== source) {
        probe = { source, valid: false, pending: true };
        probe.promise = (async () => {
          try {
            const image = new Image();
            image.src = source;
            await image.decode();
            return image.naturalWidth === 1536 && image.naturalHeight === 2288;
          } catch {
            return false;
          }
        })().then((valid) => {
          probe.valid = valid;
          probe.pending = false;
          return valid;
        });
        window[cacheKey] = probe;
      }
      const wasPending = probe.pending;
      const valid = wasPending ? await probe.promise : probe.valid;
      if (wasPending) {
        const currentBackground = getComputedStyle(element).backgroundImage || "";
        const currentMatch = currentBackground.match(/url\(["']?([^"')]+)/i);
        if (currentMatch?.[1] !== source) continue;
      }
      if (window[cacheKey] === probe && valid) return true;
    }
    return false;
  };
`

var (
	petOverlaySyncFailed  atomic.Bool
	petCursorDriverFailed atomic.Bool
)

func petRealMouseCapabilityProbeScript() string {
	return `(async () => {
  const mascot = document.querySelector('[data-avatar-mascot="true"]');
` + petV2SpriteDetectionScript + `
  if (!await isV2Sprite(mascot)) return false;
  const urls = [
    ...Array.from(document.scripts || []).map((script) => script.src),
    ...Array.from(document.querySelectorAll("link[href]") || []).map((link) => link.href),
    ...performance.getEntriesByType("resource").map((entry) => entry.name),
  ].filter((url) => url && url.includes("/assets/") && url.split("?")[0].endsWith(".js"));
  let dispatcherUrl = urls.find((url) => url.includes("vscode-api-"));
  if (!dispatcherUrl) {
    for (const url of urls) {
      try {
        const source = await fetch(url).then((response) => response.ok ? response.text() : "");
        const match = source.match(/["'](\.\/(?:assets\/)?vscode-api-[^"']+\.js)["']/);
        if (match) {
          dispatcherUrl = new URL(match[1], url).href;
          break;
        }
      } catch {
      }
    }
  }
  if (!dispatcherUrl) return false;
  try {
    const module = await import(dispatcherUrl);
    return Object.values(module || {}).some((value) => value
      && typeof value.dispatchHostMessage === "function"
      && typeof value.subscribe === "function");
  } catch {
    return false;
  }
})()`
}

func petRealMouseUpdateScript(x, y int32) string {
	return fmt.Sprintf(`(async () => {
  const mascot = document.querySelector('[data-avatar-mascot="true"]');
%s
  return await isV2Sprite(mascot)
    && window.__codexPlusPetRealMouseLook?.updateScreenPoint?.({ x: %d, y: %d }) === true;
})()`, petV2SpriteDetectionScript, x, y)
}

func petRealMouseStopScript() string {
	return `window.__codexPlusPetRealMouseLook?.stop?.();`
}

func evaluateCDPTarget(ctx context.Context, websocketURL, script string, awaitPromise bool) (bool, error) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, websocketURL, nil)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	session := newCDPSession(conn, nil)
	result, err := session.send(ctx, "Runtime.evaluate", runtimeEvaluateParams(script, awaitPromise))
	if err != nil {
		return false, err
	}
	return cdpResultBool(result), nil
}

func petOverlaySupportsV2Cursor(target cdpTarget) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cdpCommandTimeout)
	defer cancel()
	return evaluateCDPTarget(ctx, target.WebSocketDebuggerURL, petRealMouseCapabilityProbeScript(), true)
}

func (r *launcherRuntime) syncPetRealMouseOverlays() error {
	targetCtx, cancel := context.WithTimeout(context.Background(), cdpConnectTimeout)
	defer cancel()
	targets, err := listCDPTargets(targetCtx, r.debugPort)
	if err != nil {
		return err
	}
	settings := r.runtimeSettingsSnapshot()
	enabled := settings.Enhancements && settings.CodexAppPetRealMouseLook
	for _, target := range targets {
		if !isAvatarOverlayCDPPageTarget(target) {
			continue
		}
		script := petRealMouseStopScript()
		if enabled {
			supported, probeErr := petOverlaySupportsV2Cursor(target)
			if probeErr != nil {
				return fmt.Errorf("probe pet overlay %s: %w", target.ID, probeErr)
			}
			if supported {
				script = petRealMouseInjectScript
			}
		}
		ctx, stop := context.WithTimeout(context.Background(), cdpCommandTimeout)
		_, evalErr := evaluateCDPTarget(ctx, target.WebSocketDebuggerURL, script, false)
		stop()
		if evalErr != nil {
			return fmt.Errorf("sync pet overlay %s: %w", target.ID, evalErr)
		}
	}
	return nil
}

func (r *launcherRuntime) confirmedPetOverlayTargets() []cdpTarget {
	ctx, cancel := context.WithTimeout(context.Background(), cdpConnectTimeout)
	defer cancel()
	targets, err := listCDPTargets(ctx, r.debugPort)
	if err != nil {
		return nil
	}
	confirmed := make([]cdpTarget, 0, len(targets))
	for _, target := range targets {
		if !isAvatarOverlayCDPPageTarget(target) {
			continue
		}
		supported, probeErr := petOverlaySupportsV2Cursor(target)
		if probeErr == nil && supported {
			confirmed = append(confirmed, target)
		}
	}
	return confirmed
}

func (r *launcherRuntime) petRealMouseCursorDriver() {
	for {
		settings := r.runtimeSettingsSnapshot()
		if !settings.Enhancements || !settings.CodexAppPetRealMouseLook {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		targets := r.confirmedPetOverlayTargets()
		if len(targets) == 0 {
			time.Sleep(5 * time.Second)
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, len(targets))
		for _, target := range targets {
			target := target
			go func() { done <- r.runPetRealMouseTargetDriver(ctx, target) }()
		}
		err := <-done
		cancel()
		if err != nil && !strings.Contains(err.Error(), "disabled") && !petCursorDriverFailed.Swap(true) {
			appendDiagnosticLog("pet.real_mouse_cursor_driver_disconnected", map[string]any{"debug_port": r.debugPort, "error": err.Error()})
		}
		time.Sleep(5 * time.Second)
	}
}

func (r *launcherRuntime) runPetRealMouseTargetDriver(parent context.Context, target cdpTarget) error {
	ctx, cancel := context.WithTimeout(parent, cdpConnectTimeout)
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, target.WebSocketDebuggerURL, nil)
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close()
	session := newCDPSession(conn, nil)
	if err := sendPetRuntimeEvaluation(parent, session, petRealMouseInjectScript, false); err != nil {
		return err
	}
	defer func() { _ = sendPetRuntimeEvaluation(context.Background(), session, petRealMouseStopScript(), false) }()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	settingsCheck := 0
	for {
		select {
		case <-parent.Done():
			return nil
		case <-ticker.C:
			if settingsCheck == 0 {
				settings := r.runtimeSettingsSnapshot()
				if !settings.Enhancements || !settings.CodexAppPetRealMouseLook {
					return errors.New("pet real-mouse disabled")
				}
				settingsCheck = 10
			}
			settingsCheck--
			x, y, cursorErr := windowsLogicalCursorPosition()
			if cursorErr != nil {
				return cursorErr
			}
			if err := sendPetRuntimeEvaluation(parent, session, petRealMouseUpdateScript(x, y), true); err != nil {
				return err
			}
			petCursorDriverFailed.Store(false)
		}
	}
}

func sendPetRuntimeEvaluation(parent context.Context, session *cdpSession, script string, awaitPromise bool) error {
	ctx, cancel := context.WithTimeout(parent, cdpCommandTimeout)
	defer cancel()
	result, err := session.send(ctx, "Runtime.evaluate", runtimeEvaluateParams(script, awaitPromise))
	if err != nil {
		return err
	}
	if awaitPromise && !cdpResultBool(result) {
		return errors.New("pet overlay rejected cursor update")
	}
	return nil
}

func (r *launcherRuntime) recordPetRealMouseOverlaySync(err error) {
	if err == nil {
		if petOverlaySyncFailed.Swap(false) {
			appendDiagnosticLog("pet.real_mouse_overlay_sync_recovered", map[string]any{"debug_port": r.debugPort})
		}
		return
	}
	if !petOverlaySyncFailed.Swap(true) {
		appendDiagnosticLog("pet.real_mouse_overlay_sync_failed", map[string]any{"debug_port": r.debugPort, "error": err.Error()})
	}
}
