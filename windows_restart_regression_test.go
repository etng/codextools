package main

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseWindowsTCPListenerProcessIDsFiltersNonListeningStates(t *testing.T) {
	output := strings.Join([]string{
		"  Proto  Local Address          Foreign Address        State           PID",
		"  TCP    127.0.0.1:9229         0.0.0.0:0              LISTENING       111",
		"  TCP    127.0.0.1:9229         127.0.0.1:50000        ESTABLISHED     222",
		"  TCP    127.0.0.1:9229         127.0.0.1:50001        CLOSE_WAIT      333",
		"  TCP    127.0.0.1:9229         0.0.0.0:0              TIME_WAIT       0",
		"  TCP    127.0.0.1:9229         0.0.0.0:0              BOUND           777",
		"  TCP    [::1]:9229             [::]:0                 LISTENING       444",
		"  TCP    127.0.0.1:50000        127.0.0.1:9229         ESTABLISHED     555",
		"  TCP    0.0.0.0:9229           0.0.0.0:0              LISTENING       111",
		"  UDP    127.0.0.1:9229         *:*                                    666",
	}, "\r\n")

	got := parseWindowsTCPListenerProcessIDs(output, 9229)
	want := []uint32{111}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listener PID parsing mismatch: got %v want %v", got, want)
	}
	status := parseWindowsTCPPortProcessIDs(output, 9229)
	if !reflect.DeepEqual(status.Bound, []uint32{777}) {
		t.Fatalf("bound PID parsing mismatch: got %v", status.Bound)
	}
}

func TestDebugPortConsideredFreeUsesWindowsListenerState(t *testing.T) {
	tests := []struct {
		name  string
		goos  string
		state debugPortReleaseState
		want  bool
	}{
		{name: "bind succeeds", goos: "windows", state: debugPortReleaseState{}, want: true},
		{
			name: "Windows address-in-use without listener is still unavailable",
			goos: "windows",
			state: debugPortReleaseState{
				BindError:        syscall.EADDRINUSE,
				ListenerPIDKnown: true,
			},
		},
		{
			name: "Windows access denied is not free",
			goos: "windows",
			state: debugPortReleaseState{
				BindError:        syscall.EACCES,
				ListenerPIDKnown: true,
			},
		},
		{
			name: "Windows listener remains occupied",
			goos: "windows",
			state: debugPortReleaseState{
				BindError:        syscall.EADDRINUSE,
				Accepting:        true,
				ListenerPIDs:     []uint32{77},
				ListenerPIDKnown: true,
			},
		},
		{
			name: "unknown listener state is not free",
			goos: "windows",
			state: debugPortReleaseState{
				BindError: syscall.EADDRINUSE,
			},
		},
		{
			name: "Windows bind probe race does not ignore a new listener",
			goos: "windows",
			state: debugPortReleaseState{
				Accepting:        true,
				ListenerPIDs:     []uint32{88},
				ListenerPIDKnown: true,
			},
		},
		{
			name: "Windows bind probe race does not ignore a bound owner",
			goos: "windows",
			state: debugPortReleaseState{
				BoundPIDs:     []uint32{99},
				BoundPIDKnown: true,
			},
		},
		{
			name: "macOS still requires bind success",
			goos: "darwin",
			state: debugPortReleaseState{
				BindError:        syscall.EADDRINUSE,
				ListenerPIDKnown: true,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := debugPortConsideredFree(test.goos, test.state); got != test.want {
				t.Fatalf("debug-port free decision mismatch: got %v want %v for %#v", got, test.want, test.state)
			}
		})
	}
}

func TestStopWindowsTargetsBeforeRestartSurfacesCleanupResult(t *testing.T) {
	original := terminateWindowsRestartTargets
	t.Cleanup(func() { terminateWindowsRestartTargets = original })

	called := 0
	terminateWindowsRestartTargets = func(appPath string, debugPort uint16, timeout time.Duration) ([]uint32, error) {
		called++
		if appPath != `C:\Program Files\WindowsApps\OpenAI.Codex\app` {
			t.Fatalf("cleanup app path mismatch: %q", appPath)
		}
		if timeout != 8*time.Second {
			t.Fatalf("cleanup timeout mismatch: %s", timeout)
		}
		if debugPort != 9229 {
			t.Fatalf("cleanup debug port mismatch: %d", debugPort)
		}
		return []uint32{101, 202}, nil
	}
	if err := stopWindowsTargetsBeforeRestart(`C:\Program Files\WindowsApps\OpenAI.Codex\app`, 9229, 8*time.Second); err != nil {
		t.Fatalf("Windows target cleanup should succeed: %v", err)
	}
	if called != 1 {
		t.Fatalf("Windows target cleanup call count mismatch: %d", called)
	}

	cleanupErr := errors.New("process still running")
	terminateWindowsRestartTargets = func(string, uint16, time.Duration) ([]uint32, error) {
		return []uint32{303}, cleanupErr
	}
	err := stopWindowsTargetsBeforeRestart("aumid:OpenAI.Codex_test!App", 9229, time.Second)
	if err == nil || !strings.Contains(err.Error(), "关闭旧 Windows ChatGPT/Codex 进程失败") || !errors.Is(err, cleanupErr) {
		t.Fatalf("cleanup failure was not preserved: %v", err)
	}
}

