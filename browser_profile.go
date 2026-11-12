package browserkit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/go-rod/rod/lib/launcher"
)

// localBrowserProfile 记录本地 Chromium 浏览器的身份和独立存储目录。
// 默认 UA 与实际浏览器品牌和主版本匹配；只有调用方显式设置时才覆盖该选择。
type localBrowserProfile struct {
	dir       string
	userAgent string
	cleanup   bool
	owned     bool
}

var browserVersionPattern = regexp.MustCompile(`(?i)^(Google Chrome(?: for Testing)?|Chromium|Chrome for Testing|Microsoft Edge(?: Beta| Dev| Canary)?)\s+([1-9][0-9]*)(?:\.[0-9]+){1,3}(?:\s.*)?$`)

func (profile *localBrowserProfile) useInstalledBrowserIdentity(ctx context.Context, binary string) {
	if profile.userAgent != "" {
		return
	}
	probeTimeout := 2 * time.Second
	if runtime.GOOS == "windows" {
		probeTimeout = 5 * time.Second
	}
	versionCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	command := browserVersionCommand(versionCtx, binary, runtime.GOOS)
	output, err := command.Output()
	if err != nil {
		return
	}
	profile.userAgent = nativeBrowserUserAgent(string(output), runtime.GOOS)
}

func browserVersionCommand(ctx context.Context, binary, goos string) *exec.Cmd {
	if goos != "windows" {
		return exec.CommandContext(ctx, binary, "--version")
	}
	// Windows 的 GUI 版浏览器不能可靠地通过 --version 输出版本，甚至会打开用户浏览器。
	// 读取实际可执行文件的品牌和版本，不把 Edge 的裸版本号当作 Chrome。
	if resolved, err := exec.LookPath(binary); err == nil {
		binary = resolved
	}
	if absolute, err := filepath.Abs(binary); err == nil {
		binary = absolute
	}
	command := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		`$ErrorActionPreference = 'Stop'; $version = [System.Diagnostics.FileVersionInfo]::GetVersionInfo($env:BROWSERKIT_BROWSER_BINARY); '{0} {1}' -f $version.ProductName, $version.ProductVersion`)
	// 路径只作为数据传递，空格、单引号和 PowerShell 表达式都不进入脚本。
	command.Env = append(os.Environ(), "BROWSERKIT_BROWSER_BINARY="+binary)
	return command
}

func nativeBrowserUserAgent(version, goos string) string {
	match := browserVersionPattern.FindStringSubmatch(strings.TrimSpace(version))
	if len(match) != 3 {
		return ""
	}
	var platform string
	switch goos {
	case "darwin":
		platform = "Macintosh; Intel Mac OS X 10_15_7"
	case "windows":
		platform = "Windows NT 10.0; Win64; x64"
	case "linux":
		platform = "X11; Linux x86_64"
	default:
		return ""
	}
	ua := fmt.Sprintf("Mozilla/5.0 (%s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s.0.0.0 Safari/537.36", platform, match[2])
	if strings.HasPrefix(strings.ToLower(match[1]), "microsoft edge") {
		ua += " Edg/" + match[2] + ".0.0.0"
	}
	return ua
}

func newLocalBrowserProfile(config SessionConfig) (localBrowserProfile, error) {
	profile := localBrowserProfile{dir: config.UserDataDir, userAgent: config.UserAgent, cleanup: config.CleanupUserDataDir && config.UserDataDir != ""}
	if profile.dir != "" {
		return profile, nil
	}
	dir, err := os.MkdirTemp("", "browserkit-profile-")
	if err != nil {
		return localBrowserProfile{}, fmt.Errorf("创建浏览器临时 profile: %w", err)
	}
	profile.dir = dir
	profile.cleanup = true
	profile.owned = true
	return profile, nil
}

func (profile localBrowserProfile) configureLauncher(launch *launcher.Launcher) {
	launch.UserDataDir(profile.dir)
	// Rod 默认加的自动化标记和关闭站点隔离参数会改变 Chrome 的常规行为。
	launch.Delete("enable-automation").Delete("disable-site-isolation-trials").Delete("disable-features")
	launch.Set("disable-blink-features", "AutomationControlled")
	if profile.userAgent != "" {
		launch.Set("user-agent", profile.userAgent)
	}
}

func (profile localBrowserProfile) cleanupFailedLaunch(launch *launcher.Launcher) {
	if !profile.owned {
		return
	}
	if launch != nil && launch.PID() != 0 {
		launch.Cleanup()
		return
	}
	_ = os.RemoveAll(profile.dir)
}
