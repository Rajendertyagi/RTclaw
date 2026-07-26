package main

import (
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"unsafe"
)

//go:embed icon.ico
var tbIconData []byte

func iconData() []byte { return tbIconData }

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

// extractIconData reads the .ico header and extracts the raw image payload
// best matching targetSize (e.g., 256 or 16). Returns nil on failure.
func extractIconData(icoData []byte, targetSize int) []byte {
	if len(icoData) < 6 {
		return nil
	}
	numImages := int(binary.LittleEndian.Uint16(icoData[4:6]))
	// each directory entry is 16 bytes
	if len(icoData) < 6+numImages*16 {
		return nil
	}

	var bestOffset uint32
	var bestSize uint32
	bestWidth := 0

	for i := 0; i < numImages; i++ {
		entryOffset := 6 + i*16
		// width byte (0 means 256)
		w := int(icoData[entryOffset])
		if w == 0 {
			w = 256
		}

		size := binary.LittleEndian.Uint32(icoData[entryOffset+8 : entryOffset+12])
		offset := binary.LittleEndian.Uint32(icoData[entryOffset+12 : entryOffset+16])

		// Choose the image closest to targetSize (prefer exact or next smaller; fallback to larger)
		if bestWidth == 0 {
			bestWidth = w
			bestSize = size
			bestOffset = offset
			continue
		}

		// prefer exact match
		if w == targetSize {
			bestWidth = w
			bestSize = size
			bestOffset = offset
			break
		}

		// prefer the largest <= targetSize, otherwise the smallest > targetSize
		if (w <= targetSize && w > bestWidth && bestWidth <= targetSize) ||
			(bestWidth > targetSize && w < bestWidth && w > targetSize) ||
			(bestWidth <= targetSize && w > bestWidth && w <= targetSize) {
			bestWidth = w
			bestSize = size
			bestOffset = offset
		}
	}

	if bestOffset > 0 && int(bestOffset+bestSize) <= len(icoData) {
		return icoData[bestOffset : bestOffset+bestSize]
	}
	return nil
}

func setAppIcon(hwnd uintptr) {
	data := iconData()
	if len(data) == 0 {
		return
	}

	// 1) Large icon for taskbar / Alt-Tab (prefer 256)
	if largeIconData := extractIconData(data, 256); largeIconData != nil {
		hiconBig, _, _ := procCreateIconFromResource.Call(
			uintptr(unsafe.Pointer(&largeIconData[0])),
			uintptr(len(largeIconData)),
			1,
			0x00030000,
			0, 0, 0,
		)
		if hiconBig != 0 {
			procSendMessageW.Call(hwnd, WM_SETICON, ICON_BIG, hiconBig)
		}
	}

	// 2) Small icon for title bar / window (prefer 16)
	if smallIconData := extractIconData(data, 16); smallIconData != nil {
		hiconSmall, _, _ := procCreateIconFromResource.Call(
			uintptr(unsafe.Pointer(&smallIconData[0])),
			uintptr(len(smallIconData)),
			1,
			0x00030000,
			0, 0, 0,
		)
		if hiconSmall != 0 {
			procSendMessageW.Call(hwnd, WM_SETICON, ICON_SMALL, hiconSmall)
		}
	}
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
