package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	_ "embed"

	"github.com/getlantern/systray"
	"golang.org/x/sys/windows"
)

//go:embed icon.ico
var iconData []byte

var isEnabled bool = false
var currentOverlay atomic.Uintptr

var (
	user32                     = syscall.NewLazyDLL("user32.dll")
	getAsyncKeyState           = user32.NewProc("GetAsyncKeyState")
	createWindowExW            = user32.NewProc("CreateWindowExW")
	destroyWindow              = user32.NewProc("DestroyWindow")
	getMessageW                = user32.NewProc("GetMessageW")
	translateMessage           = user32.NewProc("TranslateMessage")
	dispatchMessageW           = user32.NewProc("DispatchMessageW")
	postMessageW               = user32.NewProc("PostMessageW")
	registerClassExW           = user32.NewProc("RegisterClassExW")
	defWindowProcW             = user32.NewProc("DefWindowProcW")
	beginPaint                 = user32.NewProc("BeginPaint")
	endPaint                   = user32.NewProc("EndPaint")
	getClientRect              = user32.NewProc("GetClientRect")
	fillRect                   = user32.NewProc("FillRect")
	drawTextW                  = user32.NewProc("DrawTextW")
	setLayeredWindowAttributes = user32.NewProc("SetLayeredWindowAttributes")

	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	getModuleHandleW = kernel32.NewProc("GetModuleHandleW")

	gdi32            = syscall.NewLazyDLL("gdi32.dll")
	createFontW      = gdi32.NewProc("CreateFontW")
	deleteObject     = gdi32.NewProc("DeleteObject")
	setBkMode        = gdi32.NewProc("SetBkMode")
	setTextColor     = gdi32.NewProc("SetTextColor")
	selectObject     = gdi32.NewProc("SelectObject")
	createSolidBrush = gdi32.NewProc("CreateSolidBrush")
)

type MSG struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      [2]int32
}

type PAINTSTRUCT struct {
	Hdc         uintptr
	FErase      uint32
	RcPaint     [4]int32
	FRestore    uint32
	FIncUpdate  uint32
	RgbReserved [32]byte
}

type WNDCLASSEX struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

const (
	WS_POPUP         = 0x80000000
	WS_VISIBLE       = 0x10000000
	WS_EX_TOPMOST    = 0x00000008
	WS_EX_TOOLWINDOW = 0x00000080
	WS_EX_NOACTIVATE = 0x08000000
	WS_EX_LAYERED    = 0x00080000
	WM_PAINT         = 0x000F
	WM_ERASEBKGND    = 0x0014
	WM_CLOSE         = 0x0010
	LWA_COLORKEY     = 0x1
	DT_SINGLELINE    = 0x0020
	DT_VCENTER       = 0x0004
	TRANSPARENT      = 1

	// color key: pure magenta — painted as background then made transparent
	// so text appears to float with no box behind it
	overlayColorKey = uintptr(0x00FF00FF)

	// text color: yellow (COLORREF = 0x00BBGGRR)
	overlayTextColor = uintptr(0x00946900)
)

var (
	overlayText         string
	overlayFont         uintptr
	overlayClsName      *uint16
	overlayRegisterOnce sync.Once
	overlayWndProcPtr   uintptr
)

func overlayWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch msg {
	case WM_ERASEBKGND:
		// fill background with color key — SetLayeredWindowAttributes makes it transparent
		var rc [4]int32
		getClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
		hBrush, _, _ := createSolidBrush.Call(overlayColorKey)
		fillRect.Call(wParam, uintptr(unsafe.Pointer(&rc)), hBrush)
		deleteObject.Call(hBrush)
		return 1

	case WM_PAINT:
		var ps PAINTSTRUCT
		hdc, _, _ := beginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		setBkMode.Call(hdc, TRANSPARENT)
		setTextColor.Call(hdc, overlayTextColor)
		if overlayFont != 0 {
			selectObject.Call(hdc, overlayFont)
		}
		var rc [4]int32
		getClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
		textPtr, _ := windows.UTF16PtrFromString(overlayText)
		drawTextW.Call(hdc, uintptr(unsafe.Pointer(textPtr)), ^uintptr(0),
			uintptr(unsafe.Pointer(&rc)), DT_SINGLELINE|DT_VCENTER)
		endPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	}

	ret, _, _ := defWindowProcW.Call(hwnd, msg, wParam, lParam)
	return ret
}

func registerOverlayClass() {
	overlayRegisterOnce.Do(func() {
		overlayClsName, _ = windows.UTF16PtrFromString("GTAOverlay")
		overlayWndProcPtr = windows.NewCallback(overlayWndProc)
		hInst, _, _ := getModuleHandleW.Call(0)
		wcex := WNDCLASSEX{
			LpfnWndProc:   overlayWndProcPtr,
			HInstance:     hInst,
			LpszClassName: overlayClsName,
		}
		wcex.CbSize = uint32(unsafe.Sizeof(wcex))
		registerClassExW.Call(uintptr(unsafe.Pointer(&wcex)))
	})
}

// closeOverlay closes the current overlay if one is open.
func closeOverlay() {
	if hwnd := currentOverlay.Load(); hwnd != 0 {
		postMessageW.Call(hwnd, WM_CLOSE, 0, 0)
	}
}

