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
	goclawCmdMu               sync.Mutex
	goclawShutdownWg          sync.WaitGroup
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
	setForegroundWindow        = user32.NewProc("SetForegroundWindow")
	defWindowProc              = user32.NewProc("DefWindowProcW")
	procGetWindowPlacement     = user32.NewProc("GetWindowPlacement")
	procSetWindowPlacement     = user32.NewProc("SetWindowPlacement")
	procMonitorFromRect        = user32.NewProc("MonitorFromRect")
	procCreateIconFromResource = user32.NewProc("CreateIconFromResourceEx")
	procSendMessageW           = user32.NewProc("SendMessageW")
	procSetWindowLongPtr       = user32.NewProc("SetWindowLongPtrW")
	procCallWindowProc         = user32.NewProc("CallWindowProcW")
	dwmapi                     = syscall.NewLazyDLL("dwmapi.dll")
	procDwmSetWindowAttribute  = dwmapi.NewProc("DwmSetWindowAttribute")
)

const (
	GWL_WNDPROC = ^uintptr(3)
	WM_SETICON  = 0x0080
	WM_CLOSE    = 0x0010
	ICON_SMALL  = 0
	ICON_BIG    = 1
	
	toastAppID = "com.goclaw.portable"

	DWMWA_USE_IMMERSIVE_DARK_MODE  = 20
	DWMWA_WINDOW_CORNER_PREFERENCE = 33
	DWMWCP_ROUND                   = 2
)



func main() {
	if !ensureSingleInstance("GoClawPortableMutex") {
		log.Println("Another instance is already running.")
		return
	}

	ShowToast("GoClaw", "GoClaw is booting up! Please wait a moment...")

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

	// safe restart loop
	go func() {
		// indicate the loop is running and will perform shutdown work
		goclawShutdownWg.Add(1)
		defer func() {
			goclawShutdownWg.Done()
		}()

		backoff := 3 * time.Second
		maxBackoff := 30 * time.Second

		for {
			if globalCtx.Err() != nil {
				return
			}

			cmd, err := startSilent(goclawExe)
			if err != nil {
				log.Printf("failed to start goclaw: %v; retrying in %s", err, backoff)
				select {
				case <-time.After(backoff):
					backoff *= 2
					if backoff > maxBackoff { backoff = maxBackoff }
					continue
				case <-globalCtx.Done():
					return
				}
			}

			goclawCmdMu.Lock()
			goclawCmd = cmd
			goclawCmdMu.Unlock()

			backoff = 3 * time.Second

			done := make(chan error, 1)
			go func(c *exec.Cmd) { done <- c.Wait() }(cmd)

			select {
			case <-globalCtx.Done():
				// graceful shutdown attempt
				shutdownURL := gatewayURL("/shutdown")
				client := &http.Client{Timeout: 2 * time.Second}
				_, _ = client.Get(shutdownURL)

				select {
				case <-done:
				case <-time.After(5 * time.Second):
					if cmd.Process != nil { _ = cmd.Process.Kill() }
					<-done
				}

				goclawCmdMu.Lock(); goclawCmd = nil; goclawCmdMu.Unlock()
				return

			case err := <-done:
				if err != nil {
					log.Printf("goclaw exited with error: %v", err)
				} else {
					log.Println("goclaw exited normally")
				}
				goclawCmdMu.Lock(); goclawCmd = nil; goclawCmdMu.Unlock()

				if globalCtx.Err() != nil { return }

				select {
				case <-time.After(3 * time.Second):
				case <-globalCtx.Done():
					return
				}
			}
		}
	}()

	// We still want to block startup until goclaw is healthy the first time
	if err := waitForURL(globalCtx, gatewayURL(Config.HealthPath), Config.StartTimeout); err != nil {
		log.Printf("goclaw initial health check failed: %v", err)
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

		// Apply native polish (run on GUI thread)
		if err := SetImmersiveDarkMode(hwnd, true); err != nil {
			log.Printf("SetImmersiveDarkMode: %v", err)
		}
		if err := SetWindowCornerPreference(hwnd, DWMWCP_ROUND); err != nil {
			log.Printf("SetWindowCornerPreference: %v", err)
		}

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

	// Save window placement BEFORE destroying!
	if hwnd := uintptr(w.Window()); hwnd != 0 {
		saveWindowPlacement(hwnd)
	}

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
		if appCancel != nil {
			appCancel() // signal globalCtx.Done()
		}
		if gv := getWebview(); gv != nil {
			gv.Terminate()
		}
	})
}

