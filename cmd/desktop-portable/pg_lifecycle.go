package main

import (
	"bytes"
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

type PGManager struct {
	pg0Path   string
	pidFile   string
	pg0Cmd    *exec.Cmd
	db        *sql.DB
	mu        sync.Mutex
	isAlive   bool
	exitDone  chan struct{}
	closeOnce sync.Once
	dbName    string
	ctx       context.Context
	cancel    context.CancelFunc
}

func NewPGManager(pg0Path, dataDir string) *PGManager {
	return &PGManager{
		pg0Path: pg0Path,
		pidFile: filepath.Join(dataDir, "postmaster.pid"),
	}
}

func (m *PGManager) Start(dbName string) error {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.dbName = dbName
	m.mu.Unlock()

	if err := m.cleanStaleLockNatively(); err != nil {
		return fmt.Errorf("failed to clear database lock gates safely: %w", err)
	}

	m.pg0Cmd = exec.Command(m.pg0Path, "start",
		"--name", "goclaw-portable",
		"--data-dir", filepath.Dir(m.pidFile),
		"--database", dbName,
		"--port", "5432",
	)
	m.pg0Cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	m.pg0Cmd.Stdout, m.pg0Cmd.Stderr = os.Stdout, os.Stderr

	log.Println("Starting pg0...")
	if err := m.pg0Cmd.Start(); err != nil {
		return err
	}

	m.mu.Lock()
	m.isAlive = true
	m.exitDone = make(chan struct{})
	m.closeOnce = sync.Once{}
	m.mu.Unlock()

	go func(cmd *exec.Cmd, done chan struct{}) {
		_ = cmd.Wait()
		m.mu.Lock()
		m.isAlive = false
		m.mu.Unlock()
		log.Println("pg0 background process exited")
		m.closeOnce.Do(func() { close(done) })
	}(m.pg0Cmd, m.exitDone)

	dsn := buildPgDSN(dbName)
	db, err := m.waitForDB(dsn, 30*time.Second)
	if err != nil {
		m.mu.Lock()
		if m.cancel != nil {
			m.cancel()
		}
		m.isAlive = false
		cmd := m.pg0Cmd
		done := m.exitDone
		m.mu.Unlock()

		if cmd != nil && cmd.Process != nil {
			cmd.Process.Kill()
		}
		if done != nil {
			<-done
		}
		return err
	}
	m.db = db
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
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", user, pass, host, port, dbName)
}

func (m *PGManager) Stop() {
	log.Println("Shutting down pg0...")

	m.mu.Lock()
	if m.db != nil {
		if err := m.db.Close(); err != nil {
			log.Printf("pg0: error closing database connection: %v", err)
		}
		m.db = nil
	}
	if m.cancel != nil {
		m.cancel()
	}
	cmd := m.pg0Cmd
	done := m.exitDone
	m.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		log.Println("pg0: no running process to stop")
		if done != nil {
			<-done
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stopCmd := exec.CommandContext(ctx, m.pg0Path, "stop",
		"--name", "goclaw-portable",
		"--data-dir", filepath.Dir(m.pidFile),
	)
	stopCmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	if err := stopCmd.Run(); err != nil {
		log.Printf("pg0 stop command failed — force killing process (PID %d): %v", cmd.Process.Pid, err)
		if err := cmd.Process.Kill(); err != nil {
			log.Printf("pg0: force kill failed: %v", err)
		}
	}

	if done != nil {
		<-done
	}

	if _, err := os.Stat(m.pidFile); err == nil {
		if err := os.Remove(m.pidFile); err != nil {
			log.Printf("pg0: error removing stale pid file %s: %v", m.pidFile, err)
		}
	}

	log.Println("pg0 shutdown complete")
}

func (m *PGManager) Aliveness() bool {
	m.mu.Lock()
	db := m.db
	m.mu.Unlock()

	if db == nil {
		return false
	}
	return db.Ping() == nil
}

func (m *PGManager) IsProcessRunning() bool {
	m.mu.Lock()
	alive := m.isAlive
	m.mu.Unlock()
	return alive
}

func (m *PGManager) HealthCheckLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		m.mu.Lock()
		ctx := m.ctx
		m.mu.Unlock()

		select {
		case <-ticker.C:
			if !m.Aliveness() && !m.IsProcessRunning() {
				log.Println("pg0 dead — attempting recovery restart...")
				restartDb := m.dbName
				if restartDb == "" {
					restartDb = "goclaw"
				}
				m.Stop()
				time.Sleep(2 * time.Second) // backoff
				if err := m.Start(restartDb); err != nil {
					log.Printf("pg0 recovery failed: %v", err)
				}
			}
		case <-ctx.Done():
			log.Println("pg0 health check loop exiting due to cancellation")
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
			log.Println("Database connection verified")
			return db, nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	db.Close()
	return nil, errors.New("pg0 failed to start within " + timeout.String())
}

func processExists(pid int) bool {
	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err == nil {
			return strings.Contains(out.String(), fmt.Sprintf("%d", pid))
		}
		return false
	default:
		// On Unix, FindProcess always returns a non-nil Process on success,
		// so we use Signal(0) which is a no-op that checks existence.
		p, err := os.FindProcess(pid)
		if err != nil {
			return false
		}
		return p.Signal(syscall.Signal(0)) == nil
	}
}

func (m *PGManager) cleanStaleLockNatively() error {
	if _, err := os.Stat(m.pidFile); os.IsNotExist(err) {
		return nil
	}

	data, err := os.ReadFile(m.pidFile)
	if err != nil {
		if rmErr := os.Remove(m.pidFile); rmErr != nil {
			log.Printf("pg0: failed to remove unreadable pid file %s: %v", m.pidFile, rmErr)
		}
		return nil
	}

	pidStr := strings.TrimSpace(strings.Split(string(data), "\n")[0])
	if pidStr == "" {
		if err := os.Remove(m.pidFile); err != nil {
			log.Printf("pg0: failed to remove pid file with empty PID: %v", err)
		}
		return nil
	}

	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		log.Printf("pg0: invalid PID %q in pid file — removing", pidStr)
		if err := os.Remove(m.pidFile); err != nil {
			log.Printf("pg0: failed to remove pid file with invalid PID: %v", err)
		}
		return nil
	}

	if processExists(pid) {
		return fmt.Errorf("database engine process %d is actively running", pid)
	}

	log.Printf("Removing stale lock file for inactive PID %d", pid)
	if err := os.Remove(m.pidFile); err != nil {
		log.Printf("pg0: error removing stale pid file %s: %v", m.pidFile, err)
	}
	return nil
}
