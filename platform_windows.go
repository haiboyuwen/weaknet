//go:build windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const createNoWindow = 0x08000000

// hideWindow 调用 adb 时不弹出命令行窗口
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

func openBrowser(url string) {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	hideWindow(cmd)
	_ = cmd.Start()
}

// prepareConsole 关闭控制台的「快速编辑模式」：开启时在窗口里点一下或选中文字就会暂停程序的输出，
// 写日志的请求转发和控制台接口都会跟着卡住，看起来像页面卡顿。
func prepareConsole() {
	const enableQuickEdit, enableExtendedFlags = 0x0040, 0x0080
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getMode, setMode := kernel32.NewProc("GetConsoleMode"), kernel32.NewProc("SetConsoleMode")
	handle, err := syscall.GetStdHandle(syscall.STD_INPUT_HANDLE)
	if err != nil {
		return
	}
	var mode uint32
	if ok, _, _ := getMode.Call(uintptr(handle), uintptr(unsafe.Pointer(&mode))); ok == 0 {
		return
	}
	setMode.Call(uintptr(handle), uintptr(mode&^enableQuickEdit|enableExtendedFlags))
}

// waitBeforeExit 双击运行出错时，避免窗口一闪而过看不到错误
func waitBeforeExit() {
	fmt.Println("\n按回车键退出...")
	bufio.NewReader(os.Stdin).ReadString('\n')
}
