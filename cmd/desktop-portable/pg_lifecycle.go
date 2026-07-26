package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/lib/pq"
)

// PGManager manages the pg0 PostgreSQL process lifecycle.
type PGManager struct {
	pg0Path string
	pidFile string
	dbName  string
	db      *sql.DB
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
}

func NewPGManager(pg0Path, dataDir string) *PGManager {
	return &PGManager{
		pg0Path: pg0Path,
		pidFile: filepath.Join(dataDir, "postmaster.pid"),
	}
}

// Start invokes pg0 start and blocks until the database accepts connections.
func (m *PGManager) Start(dbName string) error {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.dbName = dbName
	m.mu.Unlock()

	// Remove stale lock file if the process listed inside no longer exists
	if err := m.cleanStaleLockNatively(); err != nil {
		return err
	}

	// pg0 start launches PostgreSQL in the background and exits quickly
	startCmd := exec.Command(m.pg0Path, "start",
		"--name", "goclaw-portable",
		"--data-dir", filepath.Dir(m.pidFile),
		"--database", dbName,
		"--port", "5432",
	)
	startCmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	startCmd.Stdout, startCmd.Stderr = os.Stdout, os.Stderr

	log.Println("Starting pg0 wrapper...")
	if err := startCmd.Run(); err != nil {
		return fmt.Errorf("pg0 wrapper failed to execute launch: %w", err)
	}

	// Wait for the actual PostgreSQL server to accept connections
	dsn := buildPgDSN(dbName)
	db, err := m.waitForDB(dsn, 30*time.Second)
	if err != nil {
		log.Printf("Database server failed to respond post-launch: %v", err)
		m.Stop()
		// Re-create context so HealthCheckLoop doesn't see a cancelled ctx and exit
		m.mu.Lock()
		m.ctx, m.cancel = context.WithCancel(context.Background())
		m.mu.Unlock()
		return err
	}

	m.mu.Lock()
	m.db = db
	m.mu.Unlock()
	return nil
}

func buildPgDSN(dbName string) string {
	user := os.Getenv("PG_USER")
	if user == "" {
		user = "postgres"
	}
	pass := os.Getenv("PG_PASS")
	if pass == "" {
		pass = "postgres"
	}
	host := Config.Pg0Host
	port := Config.Pg0Port
	if host == "" {
		host = "127.0.0.1"
	}
	if port == 0 {
		port = 5432
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		user, pass, host, port, dbName)
}

// Stop shuts down the PostgreSQL process gracefully, then force-kills if hung.
func (m *PGManager) Stop() {
	log.Println("Shutting down pg0 database...")

	m.mu.Lock()
	if m.db != nil {
		_ = m.db.Close()
		m.db = nil
	}
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()

	// Graceful shutdown via pg0 stop
	stopCmd := exec.Command(m.pg0Path, "stop",
		"--name", "goclaw-portable",
		"--data-dir", filepath.Dir(m.pidFile),
	)
	stopCmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = stopCmd.Run()

	// Allow time for file descriptor cleanup
	time.Sleep(500 * time.Millisecond)

	// Force-kill if the PostgreSQL PID is still alive
	if pid, err := m.readPidFromLockFile(); err == nil && pid > 0 {
		if processExists(pid) {
			log.Printf("Postgres process (PID %d) hung after stop signal — force killing...", pid)
			if proc, err := os.FindProcess(pid); err == nil {
				_ = proc.Kill()
				_ = proc.Wait()
			}
		}
	}

	_ = os.Remove(m.pidFile)
	log.Println("Database shutdown complete.")
}

// DB returns a mutex-safe reference to the *sql.DB handle.
func (m *PGManager) DB() *sql.DB {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.db
}

// Aliveness validates a real database ping.
func (m *PGManager) Aliveness() bool {
	m.mu.Lock()
	db := m.db
	m.mu.Unlock()

	if db == nil {
		return false
	}
	return db.Ping() == nil
}

// IsProcessRunning reads postmaster.pid and checks if that exact PID is running.
func (m *PGManager) IsProcessRunning() bool {
	pid, err := m.readPidFromLockFile()
	if err != nil || pid <= 0 {
		return false
	}
	return processExists(pid)
}

// HealthCheckLoop monitors database health and triggers recovery only when
// both the DB ping fails AND the OS process has died.
func (m *PGManager) HealthCheckLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		m.mu.Lock()
		loopCtx := m.ctx
		restartDb := m.dbName
		m.mu.Unlock()

		if loopCtx == nil {
			return
		}
		if restartDb == "" {
			restartDb = "goclaw"
		}

		select {
		case <-ticker.C:
			if !m.Aliveness() && !m.IsProcessRunning() {
				log.Println("CRITICAL: Underlying database process is completely dead. Triggering recovery restart...")
				m.Stop()
				time.Sleep(2 * time.Second)
				if err := m.Start(restartDb); err != nil {
					log.Printf("Database recovery restart failed: %v", err)
				}
			}
		case <-loopCtx.Done():
			log.Println("Database health check loop exiting.")
			return
		}
	}
}

func (m *PGManager) waitForDB(dsn string, timeout time.Duration) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := db.Ping(); err == nil {
			log.Println("Database layer connection verified.")
			return db, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = db.Close()
	return nil, errors.New("timeout waiting for database instance response")
}

func (m *PGManager) readPidFromLockFile() (int, error) {
	data, err := os.ReadFile(m.pidFile)
	if err != nil {
		return 0, err
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 {
		return 0, errors.New("empty pid file")
	}
	pidStr := strings.TrimSpace(lines[0])
	return strconv.Atoi(pidStr)
}

func processExists(pid int) bool {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		out, err := cmd.Output()
		return err == nil && strings.Contains(string(out), fmt.Sprintf("%d", pid))
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func (m *PGManager) cleanStaleLockNatively() error {
	pid, err := m.readPidFromLockFile()
	if err != nil {
		_ = os.Remove(m.pidFile)
		return nil
	}

	if processExists(pid) {
		return fmt.Errorf("database engine (PID %d) is actively running; refusing to clear lock or spawn clone", pid)
	}

	log.Printf("Removing dead lock file for stale PID %d", pid)
	_ = os.Remove(m.pidFile)
	return nil
}
