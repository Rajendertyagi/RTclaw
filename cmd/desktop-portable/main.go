package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/getlantern/systray"
	"github.com/pkg/browser"
	webview "github.com/webview/webview_go"
)

var Config = struct {
	GatewayHost  string
	GatewayPort  int
	Pg0Host      string
	Pg0Port      int
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
	Pg0Host:      "127.0.0.1",
	Pg0Port:      5432,
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
	WPF_SETMINPOSITION = 0x0001
	MONITOR_DEFAULTTONULL = 0x00000000
)

var (
	goclawCmd             *exec.Cmd
	pg0Exe                 string
	w                      webview.WebView
	quitting               atomic.Bool
	user32                 = syscall.NewLazyDLL("user32.dll")
	showWindow             = user32.NewProc("ShowWindow")
	procGetWindowPlacement = user32.NewProc("GetWindowPlacement")
	procSetWindowPlacement = user32.NewProc("SetWindowPlacement")
	procMonitorFromRect    = user32.NewProc("MonitorFromRect")
)

func main() {
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
	os.Setenv("GOCLAW_POSTGRES_DSN", pgDSN())
	os.Setenv("GOCLAW_EDITION", "standard")
	os.Setenv("GOCLAW_AUTO_UPGRADE", "true")
	os.Setenv("GOCLAW_DESKTOP", "1")
	os.Setenv("GOCLAW_HOST", Config.GatewayHost)
	os.Setenv("GOCLAW_DATA_DIR", Config.GoclawData)

	pg0Exe = filepath.Join(root, "pg0.exe")
	os.Remove(filepath.Join(Config.Pg0DataDir, "postmaster.pid"))

	log.Println("Booting pg0 database...")
	runSilent(pg0Exe, "start", "--name", "goclaw-portable", "--data-dir", Config.Pg0DataDir, "--database", "goclaw")
	waitForPort(Config.Pg0Host, Config.Pg0Port, Config.StartTimeout)

	if pg0Bin := findPg0BinDir(); pg0Bin != "" {
		os.Setenv("PATH", pg0Bin+";"+os.Getenv("PATH"))
	}
	if pythonDir := findPythonDir(root); pythonDir != "" {
		os.Setenv("PATH", pythonDir+";"+os.Getenv("PATH"))
	}
	if uvDir := findUvDir(root); uvDir != "" {
		os.Setenv("PATH", uvDir+";"+os.Getenv("PATH"))
	}

	goclawExe := filepath.Join(root, "goclaw.exe")
	goclawCmd = startSilent(goclawExe)
	waitForURL(gatewayURL(Config.HealthPath), Config.StartTimeout)

	log.Println("GoClaw background services ready — starting system tray...")
	systray.Register(onReady, nil)

	// Webview must be on the main thread; systray.Register sets up the tray
	// without blocking, so the webview message loop pumps both windows.
	token := os.Getenv("GOCLAW_GATEWAY_TOKEN")
	w = webview.New(false)
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
	}

	w.Run()
	w.Destroy()

	log.Println("Shutting down core processes...")
	shutdownGoclaw(goclawCmd)
	if pg0Exe != "" {
		runSilent(pg0Exe, "stop", "--name", "goclaw-portable")
	}
	os.Exit(0)
}

func onReady() {
	systray.SetIcon(iconData())
	systray.SetTitle(Config.WindowTitle)
	systray.SetTooltip(Config.WindowTitle)

	mShow := systray.AddMenuItem("Show Window", "Open the GoClaw desktop window")
	mOpenWeb := systray.AddMenuItem("Open Web UI", "Open GoClaw in your default browser")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit GoClaw", "Shut down all background processes")

	go func() {
		for {
			select {
			case <-mShow.ClickedCh:
				w.Dispatch(func() {
					hwnd := w.Window()
					if hwnd != nil {
						showWindow.Call(uintptr(hwnd), 5)
					}
				})
			case <-mOpenWeb.ClickedCh:
				log.Printf("Opening browser at %s", gatewayURL(""))
				browser.OpenURL(gatewayURL(""))
			case <-mQuit.ClickedCh:
				quitting.Store(true)
				hwnd := w.Window()
				if hwnd != nil {
					saveWindowPlacement(uintptr(hwnd))
				}
				systray.Quit()
				return
			}
		}
	}()
}

func showAppWindow() {
	w.Dispatch(func() {
		hwnd := w.Window()
		if hwnd != nil {
			showWindow.Call(uintptr(hwnd), 5)
		}
		w.SetSize(Config.WindowWidth, Config.WindowHeight, webview.HintNone)
	})
}

func iconData() []byte {
	return []byte{
		0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x10, 0x10,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x68, 0x05,
		0x00, 0x00, 0x16, 0x00, 0x00, 0x00, 0x28, 0x00,
		0x00, 0x00, 0x10, 0x00, 0x00, 0x00, 0x20, 0x00,
		0x00, 0x00, 0x01, 0x00, 0x04, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
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

func pgDSN() string {
	return fmt.Sprintf("postgres://postgres:postgres@%s:%d/goclaw?sslmode=disable",
		Config.Pg0Host, Config.Pg0Port)
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

func waitForPort(host string, port int, timeout time.Duration) {
	addr := fmt.Sprintf("%s:%d", host, port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	log.Fatalf("Timed out waiting for %s", addr)
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
			return pyDir + ";" + scriptsDir
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
