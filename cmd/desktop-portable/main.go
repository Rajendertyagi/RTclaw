package main

import (
	_ "embed"
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/energye/systray"
	"github.com/pkg/browser"
	webview "github.com/webview/webview_go"
)

//go:embed icon.ico
var tbIconData []byte

var Config = struct {
	GatewayHost  string
	GatewayPort  int
	WindowTitle  string
	WindowWidth  int
	WindowHeight int
	Pg0DataDir   string
	GoclawData   string
	StartTimeout time.Duration
	HealthPath   string
}{
	GatewayHost:  "127.0.0.1",
	GatewayPort:  18790,
	WindowTitle:  "GoClaw Portable",
	WindowWidth:  1280,
	WindowHeight: 800,
	StartTimeout: 60 * time.Second,
	HealthPath:   "/health",
}

type POINT struct {
	X, Y int32
}

type RECT struct {
	Left, Top, Right, Bottom int32
}

type WindowPlacement struct {
	Length           uint32
	Flags            uint32
	ShowCmd          uint32
	PtMinPosition    POINT
	PtMaxPosition    POINT
	RcNormalPosition RECT
}

const (
	SW_SHOWNORMAL     = 1
	SW_SHOWMAXIMIZED  = 3
	SW_SHOWMINIMIZED  = 2
	SW_SHOWNA         = 8
	WPF_SETMINPOSITION = 0x0001
	MONITOR_DEFAULTTONULL = 0x00000000
)

var (
	goclawCmd                 *exec.Cmd
	pgMgr                      *PGManager
	w                          webview.WebView
	globalCtx                  context.Context
	appCancel                  context.CancelFunc
	teardownOnce               sync.Once
	user32                     = syscall.NewLazyDLL("user32.dll")
	showWindow                 = user32.NewProc("ShowWindow")
	procGetWindowPlacement     = user32.NewProc("GetWindowPlacement")
	procSetWindowPlacement     = user32.NewProc("SetWindowPlacement")
	procMonitorFromRect        = user32.NewProc("MonitorFromRect")
	procCreateIconFromResource = user32.NewProc("CreateIconFromResourceEx")
	procSendMessageW           = user32.NewProc("SendMessageW")
)

const (
	WM_SETICON = 0x0080
	ICON_SMALL = 0
	ICON_BIG   = 1
)

func main() {
	// Unified lifecycle: Ctrl+C, SIGTERM, or normal exit all converge to one path.
	globalCtx, appCancel = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer appCancel()

	root := executableDir()
	dataDir := filepath.Join(root, "data")
	Config.Pg0DataDir = filepath.Join(dataDir, "pg0")
	Config.GoclawData = filepath.Join(dataDir, "goclaw")

	mkdirAll(dataDir, "pg0")
	mkdirAll(dataDir, "goclaw")

	dotEnv := filepath.Join(root, ".env.local")
	loadDotEnv(dotEnv)

	if os.Getenv("GOCLAW_ENCRYPTION_KEY") == "" {
		saveDotEnv(dotEnv, "GOCLAW_ENCRYPTION_KEY", generateKey(32))
		saveDotEnv(dotEnv, "GOCLAW_GATEWAY_TOKEN", generateKey(16))
		loadDotEnv(dotEnv)
	}

	os.Setenv("GOCLAW_CONFIG", filepath.Join(root, "config.json"))
	os.Setenv("GOCLAW_POSTGRES_DSN", buildPgDSN("goclaw"))
	os.Setenv("GOCLAW_EDITION", "standard")
	os.Setenv("GOCLAW_AUTO_UPGRADE", "true")
	os.Setenv("GOCLAW_DESKTOP", "1")
	os.Setenv("GOCLAW_HOST", Config.GatewayHost)
	os.Setenv("GOCLAW_DATA_DIR", Config.GoclawData)
	os.Setenv("GOCLAW_WORKSPACE", filepath.Join(Config.GoclawData, "workspace"))

	pg0Path := filepath.Join(root, "pg0.exe")
	pgMgr = NewPGManager(pg0Path, Config.Pg0DataDir)

	log.Println("Booting pg0 database...")
	if err := pgMgr.Start("goclaw"); err != nil {
		log.Fatalf("pg0: %v", err)
	}

	if pg0Bin := findPg0BinDir(); pg0Bin != "" {
		os.Setenv("PATH", pg0Bin+string(filepath.ListSeparator)+os.Getenv("PATH"))
	}
	if pythonDir := findPythonDir(root); pythonDir != "" {
		os.Setenv("PATH", pythonDir+string(filepath.ListSeparator)+os.Getenv("PATH"))
	}
	if uvDir := findUvDir(root); uvDir != "" {
		os.Setenv("PATH", uvDir+string(filepath.ListSeparator)+os.Getenv("PATH"))
	}

	// Monitor pg0 health in background — restart if it dies
	go pgMgr.HealthCheckLoop()

	goclawExe := filepath.Join(root, "goclaw.exe")
	goclawCmd = startSilent(goclawExe)
	waitForURL(gatewayURL(Config.HealthPath), Config.StartTimeout)

	log.Println("GoClaw background services ready — starting system tray on dedicated goroutine...")
	go func() {
		systray.Register(onReady, onExit)
	}()

	// Webview runs on the main thread; systray pumps Win32 events independently.
	token := os.Getenv("GOCLAW_GATEWAY_TOKEN")
	w = webview.New(false)
	defer w.Destroy()
	if token != "" {
		initJS := fmt.Sprintf(`(function(){
var k="goclaw:auth";
if(!localStorage.getItem(k)){
localStorage.setItem(k,JSON.stringify({
state:{token:"%s",userId:"system",senderID:""},version:0
}));
}
})()`, token)
		w.Init(initJS)
	}
	w.SetTitle(Config.WindowTitle)
	w.SetSize(Config.WindowWidth, Config.WindowHeight, webview.HintNone)
	w.Navigate(gatewayURL(""))

	hwnd := w.Window()
	if hwnd != nil {
		loadWindowPlacement(uintptr(hwnd))
		setAppIcon(uintptr(hwnd))
	}

	// Signal watcher: Ctrl+C / SIGTERM closes the webview, triggers teardown.
	go func() {
		<-globalCtx.Done()
		log.Println("Shutdown signal received (Ctrl+C / SIGTERM). Closing webview...")
		w.Terminate()
	}()

	w.Run()

	// Single teardown path for all exit routes (user close, Quit menu, OS signal).
	executeGlobalTeardown()
}

