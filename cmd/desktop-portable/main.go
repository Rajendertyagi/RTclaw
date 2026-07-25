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
	"time"

	"github.com/webview/webview"
)

func main() {
	root := executableDir()
	dataDir := filepath.Join(root, "data")
	ensureDir(dataDir, "pg0")
	ensureDir(dataDir, "goclaw")

	dotEnv := filepath.Join(root, ".env.local")
	loadDotEnv(dotEnv)
	saveDotEnv(dotEnv)

	encKey := getEnvOrGenerate("GOCLAW_ENCRYPTION_KEY", 32)
	gwToken := getEnvOrGenerate("GOCLAW_GATEWAY_TOKEN", 16)

	os.Setenv("GOCLAW_POSTGRES_DSN", "postgres://postgres:postgres@127.0.0.1:5432/goclaw?sslmode=disable")
	os.Setenv("GOCLAW_EDITION", "standard")
	os.Setenv("GOCLAW_ENCRYPTION_KEY", encKey)
	os.Setenv("GOCLAW_GATEWAY_TOKEN", gwToken)
	os.Setenv("GOCLAW_AUTO_UPGRADE", "true")
	os.Setenv("GOCLAW_DESKTOP", "1")
	os.Setenv("GOCLAW_HOST", "127.0.0.1")
	os.Setenv("GOCLAW_DATA_DIR", filepath.Join(dataDir, "goclaw"))

	pg0 := startProcess(filepath.Join(root, "pg0.exe"),
		"start", "--data-dir", filepath.Join(dataDir, "pg0"), "--database", "goclaw")
	waitForPort("127.0.0.1", 5432, 60*time.Second)

	goclaw := startProcess(filepath.Join(root, "goclaw.exe"))
	waitForURL("http://127.0.0.1:18790/health", 60*time.Second)

	log.Println("GoClaw is ready — opening window...")

	w := webview.New(false)
	w.SetTitle("GoClaw Portable")
	w.SetSize(1280, 800, webview.HintNone)
	w.Navigate("http://127.0.0.1:18790")
	w.Run()

	log.Println("Window closed, shutting down...")
	goclaw.Process.Kill()
	pg0.Process.Kill()
}

func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

func ensureDir(parent, child string) {
	os.MkdirAll(filepath.Join(parent, child), 0755)
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
		if len(p) != 2 {
			continue
		}
		k := strings.TrimSpace(p[0])
		v := strings.TrimSpace(p[1])
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

func saveDotEnv(path string) {
	if _, err := os.Stat(path); err == nil {
		return
	}
	content := fmt.Sprintf(`# GoClaw Portable — auto-generated config
# Edit this file to customize settings.
# Values here override defaults on next launch.

GOCLAW_ENCRYPTION_KEY=%s
GOCLAW_GATEWAY_TOKEN=%s
GOCLAW_PORT=18790
`, os.Getenv("GOCLAW_ENCRYPTION_KEY"), os.Getenv("GOCLAW_GATEWAY_TOKEN"))
	os.WriteFile(path, []byte(content), 0644)
}

func getEnvOrGenerate(key string, n int) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	b := make([]byte, n)
	rand.Read(b)
	v := hex.EncodeToString(b)
	os.Setenv(key, v)
	return v
}

func startProcess(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start %s: %v", name, err)
	}
	log.Printf("Started %s (pid %d)", name, cmd.Process.Pid)
	return cmd
}

func waitForPort(host string, port int, timeout time.Duration) {
	addr := fmt.Sprintf("%s:%d", host, port)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			log.Fatalf("Timed out waiting for %s", addr)
		case <-tick.C:
			c, err := net.DialTimeout("tcp", addr, time.Second)
			if err == nil {
				c.Close()
				log.Printf("%s is ready", addr)
				return
			}
		}
	}
}

func waitForURL(url string, timeout time.Duration) {
	client := &http.Client{Timeout: time.Second}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			log.Fatalf("Timed out waiting for %s", url)
		case <-tick.C:
			resp, err := client.Get(url)
			if err == nil && resp.StatusCode == http.StatusOK {
				resp.Body.Close()
				log.Printf("%s responded OK", url)
				return
			}
			if resp != nil {
				resp.Body.Close()
			}
		}
	}
}
