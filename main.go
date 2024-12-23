// +build windows

package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// The name of the file where mouse recording is saved.
const (
	recordFileName = "recorded-mice.cfg"
)

// -----------------------------------------------------------------------------
// Win32 API constants & structures
// -----------------------------------------------------------------------------

const (
	WH_KEYBOARD_LL = 13
	WH_MOUSE_LL    = 14

	WM_KEYDOWN   = 0x0100
	WM_SYSKEYDOWN = 0x0104

	VK_INSERT = 0x2D
	VK_END    = 0x23

	// For the message loop
	WM_QUIT = 0x0012
)

// KBDLLHOOKSTRUCT as defined in WinUser.h (low-level keyboard hook).
type KBDLLHOOKSTRUCT struct {
	VKCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	ExtraInfo   uintptr
}

// MSLLHOOKSTRUCT as defined in WinUser.h (low-level mouse hook).
type MSLLHOOKSTRUCT struct {
	Point     POINT
	MouseData uint32
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

// POINT structure for mouse coordinate.
type POINT struct {
	X int32
	Y int32
}

// MSG structure for the message loop.
type MSG struct {
	HWND    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      POINT
}

// -----------------------------------------------------------------------------
// Global variables
// -----------------------------------------------------------------------------

var (
	user32              = syscall.MustLoadDLL("user32.dll")
	kernel32            = syscall.MustLoadDLL("kernel32.dll")

	procSetWindowsHookExW = user32.MustFindProc("SetWindowsHookExW")
	procCallNextHookEx    = user32.MustFindProc("CallNextHookEx")
	procGetMessageW       = user32.MustFindProc("GetMessageW")
	procSetCursorPos      = user32.MustFindProc("SetCursorPos")
	procGetCursorPos      = user32.MustFindProc("GetCursorPos")
	procUnhookWindowsHookEx = user32.MustFindProc("UnhookWindowsHookEx")

	// Hooks
	hKeyboardHook syscall.Handle
	hMouseHook    syscall.Handle

	// Synchronization
	mtx          sync.Mutex
	isRecording  bool
	recordedData []MouseRecord

	// For timing the recordings
	lastEventTime time.Time
)

// MouseRecord is the structure we will save for each event.
type MouseRecord struct {
	// DeltaMS is how many milliseconds have passed since the previous event.
	DeltaMS int64 `json:"DeltaMS"`
	// X, Y store the absolute coordinates of the mouse.
	X       int32 `json:"X"`
	Y       int32 `json:"Y"`
}

// -----------------------------------------------------------------------------
// Hook procedures (exported via cgo syntax for callback from Windows)
// -----------------------------------------------------------------------------

//export keyboardHookProc
func keyboardHookProc(code int, wparam uintptr, lparam uintptr) uintptr {
	// If code < 0, skip
	if code < 0 {
		ret, _, _ := procCallNextHookEx.Call(0, uintptr(code), wparam, lparam)
		return ret
	}

	// We only care about WM_KEYDOWN or WM_SYSKEYDOWN
	if wparam == WM_KEYDOWN || wparam == WM_SYSKEYDOWN {
		kbStruct := (*KBDLLHOOKSTRUCT)(unsafe.Pointer(lparam))
		switch kbStruct.VKCode {
		case VK_INSERT:
			// Start recording
			mtx.Lock()
			isRecording = true
			recordedData = make([]MouseRecord, 0)
			lastEventTime = time.Now()
			mtx.Unlock()

			fmt.Println("[INFO] Insert key pressed -> Start recording")

		case VK_END:
			// Stop recording, dump data, replay
			mtx.Lock()
			isRecording = false
			err := dumpToFile(recordFileName, recordedData)
			mtx.Unlock()

			if err != nil {
				fmt.Println("[ERROR] Could not write to file:", err)
			} else {
				fmt.Println("[INFO] Recording saved to", recordFileName)
				fmt.Println("[INFO] Now replaying the recorded movements...")
				err = replayFromFile(recordFileName)
				if err != nil {
					fmt.Println("[ERROR] Replay failed:", err)
				} else {
					fmt.Println("[INFO] Replay completed.")
				}
			}
		}
	}

	ret, _, _ := procCallNextHookEx.Call(0, uintptr(code), wparam, lparam)
	return ret
}

//export mouseHookProc
func mouseHookProc(code int, wparam uintptr, lparam uintptr) uintptr {
	if code < 0 {
		ret, _, _ := procCallNextHookEx.Call(0, uintptr(code), wparam, lparam)
		return ret
	}

	// Record only if we are in isRecording mode
	mtx.Lock()
	rec := isRecording
	mtx.Unlock()

	if rec {
		// Low-level mouse hook data
		msStruct := (*MSLLHOOKSTRUCT)(unsafe.Pointer(lparam))

		// We only handle mouse move messages. Windows doesn't directly send
		// "mouse move" codes in wparam for WH_MOUSE_LL, but we can track
		// all mouse events or check if the position changed from last time.
		// To be safe and extremely accurate, let's store an event on *every*
		// hook callback. You could filter further if desired.
		x := msStruct.Point.X
		y := msStruct.Point.Y

		// Compute delta time
		now := time.Now()

		mtx.Lock()
		delta := now.Sub(lastEventTime)
		lastEventTime = now

		recordedData = append(recordedData, MouseRecord{
			DeltaMS: delta.Milliseconds(),
			X:       x,
			Y:       y,
		})
		mtx.Unlock()
	}

	ret, _, _ := procCallNextHookEx.Call(0, uintptr(code), wparam, lparam)
	return ret
}

// -----------------------------------------------------------------------------
// Main
// -----------------------------------------------------------------------------

func main() {
	// Install hooks
	err := installHooks()
	if err != nil {
		fmt.Println("[ERROR] Could not install hooks:", err)
		return
	}
	defer unInstallHooks()

	fmt.Println("=======================================================")
	fmt.Println(" Mouse Recorder & Replayer (Go + WinAPI, no extra deps)")
	fmt.Println("=======================================================")
	fmt.Println(" Press INSERT to start recording mouse movements.")
	fmt.Println(" Press END to stop recording, save to file, and replay.")
	fmt.Println(" Close this console or press Ctrl+C to exit.")
	fmt.Println()

	// Message loop so hooks actually function
	runMessageLoop()
}

// -----------------------------------------------------------------------------
// Helper Functions
// -----------------------------------------------------------------------------

func installHooks() error {
	// Keyboard hook
	hk, _, err := procSetWindowsHookExW.Call(
		uintptr(WH_KEYBOARD_LL),
		syscall.NewCallback(keyboardHookProc),
		0,
		0,
	)
	if hk == 0 {
		return fmt.Errorf("SetWindowsHookExW WH_KEYBOARD_LL failed: %v", err)
	}
	hKeyboardHook = syscall.Handle(hk)

	// Mouse hook
	hm, _, err := procSetWindowsHookExW.Call(
		uintptr(WH_MOUSE_LL),
		syscall.NewCallback(mouseHookProc),
		0,
		0,
	)
	if hm == 0 {
		return fmt.Errorf("SetWindowsHookExW WH_MOUSE_LL failed: %v", err)
	}
	hMouseHook = syscall.Handle(hm)

	return nil
}

func unInstallHooks() {
	if hKeyboardHook != 0 {
		procUnhookWindowsHookEx.Call(uintptr(hKeyboardHook))
		hKeyboardHook = 0
	}
	if hMouseHook != 0 {
		procUnhookWindowsHookEx.Call(uintptr(hMouseHook))
		hMouseHook = 0
	}
}

func runMessageLoop() {
	var msg MSG
	for {
		r, _, _ := procGetMessageW.Call(
			uintptr(unsafe.Pointer(&msg)),
			0,
			0,
			0,
		)

		// If r == 0, WM_QUIT received -> exit
		if r == 0 {
			break
		}
		// Otherwise, do default message translation & dispatch
		// In a typical Win32 program you'd call TranslateMessage, DispatchMessage, etc.
	}
}

// dumpToFile writes recorded mouse events to the given filename in JSON format.
func dumpToFile(filename string, data []MouseRecord) error {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(filename, b, 0644)
}

// replayFromFile reads the mouse events from filename, then replays them.
func replayFromFile(filename string) error {
	b, err := ioutil.ReadFile(filename)
	if err != nil {
		return err
	}

	var records []MouseRecord
	err = json.Unmarshal(b, &records)
	if err != nil {
		return err
	}

	// Now replay
	// We assume each record has DeltaMS and X, Y
	for i, rec := range records {
		if i == 0 {
			// First event, no need to sleep
		} else {
			// Sleep the delta from the previous event
			time.Sleep(time.Duration(rec.DeltaMS) * time.Millisecond)
		}

		// Move mouse
		setCursorPos(int(rec.X), int(rec.Y))
	}

	return nil
}

// setCursorPos calls the Windows API SetCursorPos(x, y).
func setCursorPos(x, y int) {
	procSetCursorPos.Call(uintptr(x), uintptr(y))
}