func onReady() {
	systray.SetIcon(iconData())
	systray.SetTitle(Config.WindowTitle)
	systray.SetTooltip(Config.WindowTitle)

	systray.SetOnDClick(func(menu systray.IMenu) {
		showAppWindow()
	})
	systray.SetOnRClick(func(menu systray.IMenu) {
		if err := menu.ShowMenu(); err != nil {
			log.Printf("Failed to show systray menu: %v", err)
		}
	})

	mShow := systray.AddMenuItem("Show Window", "Open the GoClaw desktop window")
	mOpenWeb := systray.AddMenuItem("Open Web UI", "Open GoClaw in your default browser")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit GoClaw", "Shut down all background processes")

	mShow.Click(func() {
		showAppWindow()
	})
	mOpenWeb.Click(func() {
		log.Printf("Opening browser at %s", gatewayURL(""))
		browser.OpenURL(gatewayURL(""))
	})
	mQuit.Click(func() {
		systray.Quit()
		w.Terminate()
	})
}

func onExit() {
	log.Println("Exit triggered from system tray context menu.")
	executeGlobalTeardown()
}

// executeGlobalTeardown unifies all exit paths into one ordered sequence.
// It is guaranteed to run exactly once via teardownOnce.
func executeGlobalTeardown() {
	teardownOnce.Do(func() {
		log.Println("Executing synchronized global cleanup...")

		// Cancel the global context to signal any background watchers.
		if appCancel != nil {
			appCancel()
		}

		// Persist window placement before teardown.
		hwnd := w.Window()
		if hwnd != nil {
			saveWindowPlacement(uintptr(hwnd))
		}

		// Gracefully stop the goclaw backend process.
		shutdownGoclaw(goclawCmd)

		// Close database — cancels m.ctx, health loop exits, PG stops.
		if pgMgr != nil {
			pgMgr.Close()
		}

		// Remove systray icon.
		systray.Quit()

		log.Println("All portable services closed cleanly.")
	})
}

func showAppWindow() {
	w.Dispatch(func() {
		hwnd := w.Window()
		if hwnd != nil {
			showWindow.Call(uintptr(hwnd), SW_SHOWNA)
		}
		w.SetSize(Config.WindowWidth, Config.WindowHeight, webview.HintNone)
	})
}

func iconData() []byte { return tbIconData }

