# GoClaw Desktop Portable Wrapper

This directory contains the source code for the GoClaw Desktop Portable application. This application acts as a native Windows wrapper that seamlessly orchestrates the GoClaw backend (`goclaw.exe`), the embedded database (`pg0.exe`), and the web-based UI into a single, cohesive desktop experience.

## Features

* **Robust Background Service Orchestration**: Automatically boots `pg0.exe` and `goclaw.exe` silently in the background on startup.
* **Crash Recovery Loop**: Features a self-healing restart loop for `goclaw.exe`. If the backend crashes, it restarts automatically using exponential backoff (starting at 3s and capping at 30s) to prevent CPU thrashing.
* **Native Windows Styling (DWM)**: Uses raw Desktop Window Manager (DWM) APIs to enforce Immersive Dark Mode on the title bar and Windows 11 rounded corners. This guarantees the web wrapper feels like a premium native app.
* **Smart Window Memory**: Remembers the exact size and position of the window when closed, and perfectly restores it on the next launch using Win32 `SetWindowPlacement`.
* **System Tray & Toast Notifications**: Minimizes cleanly to the system tray, allows restarting the DB from the context menu, and pushes native OS-level Toast notifications (e.g., "Booting Up") to communicate with the user.
* **Deadlock-Free Graceful Teardown**: Ensures thread-safe shutdown across all background processes using Go context cancellation and `sync.Once`. The "Quit" system tray button calls `appCancel()` first to signal `globalCtx.Done()`, triggering a fallback signal watcher goroutine that calls `Terminate()` — guaranteeing shutdown even if called from a non-main thread. It gracefully waits for `goclaw.exe` and `pg0` to close, falling back to forceful kills if necessary, guaranteeing no zombie processes or memory access violations.

## File Architecture

### `main.go`
The core entry point of the wrapper. 
* Enforces a single-instance lock to prevent multiple apps from opening simultaneously.
* Initializes the `webview_go` instance and binds the Win32 hooks for Dark Mode and window state.
* Subclasses the window via `SetWindowLongPtrW` with a custom `wndProc` that intercepts `WM_CLOSE` (minimize to tray) and `WM_SETICON` (preserve app icon against WebView2 override).
* Manages the `goclaw.exe` background crash recovery loop using strict mutexes.
* Orchestrates the final `executeGlobalTeardown` sequence when the user quits the app.

### `pg_lifecycle.go`
Encapsulates all PostgreSQL (`pg0`) management logic.
* Handles starting the database and building dynamic connection strings.
* Runs a continuous background health-check loop that monitors the DB.
* Provides thread-safe methods to safely close or restart the DB process.

### `ui_windows.go`
Contains complex Win32-specific logic for Window manipulation.
* Manages the serialization of window coordinates so the app remembers exactly where you left it on the screen.

### `icon.ico`
The application icon, compiled directly into the binary using `go-winres` and displayed in the System Tray.

## Build Process

The wrapper is compiled using GitHub Actions via `.github/workflows/release-portable.yaml`. 
* We use `go-winres` to embed the application icon and manifest directly into the executable.
* We pass the `-ldflags="-s -w -H windowsgui"` linker flags to dramatically shrink the binary size and ensure it runs natively without popping open a black CMD console window.