func onExit() {
	log.Println("Exit triggered from system tray context menu.")
	if appCancel != nil {
		appCancel()
	}
}

// executeGlobalTeardown unifies all exit paths into one ordered sequence.
// It is guaranteed to run exactly once via teardownOnce.
func executeGlobalTeardown() {
	teardownOnce.Do(func() {
		log.Println("Executing synchronized global cleanup...")

		if appCancel != nil { appCancel() }

		// Wait for goclaw restart loop to finish its shutdown work, but don't block forever.
		done := make(chan struct{})
		go func() {
			goclawShutdownWg.Wait()
			close(done)
		}()

		select {
		case <-done:
			// loop finished; safe to continue
		case <-time.After(10 * time.Second):
			log.Println("Timed out waiting for goclaw shutdown loop; proceeding with teardown")
		}

		// ensure goclaw is stopped (best-effort fallback)
		goclawCmdMu.Lock()
		cmd := goclawCmd
		goclawCmdMu.Unlock()
		if cmd != nil && cmd.Process != nil {
			log.Println("Force killing goclaw as a last resort...")
			cmd.Process.Kill()
		}

		if pgMgr != nil {
			pgMgr.Close()
		}

		// Remove systray icon only once
		systray.Quit()

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
					setForegroundWindow.Call(hwnd)
				} else {
					showWindow.Call(hwnd, SW_SHOWNA)
				}
			}
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



// shutdownGoclaw was removed as it is now handled cleanly by the restart loop.

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

var settingIcon bool

// Custom window procedure intercepts WM_CLOSE to hide-to-tray instead of closing.
func wndProc(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	switch msg {
	case WM_CLOSE:
		showWindow.Call(hwnd, SW_HIDE)
		ShowToast("GoClaw", "Window minimized to tray")
		return 0

	case WM_SETICON:
		if !settingIcon {
			settingIcon = true
			var prevIcon uintptr
			if oldWndProc != 0 {
				prevIcon, _, _ = procCallWindowProc.Call(oldWndProc, hwnd, uintptr(msg), wparam, lparam)
			} else {
				prevIcon, _, _ = defWindowProc.Call(hwnd, uintptr(msg), wparam, lparam)
			}
			setAppIcon(hwnd)
			settingIcon = false
			return prevIcon
		}
	}

	if oldWndProc != 0 {
		ret, _, _ := procCallWindowProc.Call(oldWndProc, hwnd, uintptr(msg), wparam, lparam)
		return ret
	}
	ret, _, _ := defWindowProc.Call(hwnd, uintptr(msg), wparam, lparam)
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

// setWindowAttributeUint32 sets a uint32 DWM attribute; returns error on nonzero result.
func setWindowAttributeUint32(hwnd uintptr, attr uint32, value uint32) error {
	v := value
	r, _, err := procDwmSetWindowAttribute.Call(
		uintptr(hwnd),
		uintptr(attr),
		uintptr(unsafe.Pointer(&v)),
		uintptr(unsafe.Sizeof(v)),
	)
	if r != 0 {
		return fmt.Errorf("DwmSetWindowAttribute failed: ret=%d err=%v", r, err)
	}
	return nil
}

func SetImmersiveDarkMode(hwnd uintptr, enable bool) error {
	var v uint32
	if enable {
		v = 1
	}
	return setWindowAttributeUint32(hwnd, DWMWA_USE_IMMERSIVE_DARK_MODE, v)
}

func SetWindowCornerPreference(hwnd uintptr, pref uint32) error {
	return setWindowAttributeUint32(hwnd, DWMWA_WINDOW_CORNER_PREFERENCE, pref)
}