func TestLauncherRestartLockCoalescesConcurrentRestart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	first, acquired, err := acquireLauncherRestartLock()
	if err != nil || !acquired || first == nil {
		t.Fatalf("first restart lock acquisition failed: acquired=%v err=%v", acquired, err)
	}
	defer first.release()

	second, acquired, err := acquireLauncherRestartLock()
	if err != nil || acquired || second != nil {
		t.Fatalf("concurrent restart should coalesce: lock=%#v acquired=%v err=%v", second, acquired, err)
	}
	if err := first.release(); err != nil {
		t.Fatalf("release first restart lock: %v", err)
	}
	first = nil

	third, acquired, err := acquireLauncherRestartLock()
	if err != nil || !acquired || third == nil {
		t.Fatalf("restart lock should be reusable: acquired=%v err=%v", acquired, err)
	}
	_ = third.release()
}

func TestInjectionRetryLoggingIsSampled(t *testing.T) {
	for attempt := 1; attempt <= 24; attempt++ {
		want := attempt == 1 || attempt == 24
		if got := shouldLogInjectionRetry("windows", attempt); got != want {
			t.Fatalf("retry logging mismatch at attempt %d: got %v want %v", attempt, got, want)
		}
		if !shouldLogInjectionRetry("darwin", attempt) {
			t.Fatalf("macOS retry logging changed at attempt %d", attempt)
		}
	}
}

func TestWindowsRestartTargetMatchingIsScoped(t *testing.T) {
	const family = "OpenAI.Codex_2p2nqsd0c76g0"
	if !windowsRestartTargetProcessMatches("ChatGPT.exe", family, "", family, "") {
		t.Fatal("matching official package target was rejected")
	}
	if windowsRestartTargetProcessMatches("ChatGPT.exe", "OpenAI.ChatGPT_2p2nqsd0c76g0", "", family, "") {
		t.Fatal("different official package family must not be terminated")
	}
	const packagedExecutable = `C:\Program Files\WindowsApps\OpenAI.Codex_1.0.0.0_x64__2p2nqsd0c76g0\app\ChatGPT.exe`
	if !windowsRestartTargetProcessMatches("ChatGPT.exe", "", packagedExecutable, family, packagedExecutable) {
		t.Fatal("registered FullTrust executable without package identity was rejected")
	}
	const executable = `C:\Users\tester\Apps\ChatGPT\ChatGPT.exe`
	if !windowsRestartTargetProcessMatches("ChatGPT.exe", "", `\\?\C:\Users\tester\Apps\ChatGPT\ChatGPT.exe`, "", executable) {
		t.Fatal("matching standalone executable was rejected")
	}
	if windowsRestartTargetProcessMatches("ChatGPT.exe", "", `C:\Other\ChatGPT.exe`, "", executable) {
		t.Fatal("same-name executable at another path must not be terminated")
	}
	if windowsRestartTargetProcessMatches("Codex.exe", "", `C:\Tools\codex.exe`, "", executable) {
		t.Fatal("standalone Codex CLI must not match a configured ChatGPT executable")
	}
	const codexDesktop = `C:\Users\tester\Apps\Codex\Codex.exe`
	if !windowsRestartTargetProcessMatches("Codex.exe", "", codexDesktop, "", codexDesktop) {
		t.Fatal("configured standalone Codex desktop executable was rejected")
	}
}

func TestWindowsRestartFlowKeepsMacOSBranchSeparate(t *testing.T) {
	source, err := os.ReadFile("launcher.go")
	if err != nil {
		t.Fatalf("read launcher.go: %v", err)
	}
	text := string(source)
	for _, expected := range []string{
		`windowsRestart := currentRuntimeGOOS() == "windows"`,
		`stopWindowsTargetsBeforeRestart(appPath, debugPort, 8*time.Second)`,
		`waitForRequiredDebugPortFree(debugPort, 12*time.Second)`,
		`runtime.GOOS == "darwin" && macOSAppRunning(appPath)`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("restart flow regression guard missing %q", expected)
		}
	}
}

func TestWindowsInstallerStopsRunningChatGPTBeforeUpgrade(t *testing.T) {
	script := readFile("scripts/build/windows-release.sh")
	if !strings.Contains(script, `taskkill /IM ChatGPT.exe /T /F`) {
		t.Fatal("Windows installer must stop the running ChatGPT desktop app before replacing the launcher")
	}
	if strings.Contains(script, `taskkill /IM Codex.exe /T /F`) {
		t.Fatal("Windows installer must not terminate unrelated Codex CLI processes")
	}
}
