package main

import (
	_ "embed"
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
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
	kernel32                   = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutex            = kernel32.NewProc("CreateMutexW")
	procGetLastError           = kernel32.NewProc("GetLastError")
	procSetLastError           = kernel32.NewProc("SetLastError")
	user32                     = syscall.NewLazyDLL("user32.dll")
	showWindow                 = user32.NewProc("ShowWindow")
	defWindowProc              = user32.NewProc("DefWindowProcW")
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
	
	toastAppID = "com.goclaw.portable"
)



func main() {
	if !ensureSingleInstance("GoClawPortableMutex") {
		log.Println("Another instance is already running.")
		return
	}

	// Unified lifecycle: Ctrl+C, SIGTERM, or normal exit all converge to one path.
	globalCtx, appCancel = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer appCancel()
	
	defer executeGlobalTeardown()

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
		log.Printf("pg0 start failed: %v", err)
		return
	}

	if pg0Bin := findPg0BinDir(root); pg0Bin != "" {
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
	var err error
	goclawCmd, err = startSilent(goclawExe)
	if err != nil {
		log.Printf("failed to start goclaw: %v", err)
		return
	}
	if err := waitForURL(globalCtx, gatewayURL(Config.HealthPath), Config.StartTimeout); err != nil {
		log.Printf("goclaw health check failed: %v", err)
		return
	}

	log.Println("GoClaw background services ready — starting system tray on dedicated goroutine...")
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		systray.Run(onReady, onExit)
	}()

	// Webview runs on the main thread; systray pumps Win32 events independently.
	token := os.Getenv("GOCLAW_GATEWAY_TOKEN")
	w = webview.New(false)
	setWebview(w)
	if token != "" {
		initJS := fmt.Sprintf(`(function(){
var k="goclaw:auth";
var existing = localStorage.getItem(k);
var data = existing ? JSON.parse(existing) : {state: {}, version: 0};
if (!data.state) data.state = {};
if (data.state.token !== "%s") {
	data.state.token = "%s";
	data.state.userId = "system";
	localStorage.setItem(k, JSON.stringify(data));
}
})()`, token, token)
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

	// Destroy must be called on the main UI thread (per WebView2 COM rules)
	w.Destroy()
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
			if err := pgMgr.Restart("goclaw"); err != nil {
				log.Printf("Failed to restart pg0: %v", err)
				if err.Error() == "restart cooldown active" {
					ShowToast("GoClaw", "Restart cooldown active")
				} else {
					ShowToast("GoClaw", "DB restart failed")
				}
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

// startSilent starts a process hidden and returns the Cmd or an error.
func startSilent(name string, args ...string) (*exec.Cmd, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start %s: %w", name, err)
	}
	return cmd, nil
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

// waitForURL polls the given URL until it returns 200 or the timeout/context expires.
func waitForURL(ctx context.Context, url string, timeout time.Duration) error {
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// respect external cancellation
		select {
		case <-ctx.Done():
			return fmt.Errorf("waitForURL aborted due to shutdown")
		default:
		}

		resp, err := client.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", url)
}

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

	ret, _, _ := defWindowProc.Call(uintptr(hwnd), uintptr(msg), wparam, lparam)
	return ret
}

func ensureSingleInstance(name string) bool {
	procSetLastError.Call(0)
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
