package browserkit

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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
	defer func() { _ = os.RemoveAll(first.dir) }()
	second, err := newLocalBrowserProfile(SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(second.dir) }()
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
	for _, test := range []struct{ version, goos, platform string }{
		{"Google Chrome 155.0.8059.39", "darwin", "Macintosh; Intel Mac OS X 10_15_7"},
		{"Chromium 155.0.8059.39", "linux", "X11; Linux x86_64"},
		{"Chrome for Testing 155.0.8059.39", "windows", "Windows NT 10.0; Win64; x64"},
		{"Google Chrome for Testing 155.0.8059.39", "linux", "X11; Linux x86_64"},
		{"Google Chrome 155.0.8059.39\r\n", "windows", "Windows NT 10.0; Win64; x64"},
	} {
		t.Run(test.version+"/"+test.goos, func(t *testing.T) {
			ua := nativeBrowserUserAgent(test.version, test.goos)
			want := "Mozilla/5.0 (" + test.platform + ") AppleWebKit/537.36 (KHTML, like Gecko) Chrome/155.0.0.0 Safari/537.36"
			if ua != want {
				t.Fatalf("Chrome 版本或平台未转换为正常 UA：got=%q want=%q", ua, want)
			}
		})
	}
	for _, version := range []string{"", "unexpected browser", "155", "155.0.invalid", "0.0.0.0", "155.0.8059.39"} {
		if nativeBrowserUserAgent(version, "windows") != "" {
			t.Fatalf("未知版本不应猜测 UA：%q", version)
		}
	}
}

func TestNativeEdgeUserAgentPreservesBrand(t *testing.T) {
	for _, version := range []string{"Microsoft Edge 157.0.4341.42\r\n", "Microsoft Edge Dev 157.0.4341.42", "Microsoft Edge Beta 157.0.4341.42", "Microsoft Edge Canary 157.0.4341.42"} {
		for _, goos := range []string{"windows", "darwin", "linux"} {
			ua := nativeBrowserUserAgent(version, goos)
			if !strings.Contains(ua, "Chrome/157.0.0.0") || !strings.HasSuffix(ua, " Edg/157.0.0.0") || strings.Contains(ua, "HeadlessChrome") {
				t.Fatalf("Edge 身份未保留：version=%q goos=%s ua=%q", version, goos, ua)
			}
		}
	}
}

func TestBrowserVersionCommandWindowsReadsExecutableWithoutEvaluatingPath(t *testing.T) {
	// 文件名包含中文、空格、单引号和 PowerShell 表达式，仍须作为原始路径传递。
	binary := filepath.Join(t.TempDir(), "浏览器 ' $(exit 1)", "chrome.exe")
	command := browserVersionCommand(context.Background(), binary, "windows")
	if filepath.Base(command.Args[0]) != "powershell.exe" || len(command.Args) != 5 {
		t.Fatalf("Windows 不应执行 Chrome --version：%v", command.Args)
	}
	if command.Args[1] != "-NoProfile" || command.Args[2] != "-NonInteractive" || command.Args[3] != "-Command" || strings.Contains(command.Args[4], binary) || !strings.Contains(command.Args[4], "ProductName") || !strings.Contains(command.Args[4], "ProductVersion") {
		t.Fatalf("Windows 版本读取包含用户配置或路径插值：%v", command.Args)
	}
	if command.Env[len(command.Env)-1] != "BROWSERKIT_BROWSER_BINARY="+binary {
		t.Fatal("版本探测丢失原始浏览器路径")
	}
	for _, goos := range []string{"darwin", "linux"} {
		command = browserVersionCommand(context.Background(), binary, goos)
		if len(command.Args) != 2 || command.Args[0] != binary || command.Args[1] != "--version" {
			t.Fatalf("%s 版本探测改变：%v", goos, command.Args)
		}
	}
}

func TestLocalBrowserProfilePreservesExplicitIdentityWithoutVersionProbe(t *testing.T) {
	profile := localBrowserProfile{userAgent: "BrowserKit-Test-Agent"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	profile.useInstalledBrowserIdentity(ctx, "missing-chrome")
	if profile.userAgent != "BrowserKit-Test-Agent" {
		t.Fatal("版本探测覆盖了显式 UA")
	}
}

func TestFailedLocalLaunchRemovesOwnedProfile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	if runtime.GOOS == "windows" {
		t.Setenv("TMP", root)
		t.Setenv("TEMP", root)
	}
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
