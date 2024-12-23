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

const (
	recordFileName = "recorded-mice.cfg"

	WH_KEYBOARD_LL = 13
	WH_MOUSE_LL    = 14

	WM_KEYDOWN   = 0x0100
	WM_SYSKEYDOWN = 0x0104

	VK_INSERT = 0x2D
	VK_END    = 0x23

	WM_QUIT = 0x0012

	WM_LBUTTONDOWN = 0x0201
	WM_LBUTTONUP   = 0x0202
	WM_RBUTTONDOWN = 0x0204
	WM_RBUTTONUP   = 0x0205
	WM_MOUSEWHEEL  = 0x020A
)

type KBDLLHOOKSTRUCT struct {
	VKCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	ExtraInfo   uintptr
}

type MSLLHOOKSTRUCT struct {
	Point     POINT
	MouseData uint32
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

type POINT struct {
	X int32
	Y int32
}

type MSG struct {
	HWND    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      POINT
}

var (
	user32              = syscall.MustLoadDLL("user32.dll")
	kernel32            = syscall.MustLoadDLL("kernel32.dll")

	procSetWindowsHookExW = user32.MustFindProc("SetWindowsHookExW")
	procCallNextHookEx    = user32.MustFindProc("CallNextHookEx")
	procGetMessageW       = user32.MustFindProc("GetMessageW")
	procSetCursorPos      = user32.MustFindProc("SetCursorPos")
	procMouseEvent        = user32.MustFindProc("mouse_event")
	procUnhookWindowsHookEx = user32.MustFindProc("UnhookWindowsHookEx")

	hKeyboardHook syscall.Handle
	hMouseHook    syscall.Handle

	mtx          sync.Mutex
	isRecording  bool
	recordedData []MouseRecord
	lastEventTime time.Time
)

type MouseRecord struct {
	DeltaMS int64  `json:"DeltaMS"`
	X       int32  `json:"X"`
	Y       int32  `json:"Y"`
	Event   string `json:"Event"`
	Data    int32  `json:"Data"`
}

var (
	recordingStarted = false
)

func keyboardHookProc(code int, wparam uintptr, lparam uintptr) uintptr {
	if code < 0 {
		ret, _, _ := procCallNextHookEx.Call(0, uintptr(code), wparam, lparam)
		return ret
	}

	if wparam == WM_KEYDOWN || wparam == WM_SYSKEYDOWN {
		kbStruct := (*KBDLLHOOKSTRUCT)(unsafe.Pointer(lparam))
		switch kbStruct.VKCode {
		case VK_INSERT:
			mtx.Lock()
			if recordingStarted {
				isRecording = false
				recordingStarted = false
				fmt.Println("[INFO] Insert key pressed -> Stop recording")
				dumpToFile(recordFileName, recordedData)
			} else {
				isRecording = true
				recordingStarted = true
				recordedData = make([]MouseRecord, 0)
				lastEventTime = time.Now()
				fmt.Println("[INFO] Insert key pressed -> Start recording")
			}
			mtx.Unlock()

		case VK_END:
			fmt.Println("[INFO] End key pressed -> Replaying recorded movements")
			if err := replayFromFile(recordFileName); err != nil {
				fmt.Println("[ERROR] Replay failed:", err)
			} else {
				fmt.Println("[INFO] Replay completed.")
			}
		}
	}

	ret, _, _ := procCallNextHookEx.Call(0, uintptr(code), wparam, lparam)
	return ret
}

func mouseHookProc(code int, wparam uintptr, lparam uintptr) uintptr {
	if code < 0 {
		ret, _, _ := procCallNextHookEx.Call(0, uintptr(code), wparam, lparam)
		return ret
	}

	mtx.Lock()
	rec := isRecording
	mtx.Unlock()

	if rec {
		msStruct := (*MSLLHOOKSTRUCT)(unsafe.Pointer(lparam))
		x := msStruct.Point.X
		y := msStruct.Point.Y
		mouseData := int32(msStruct.MouseData) >> 16 // Extract signed scroll data
		event := ""

		switch wparam {
		case WM_LBUTTONDOWN:
			event = "LeftButtonDown"
		case WM_LBUTTONUP:
			event = "LeftButtonUp"
		case WM_RBUTTONDOWN:
			event = "RightButtonDown"
		case WM_RBUTTONUP:
			event = "RightButtonUp"
		case WM_MOUSEWHEEL:
			event = "MouseWheel"
		default:
			event = "MouseMove"
		}

		now := time.Now()

		mtx.Lock()
		delta := now.Sub(lastEventTime)
		lastEventTime = now

		recordedData = append(recordedData, MouseRecord{
			DeltaMS: delta.Milliseconds(),
			X:       x,
			Y:       y,
			Event:   event,
			Data:    mouseData,
		})
		mtx.Unlock()
	}

	ret, _, _ := procCallNextHookEx.Call(0, uintptr(code), wparam, lparam)
	return ret
}

func main() {
	err := installHooks()
	if err != nil {
		fmt.Println("[ERROR] Could not install hooks:", err)
		return
	}
	defer unInstallHooks()

	fmt.Println("=======================================================")
	fmt.Println(" Mouse Recorder & Replayer (Modified)")
	fmt.Println("=======================================================")
	fmt.Println(" Press INSERT to toggle recording.")
	fmt.Println(" Press END to replay recorded movements.")
	fmt.Println(" Close this console or press Ctrl+C to exit.")
	fmt.Println()

	runMessageLoop()
}

func installHooks() error {
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

		if r == 0 {
			break
		}
	}
}

func dumpToFile(filename string, data []MouseRecord) error {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(filename, b, 0644)
}

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

	for i, rec := range records {
		if i != 0 {
			time.Sleep(time.Duration(rec.DeltaMS) * time.Millisecond)
		}
		setCursorPos(int(rec.X), int(rec.Y))
		sendMouseEvent(rec.Event, rec.Data)
	}

	return nil
}

func setCursorPos(x, y int) {
	procSetCursorPos.Call(uintptr(x), uintptr(y))
}

func sendMouseEvent(event string, data int32) {
	switch event {
	case "LeftButtonDown":
		procMouseEvent.Call(0x02, 0, 0, 0, 0)
	case "LeftButtonUp":
		procMouseEvent.Call(0x04, 0, 0, 0, 0)
	case "RightButtonDown":
		procMouseEvent.Call(0x08, 0, 0, 0, 0)
	case "RightButtonUp":
		procMouseEvent.Call(0x10, 0, 0, 0, 0)
	case "MouseWheel":
		procMouseEvent.Call(0x0800, 0, 0, uintptr(data), 0)
	}
}