// showOverlayFor shows a floating text overlay at (x, y) for duration.
// Pass duration <= 0 for infinite (stays until closeOverlay is called).
func showOverlayFor(text string, x, y int, duration time.Duration) {
	closeOverlay()
	registerOverlayClass()

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		// create font
		fontName, _ := windows.UTF16PtrFromString("Arial")
		hFont, _, _ := createFontW.Call(
			20, 0, 0, 0,
			700,     // bold
			0, 0, 0, // italic, underline, strikeout
			1, 0, 0, 3, 0, // 3 = NONANTIALIASED_QUALITY
			uintptr(unsafe.Pointer(fontName)),
		)
		defer deleteObject.Call(hFont)

		// set globals for WndProc
		overlayText = text
		overlayFont = hFont

		hwnd, _, _ := createWindowExW.Call(
			WS_EX_TOPMOST|WS_EX_TOOLWINDOW|WS_EX_NOACTIVATE|WS_EX_LAYERED,
			uintptr(unsafe.Pointer(overlayClsName)),
			0,
			WS_POPUP|WS_VISIBLE,
			uintptr(x), uintptr(y), 160, 28,
			0, 0, 0, 0,
		)
		currentOverlay.Store(hwnd)
		defer currentOverlay.Store(0)

		// make background color key transparent
		setLayeredWindowAttributes.Call(hwnd, overlayColorKey, 0, LWA_COLORKEY)

		if duration > 0 {
			time.AfterFunc(duration, func() {
				postMessageW.Call(hwnd, WM_CLOSE, 0, 0)
			})
		}

		var msg MSG
		for {
			ret, _, _ := getMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
			if int32(ret) <= 0 || msg.Message == WM_CLOSE {
				break
			}
			translateMessage.Call(uintptr(unsafe.Pointer(&msg)))
			dispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
		}

		destroyWindow.Call(hwnd)
	}()
}

const (
	VK_F9   = 0x78
	VK_F12  = 0x7B
	VK_CTRL = 0x11
)

func amAdmin() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

func createWinMessageBoxW(title string, message string) {
	messageBox := user32.NewProc("MessageBoxW")

	caption, _ := syscall.UTF16PtrFromString(title)
	text, _ := syscall.UTF16PtrFromString(message)

	const style = 0x00000010 // MB_ICONERROR

	messageBox.Call(
		0,
		uintptr(unsafe.Pointer(text)),
		uintptr(unsafe.Pointer(caption)),
		uintptr(style),
	)
}

func isKeyPressed(keys [2]int) bool {
	for _, vk := range keys {
		ret, _, _ := getAsyncKeyState.Call(uintptr(vk))
		if ret&0x8000 == 0 {
			return false
		}
	}
	return true
}

const RockstarIP = "192.81.241.171"

func runHidden(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Run()
}

var singleInstanceMutex windows.Handle

func checkSingleInstance() {
    name, _ := windows.UTF16PtrFromString("Global\\GTANosave")
    mu, err := windows.CreateMutex(nil, false, name)
    if err != nil || windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
        createWinMessageBoxW("Error", "Another instance is already running.")
        os.Exit(1)
    }
    singleInstanceMutex = mu
}

func enableFirewallRule() error {
	return runHidden("netsh", "advfirewall", "firewall", "add", "rule", "name=GTANosave", "dir=out", "action=block", "remoteip="+RockstarIP)
}

func disableFirewallRule() error {
	return runHidden("netsh", "advfirewall", "firewall", "delete", "rule", "name=GTANosave")
}

func main() {
	if !amAdmin() {
		fmt.Println("This program must be run as an administrator.")
		createWinMessageBoxW("Error", "This program must be run as an administrator.")
		os.Exit(1)
	}

	// single instance check
	checkSingleInstance()

	// ready to go
	// cleanup just in case
	disableFirewallRule()
	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetIcon(iconData)
	systray.SetTitle("El Rubio Robbing Machine 5000")
	systray.SetTooltip("GTA Online Nosave toggle")

	mExit := systray.AddMenuItem("Exit", "Exit the application")
	systray.AddSeparator()
	mStatus := systray.AddMenuItem("Status: Disabled", "Nosave toggle status")
	mStatus.Disable()

	go func() {
		<-mExit.ClickedCh
		systray.Quit()
	}()

	go func() {
		for {
			if isKeyPressed([2]int{VK_F9, VK_CTRL}) {
				if !isEnabled {
					err := enableFirewallRule()
					if err != nil {
						fmt.Println("Failed to enable firewall rule:", err)
						createWinMessageBoxW("Error", "Failed to enable firewall rule: "+err.Error())
					} else {
						isEnabled = true
						mStatus.SetTitle("Status: Enabled")
						showOverlayFor("Nosave Enabled", 10, 10, -1)
					}
				}
			} else if isKeyPressed([2]int{VK_F12, VK_CTRL}) {
				if isEnabled {
					err := disableFirewallRule()
					if err != nil {
						fmt.Println("Failed to disable firewall rule:", err)
						createWinMessageBoxW("Error", "Failed to disable firewall rule: "+err.Error())
					} else {
						isEnabled = false
						mStatus.SetTitle("Status: Disabled")
						showOverlayFor("Nosave Disabled", 10, 10, 3*time.Second)
					}
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
}

func onExit() {
	if isEnabled {
		err := disableFirewallRule()
		if err != nil {
			fmt.Println("Failed to disable firewall rule on exit:", err)
			createWinMessageBoxW("Error", "Failed to disable firewall rule on exit: "+err.Error())
		}
	}
}
