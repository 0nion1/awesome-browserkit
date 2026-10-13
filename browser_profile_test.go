package browserkit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
)

func TestLocalBrowserProfileOwnsTemporaryDirectoryAndKeepsNativeIdentity(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	first, err := newLocalBrowserProfile(SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(first.dir)
	second, err := newLocalBrowserProfile(SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(second.dir)
	if !first.owned || !first.cleanup || first.dir == second.dir {
		t.Fatalf("临时 profile 没有隔离：first=%+v second=%+v", first, second)
	}
	launch := launcher.New()
	first.configureLauncher(launch)
	if launch.Get("user-data-dir") != first.dir || launch.Has("user-agent") || launch.Has("enable-automation") || launch.Has("disable-site-isolation-trials") || launch.Has("disable-features") || launch.Get("disable-blink-features") != "AutomationControlled" {
		t.Fatalf("本地 Chrome 参数不符合 profile 配置：%v", launch.FormatArgs())
	}
}

func TestLocalBrowserProfilePreservesExplicitDirectoryAndIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "existing-profile")
	profile, err := newLocalBrowserProfile(SessionConfig{UserDataDir: dir, UserAgent: "BrowserKit-Test-Agent"})
	if err != nil {
		t.Fatal(err)
	}
	if profile.owned || profile.cleanup {
		t.Fatalf("显式 profile 不应被隐式清理：%+v", profile)
	}
	launch := launcher.New()
	profile.configureLauncher(launch)
	if launch.Get("user-data-dir") != dir || launch.Get("user-agent") != "BrowserKit-Test-Agent" {
		t.Fatalf("显式 profile 配置丢失：%v", launch.FormatArgs())
	}
	profile, err = newLocalBrowserProfile(SessionConfig{UserDataDir: dir, CleanupUserDataDir: true})
	if err != nil || !profile.cleanup || profile.owned {
		t.Fatalf("显式清理选项未保留：profile=%+v err=%v", profile, err)
	}
}

func TestNativeChromeUserAgentUsesInstalledMajorVersion(t *testing.T) {
	ua := nativeChromeUserAgent("Google Chrome 155.0.8059.39")
	if !strings.Contains(ua, "Chrome/155.0.0.0") || strings.Contains(ua, "HeadlessChrome") {
		t.Fatalf("Chrome 版本未转换为正常 UA：%q", ua)
	}
	if nativeChromeUserAgent("unexpected browser") != "" {
		t.Fatal("未知版本不应猜测 UA")
	}
}

func TestFailedLocalLaunchRemovesOwnedProfile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := NewSession(ctx, SessionConfig{ExecPath: filepath.Join(root, "missing-chrome")}); err == nil {
		t.Fatal("无效 Chrome 路径应启动失败")
	}
	profiles, err := filepath.Glob(filepath.Join(root, "browserkit-profile-*"))
	if err != nil || len(profiles) != 0 {
		t.Fatalf("启动失败后临时 profile 未清理：%v, %v", profiles, err)
	}
}