func setAppIcon(hwnd uintptr) {
	data := iconData()
	if len(data) == 0 {
		return
	}
	hicon, _, _ := procCreateIconFromResource.Call(
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
		1,
		0x00030000,
		0, 0, 0,
	)
	if hicon != 0 {
		procSendMessageW.Call(hwnd, WM_SETICON, ICON_BIG, hicon)
		procSendMessageW.Call(hwnd, WM_SETICON, ICON_SMALL, hicon)
	}
}

func shutdownGoclaw(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	log.Printf("Sending graceful shutdown to goclaw (PID %d)...", pid)
	exec.Command("taskkill", "/PID", fmt.Sprintf("%d", pid)).Run()
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Println("goclaw exited gracefully")
	case <-time.After(10 * time.Second):
		log.Println("goclaw did not exit in time, force killing...")
		cmd.Process.Kill()
	}
}

func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

func mkdirAll(parent, child string) {
	os.MkdirAll(filepath.Join(parent, child), 0755)
}

func gatewayURL(path string) string {
	return fmt.Sprintf("http://%s:%d%s", Config.GatewayHost, Config.GatewayPort, path)
}

func runSilent(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err != nil {
		log.Printf("%s completed with: %v", name, err)
	}
}

func startSilent(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start %s: %v", name, err)
	}
	return cmd
}

func generateKey(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := strings.SplitN(line, "=", 2)
		if len(p) == 2 {
			k, v := strings.TrimSpace(p[0]), strings.TrimSpace(p[1])
			if os.Getenv(k) == "" {
				os.Setenv(k, v)
			}
		}
	}
}

func saveDotEnv(path string, key, value string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s=%s\n", key, value)
}

func findPg0BinDir() string {
	root := executableDir()
	bin := filepath.Join(root, "bin")
	if info, err := os.Stat(filepath.Join(bin, "pg_dump.exe")); err == nil && !info.IsDir() {
		return bin
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	installDir := filepath.Join(home, ".pg0", "installation")
	entries, err := os.ReadDir(installDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			bin := filepath.Join(installDir, e.Name(), "bin")
			if info, err := os.Stat(filepath.Join(bin, "pg_dump.exe")); err == nil && !info.IsDir() {
				return bin
			}
		}
	}
	return ""
}

func findPythonDir(root string) string {
	pyDir := filepath.Join(root, "python")
	if info, err := os.Stat(filepath.Join(pyDir, "python.exe")); err == nil && !info.IsDir() {
		scriptsDir := filepath.Join(pyDir, "Scripts")
		if info, err := os.Stat(filepath.Join(scriptsDir, "pip.exe")); err == nil && !info.IsDir() {
			return pyDir + string(filepath.ListSeparator) + scriptsDir
		}
		return pyDir
	}
	return ""
}

func findUvDir(root string) string {
	if info, err := os.Stat(filepath.Join(root, "uv.exe")); err == nil && !info.IsDir() {
		return root
	}
	return ""
}

func waitForURL(url string, timeout time.Duration) {
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	log.Fatalf("Timed out waiting for %s", url)
}

func windowPlacementPath() string {
	return filepath.Join(executableDir(), "data", "window-state.json")
}

func loadWindowPlacement(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	data, err := os.ReadFile(windowPlacementPath())
	if err != nil {
		return
	}
	var wp WindowPlacement
	if err := json.Unmarshal(data, &wp); err != nil {
		return
	}
	wp.Length = uint32(unsafe.Sizeof(wp))

	mon, _, _ := procMonitorFromRect.Call(
		uintptr(unsafe.Pointer(&wp.RcNormalPosition)),
		MONITOR_DEFAULTTONULL,
	)
	if mon == 0 {
		log.Println("Saved window position is off-screen. Falling back to defaults.")
		return
	}

	if wp.ShowCmd == SW_SHOWMINIMIZED {
		wp.Flags |= WPF_SETMINPOSITION
		wp.ShowCmd = SW_SHOWNORMAL
	}

	_, _, _ = procSetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&wp)))
}

func saveWindowPlacement(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	var wp WindowPlacement
	wp.Length = uint32(unsafe.Sizeof(wp))

	ret, _, _ := procGetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&wp)))
	if ret != 0 {
		data, err := json.MarshalIndent(wp, "", "  ")
		if err == nil {
			_ = os.MkdirAll(filepath.Join(executableDir(), "data"), 0755)
			_ = os.WriteFile(windowPlacementPath(), data, 0644)
		}
	}
}
