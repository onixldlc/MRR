// +build windows

package main

import (
    "encoding/json"
    "fmt"
    "io/ioutil"
    "os"
    "sync"
    "syscall"
    "time"
    "unsafe"
)

// ------------------------------------------
// 1) EXTRA STRUCTS/CONSTS FOR SendInput
// ------------------------------------------
const (
    INPUT_MOUSE = 0

    // For mouse_event style flags:
    MOUSEEVENTF_XDOWN = 0x0080
    MOUSEEVENTF_XUP   = 0x0100

    // For XBUTTON1 (Mouse4) and XBUTTON2 (Mouse5):
    XBUTTON1 = 0x0001
    XBUTTON2 = 0x0002
)

type MOUSEINPUT struct {
    Dx          int32
    Dy          int32
    MouseData   uint32
    DwFlags     uint32
    Time        uint32
    DwExtraInfo uintptr
}

type INPUT struct {
    Type uint32
    Mi   MOUSEINPUT
}

var (
    user32   = syscall.MustLoadDLL("user32.dll")
    kernel32 = syscall.MustLoadDLL("kernel32.dll")

    // Hooks
    procSetWindowsHookExW   = user32.MustFindProc("SetWindowsHookExW")
    procCallNextHookEx      = user32.MustFindProc("CallNextHookEx")
    procGetMessageW         = user32.MustFindProc("GetMessageW")
    procUnhookWindowsHookEx = user32.MustFindProc("UnhookWindowsHookEx")
    procSetCursorPos        = user32.MustFindProc("SetCursorPos")
    procMouseEvent          = user32.MustFindProc("mouse_event")

    // NEW: We import SendInput
    procSendInput = user32.MustFindProc("SendInput")
)

// Original constants
const (
    recordFileName = "recorded-mice.cfg"

    WH_KEYBOARD_LL = 13
    WH_MOUSE_LL    = 14

    WM_KEYDOWN    = 0x0100
    WM_SYSKEYDOWN = 0x0104

    VK_INSERT = 0x2D
    VK_END    = 0x23

    WM_QUIT = 0x0012

    WM_LBUTTONDOWN = 0x0201
    WM_LBUTTONUP   = 0x0202
    WM_RBUTTONDOWN = 0x0204
    WM_RBUTTONUP   = 0x0205
    WM_MOUSEWHEEL  = 0x020A
    WM_XBUTTONDOWN = 0x020B
    WM_XBUTTONUP   = 0x020C
)

type KBDLLHOOKSTRUCT struct {
    VKCode    uint32
    ScanCode  uint32
    Flags     uint32
    Time      uint32
    ExtraInfo uintptr
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
    hKeyboardHook syscall.Handle
    hMouseHook    syscall.Handle

    mtx           sync.Mutex
    isRecording   bool
    recordedData  []MouseRecord
    lastEventTime time.Time

    recordingStarted = false
)

// NEW: We'll add a global debugMode
var debugMode bool

type MouseRecord struct {
    DeltaMS int64  `json:"DeltaMS"`
    X       int32  `json:"X"`
    Y       int32  `json:"Y"`
    Event   string `json:"Event"`
    Data    int32  `json:"Data"`
}

// ------------------------------------------------------------------
// 2) HELPER FUNCTION: SendInput for XBUTTON (Mouse4, Mouse5)
// ------------------------------------------------------------------
func sendXButtonInput(flags, xbutton uint32) {
    var inp INPUT
    inp.Type = INPUT_MOUSE
    inp.Mi = MOUSEINPUT{
        Dx:         0,
        Dy:         0,
        MouseData:  xbutton, // 1 for XBUTTON1, 2 for XBUTTON2
        DwFlags:    flags,   // MOUSEEVENTF_XDOWN or MOUSEEVENTF_XUP
        Time:       0,
        DwExtraInfo: 0,
    }

    procSendInput.Call(
        1,
        uintptr(unsafe.Pointer(&inp)),
        uintptr(unsafe.Sizeof(inp)),
    )
}

// ------------------------------------------------------------------
//     HELPER DEBUG PRINT FUNCTIONS
// ------------------------------------------------------------------
func debugPrintln(a ...interface{}) {
    if debugMode {
        fmt.Println(a...)
    }
}

func debugPrintf(format string, a ...interface{}) {
    if debugMode {
        fmt.Printf(format, a...)
    }
}

// ------------------------------------------
//          HOOK CALLBACKS
// ------------------------------------------
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

    msStruct := (*MSLLHOOKSTRUCT)(unsafe.Pointer(lparam))
    x := msStruct.Point.X
    y := msStruct.Point.Y

    // Extract high word for XBUTTON ID: 1 == XBUTTON1, 2 == XBUTTON2
    mouseData := (msStruct.MouseData >> 16) & 0xFFFF
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
    case WM_XBUTTONDOWN:
        if mouseData == XBUTTON1 {
            event = "Mouse4Down"
        } else if mouseData == XBUTTON2 {
            event = "Mouse5Down"
        }
    case WM_XBUTTONUP:
        if mouseData == XBUTTON1 {
            event = "Mouse4Up"
        } else if mouseData == XBUTTON2 {
            event = "Mouse5Up"
        }
    default:
        event = "MouseMove"
    }

    // Print debug only if --debug
    debugPrintf("Detected event: %s, X: %d, Y: %d, Data: %d\n", event, x, y, mouseData)

    if rec {
        now := time.Now()
        mtx.Lock()
        delta := now.Sub(lastEventTime)
        lastEventTime = now

        recordedData = append(recordedData, MouseRecord{
            DeltaMS: delta.Milliseconds(),
            X:       x,
            Y:       y,
            Event:   event,
            Data:    int32(mouseData),
        })
        mtx.Unlock()
    }

    ret, _, _ := procCallNextHookEx.Call(0, uintptr(code), wparam, lparam)
    return ret
}

func main() {
    // Check for --debug in args
    for _, arg := range os.Args[1:] {
        if arg == "--debug" {
            debugMode = true
            break
        }
    }

    err := installHooks()
    if err != nil {
        fmt.Println("[ERROR] Could not install hooks:", err)
        return
    }
    defer unInstallHooks()

    // Always show instructions to user
    fmt.Println("=======================================================")
    fmt.Println(" Mouse Recorder & Replayer (Modified)")
    fmt.Println("=======================================================")
    fmt.Println(" Press INSERT to toggle recording.")
    fmt.Println(" Press END to replay recorded movements.")
    fmt.Println(" Close this console or press Ctrl+C to exit.")
    fmt.Println()
    fmt.Println(" Run with --debug to see verbose logs.")

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

// ------------------------------------------
//        Save/Load Recorded Data
// ------------------------------------------
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

// ------------------------------------------
//     3) Updated sendMouseEvent
// ------------------------------------------
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
        procMouseEvent.Call(uintptr(0x0800), 0, 0, uintptr(data), 0)

    case "Mouse4Down":
        sendXButtonInput(MOUSEEVENTF_XDOWN, XBUTTON1)
    case "Mouse4Up":
        sendXButtonInput(MOUSEEVENTF_XUP, XBUTTON1)

    case "Mouse5Down":
        sendXButtonInput(MOUSEEVENTF_XDOWN, XBUTTON2)
    case "Mouse5Up":
        sendXButtonInput(MOUSEEVENTF_XUP, XBUTTON2)

    default:
        // e.g. "MouseMove" or others not replayed
    }
}
