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
	"github.com/go-toast/toast"
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
	RcDevice         RECT
}

const (
	SW_SHOWNORMAL     = 1
	SW_SHOWMAXIMIZED  = 3
	SW_SHOWMINIMIZED  = 2
	SW_SHOWNA         = 8
	SW_HIDE           = 0
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
	oldWndProc                 uintptr
	wndProcCallback             uintptr
	instanceMutex               syscall.Handle
	wMu                         sync.Mutex
	restartMu                   sync.Mutex
	lastRestart                 time.Time
	kernel32                   = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutex            = kernel32.NewProc("CreateMutexW")
	procGetLastError           = kernel32.NewProc("GetLastError")
	procSetLastError           = kernel32.NewProc("SetLastError")
	user32                     = syscall.NewLazyDLL("user32.dll")
	showWindow                 = user32.NewProc("ShowWindow")
	procGetWindowPlacement     = user32.NewProc("GetWindowPlacement")
	procSetWindowPlacement     = user32.NewProc("SetWindowPlacement")
	procMonitorFromRect        = user32.NewProc("MonitorFromRect")
	procCreateIconFromResource = user32.NewProc("CreateIconFromResourceEx")
	procSendMessageW           = user32.NewProc("SendMessageW")
	procSetWindowLongPtr       = user32.NewProc("SetWindowLongPtrW")
	procCallWindowProc         = user32.NewProc("CallWindowProcW")
)

const (
	GWL_WNDPROC = ^uintptr(3)
	WM_SETICON  = 0x0080
	WM_CLOSE    = 0x0010
	ICON_SMALL  = 0
	ICON_BIG    = 1
)

// Custom window procedure intercepts WM_CLOSE to hide-to-tray instead of closing.
func wndProc(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	if msg == WM_CLOSE {
		showWindow.Call(hwnd, SW_HIDE)
		ShowToast("GoClaw", "Window minimized to tray")
		return 0
	}

	if oldWndProc != 0 {
		ret, _, _ := procCallWindowProc.Call(oldWndProc, uintptr(hwnd), uintptr(msg), wparam, lparam)
		return ret
	}

	return 0
}

func ensureSingleInstance(name string) bool {
	h, _, _ := procCreateMutex.Call(0, 1, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(name))))
	// keep the handle so the mutex stays owned for the process lifetime
	instanceMutex = syscall.Handle(h)
	lastErr, _, _ := procGetLastError.Call()
	if lastErr == 183 { // ERROR_ALREADY_EXISTS
		return false
	}
	return true
}

func setWebview(nw webview.WebView) {
	wMu.Lock()
	defer wMu.Unlock()
	w = nw
}

func getWebview() webview.WebView {
	wMu.Lock()
	defer wMu.Unlock()
	return w
}

func main() {
	if !ensureSingleInstance("GoClawPortableMutex") {
		log.Println("Another instance is already running.")
		return
	}

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
	setWebview(w)
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

	hwnd := uintptr(w.Window())
	if hwnd != 0 {
		setAppIcon(hwnd)
		loadWindowPlacement(hwnd)
		wndProcCallback = syscall.NewCallback(wndProc)
		procSetLastError.Call(0)
		old, _, _ := procSetWindowLongPtr.Call(
			hwnd,
			GWL_WNDPROC,
			wndProcCallback,
		)
		if old == 0 {
			lastErr, _, _ := procGetLastError.Call()
			if lastErr != 0 {
				log.Printf("SetWindowLongPtr failed: previous WndProc == 0, GetLastError=%d", lastErr)
			}
		}
		oldWndProc = old
	}

	// Signal watcher: Ctrl+C / SIGTERM closes the webview, triggers teardown.
	go func() {
		<-globalCtx.Done()
		log.Println("Shutdown signal received (Ctrl+C / SIGTERM). Closing webview...")
		if gv := getWebview(); gv != nil {
			gv.Terminate()
		}
	}()

	w.Run()

	// Single teardown path for all exit routes (user close, Quit menu, OS signal).
	executeGlobalTeardown()
}

