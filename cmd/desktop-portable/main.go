package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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

	os.Setenv("GOCLAW_POSTGRES_DSN", pgDSN())
	os.Setenv("GOCLAW_EDITION", "standard")
	os.Setenv("GOCLAW_AUTO_UPGRADE", "true")
	os.Setenv("GOCLAW_DESKTOP", "1")
	os.Setenv("GOCLAW_HOST", Config.GatewayHost)
	os.Setenv("GOCLAW_DATA_DIR", Config.GoclawData)

	pg0Exe := filepath.Join(root, "pg0.exe")
	os.Remove(filepath.Join(Config.Pg0DataDir, "postmaster.pid"))

	log.Println("Booting pg0 database...")
	runSilent(pg0Exe, "start", "--name", "goclaw-portable", "--data-dir", Config.Pg0DataDir, "--database", "goclaw")
	waitForPort(Config.Pg0Host, Config.Pg0Port, Config.StartTimeout)

	// Add pg0's bundled PostgreSQL bin dir to PATH (pg_dump, pg_restore, etc.)
	if pg0Bin := findPg0BinDir(); pg0Bin != "" {
		os.Setenv("PATH", pg0Bin+";"+os.Getenv("PATH"))
	}

	// Add bundled Python + pip to PATH
	if pythonDir := findPythonDir(root); pythonDir != "" {
		os.Setenv("PATH", pythonDir+";"+os.Getenv("PATH"))
	}

	// Add bundled uv to PATH
	if uvDir := findUvDir(root); uvDir != "" {
		os.Setenv("PATH", uvDir+";"+os.Getenv("PATH"))
	}

	goclawExe := filepath.Join(root, "goclaw.exe")
	goclawCmd := startSilent(goclawExe)
	waitForURL(gatewayURL(Config.HealthPath), Config.StartTimeout)

	log.Println("GoClaw is ready — opening window...")

	w := webview.New(false)
	defer w.Destroy()

	token := os.Getenv("GOCLAW_GATEWAY_TOKEN")
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
	w.Run()

	log.Println("Shutting down...")
	shutdownGoclaw(goclawCmd)
	runSilent(pg0Exe, "stop", "--name", "goclaw-portable")
}

func shutdownGoclaw(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	log.Printf("Sending graceful shutdown to goclaw (PID %d)...", pid)
	// taskkill without /F sends Ctrl+C which triggers Go's signal.Notify handler
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
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	installDir := filepath.Join(home, ".pg0", "installation")
	entries, err := os.ReadDir(installDir)
	if err != nil {
		return ""
	}
	// Scan versioned dirs (e.g. "18.1.0") for bin/pg_dump.exe
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
	if info, err := os.Stat(filepath.Join(pyDir, "python3.exe")); err == nil && !info.IsDir() {
		scriptsDir := filepath.Join(pyDir, "Scripts")
		if info, err := os.Stat(filepath.Join(scriptsDir, "pip3.exe")); err == nil && !info.IsDir() {
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
