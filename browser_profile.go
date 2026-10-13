package browserkit

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"time"

	"github.com/go-rod/rod/lib/launcher"
)

// localBrowserProfile 记录本地 Chrome 会话的身份和独立存储目录。
// 默认 UA 与实际 Chrome 主版本匹配；只有调用方显式设置时才覆盖该选择。
type localBrowserProfile struct {
	dir       string
	userAgent string
	cleanup   bool
	owned     bool
}

var chromeVersionPattern = regexp.MustCompile(`(?i)(?:Google Chrome|Chromium|Chrome for Testing)\s+([0-9]+)(?:\.[0-9]+){1,3}\b`)

func (profile *localBrowserProfile) useInstalledBrowserIdentity(ctx context.Context, binary string) {
	if profile.userAgent != "" {
		return
	}
	versionCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(versionCtx, binary, "--version").Output()
	if err != nil {
		return
	}
	profile.userAgent = nativeChromeUserAgent(string(output))
}

func nativeChromeUserAgent(version string) string {
	match := chromeVersionPattern.FindStringSubmatch(version)
	if len(match) != 2 {
		return ""
	}
	var platform string
	switch runtime.GOOS {
	case "darwin":
		platform = "Macintosh; Intel Mac OS X 10_15_7"
	case "windows":
		platform = "Windows NT 10.0; Win64; x64"
	case "linux":
		platform = "X11; Linux x86_64"
	default:
		return ""
	}
	return fmt.Sprintf("Mozilla/5.0 (%s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s.0.0.0 Safari/537.36", platform, match[1])
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