// acquireRestartCooldown returns true if the restart is allowed (cooldown elapsed).
func acquireRestartCooldown() bool {
	restartMu.Lock()
	defer restartMu.Unlock()
	if time.Since(lastRestart) < 5*time.Second {
		return false
	}
	lastRestart = time.Now()
	return true
}

// releaseRestartCooldown is a no-op for now; kept for symmetry in case we want to extend behavior.
func releaseRestartCooldown() {
}

func onReady() {
	systray.SetIcon(iconData())
	systray.SetTitle(Config.WindowTitle)
	systray.SetTooltip(Config.WindowTitle)

	// Double‑click restores window properly
	systray.SetOnDClick(func(menu systray.IMenu) {
		showAppWindow(true)
	})

	mShow := systray.AddMenuItem("Show Window", "Open the GoClaw desktop window")
	mOpenWeb := systray.AddMenuItem("Open Web UI", "Open GoClaw in your default browser")
	mRestart := systray.AddMenuItem("Restart DB", "Restart pg0 database")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit GoClaw", "Shut down all background processes")

	mShow.Click(func() {
		showAppWindow(true)
		ShowToast("GoClaw", "Window restored")
	})
	mOpenWeb.Click(func() {
		log.Printf("Opening browser at %s", gatewayURL(""))
		browser.OpenURL(gatewayURL(""))
		ShowToast("GoClaw", "Web UI opened in browser")
	})
	mRestart.Click(func() {
		go func() {
			// simple cooldown to avoid rapid repeated restarts
			if !acquireRestartCooldown() {
				ShowToast("GoClaw", "Restart cooldown active")
				return
			}
			defer releaseRestartCooldown()

			if err := pgMgr.Restart("goclaw"); err != nil {
				log.Printf("Failed to restart pg0: %v", err)
				ShowToast("GoClaw", "DB restart failed")
			} else {
				ShowToast("GoClaw", "Database restarted successfully")
			}
		}()
	})
	mQuit.Click(func() {
		if gv := getWebview(); gv != nil {
			gv.Terminate() // teardown handles systray.Quit
		}
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

		if appCancel != nil {
			appCancel()
		}

		// save window placement if webview still exists
		if gv := getWebview(); gv != nil {
			if hwnd := uintptr(gv.Window()); hwnd != 0 {
				saveWindowPlacement(hwnd)
			}
		}

		shutdownGoclaw(goclawCmd)

		if pgMgr != nil {
			pgMgr.Close()
		}

		if gv := getWebview(); gv != nil {
			gv.Destroy()
		}

		// Remove systray icon only once
		systray.Quit()

		// close the instance mutex handle if we created one
		if instanceMutex != 0 {
			syscall.CloseHandle(instanceMutex)
			instanceMutex = 0
		}

		log.Println("All portable services closed cleanly.")
	})
}

func showAppWindow(forceNormal bool) {
	if gv := getWebview(); gv != nil {
		gv.Dispatch(func() {
			hwnd := uintptr(gv.Window())
			if hwnd != 0 {
				if forceNormal {
					showWindow.Call(hwnd, SW_SHOWNORMAL)
				} else {
					showWindow.Call(hwnd, SW_SHOWNA)
				}
			}
			gv.SetSize(Config.WindowWidth, Config.WindowHeight, webview.HintNone)
		})
	}
}

const toastAppID = "com.goclaw.portable"

func ShowToast(title, msg string) {
	notification := toast.Notification{
		AppID:   toastAppID,
		Title:   title,
		Message: msg,
	}
	if err := notification.Push(); err != nil {
		log.Printf("toast push failed: %v", err)
	}
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

	// Try HTTP shutdown endpoint first
	shutdownURL := gatewayURL("/shutdown")
	client := &http.Client{Timeout: 2 * time.Second}
	_, _ = client.Get(shutdownURL)

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
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("failed to generate key: %v", err)
	}
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
		// allow shutdown to interrupt waiting
		select {
		case <-globalCtx.Done():
			log.Println("waitForURL aborted due to shutdown")
			return
		default:
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
	log.Println("Window position restored.")
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
			log.Println("Window position saved.")
		}
	}
}
