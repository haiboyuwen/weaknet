package main

import (
	"fmt"
	"net"
	"strings"
)

// 局域网模式：代理只监听电脑上与手机同网段的那块网卡，并且只接受这台手机的 IP，
// 手机系统代理指向电脑的局域网地址，不使用 adb reverse 和手机端中转程序，因此无法按 App 分类。

const (
	modeUSB = "usb"
	modeLAN = "lan"
)

// parseWifiAddr 从 `ip -4 -o addr show` 的输出里取出手机 Wi-Fi 网卡（wlan*）的 IPv4 地址和网段。
func parseWifiAddr(out string) (net.IP, *net.IPNet, error) {
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || !strings.HasPrefix(strings.TrimSuffix(f[1], ":"), "wlan") {
			continue
		}
		for i := 2; i+1 < len(f); i++ {
			if f[i] != "inet" {
				continue
			}
			ip, ipnet, err := net.ParseCIDR(f[i+1])
			if err == nil && ip.To4() != nil {
				return ip.To4(), ipnet, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("手机没有连接 Wi-Fi，局域网模式需要手机和电脑在同一个 Wi-Fi / 局域网")
}

// pickLocalIP 在电脑的网卡地址里找一个落在手机网段内的 IPv4 地址。
func pickLocalIP(addrs []net.Addr, phone *net.IPNet) (net.IP, error) {
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := n.IP.To4()
		if ip != nil && !ip.IsLoopback() && phone.Contains(ip) {
			return ip, nil
		}
	}
	return nil, fmt.Errorf("电脑没有处于手机所在网段（%s）的网卡，请让电脑和手机连同一个 Wi-Fi / 局域网", phone)
}

func localAddrs() []net.Addr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var all []net.Addr
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 {
			continue
		}
		if addrs, err := i.Addrs(); err == nil {
			all = append(all, addrs...)
		}
	}
	return all
}

// lanTarget 解析手机 Wi-Fi 地址并选出电脑端地址。
func (a *App) lanTarget(serial string) (phone, local net.IP, err error) {
	out, err := a.runAdb(serial, "shell", "ip", "-4", "-o", "addr", "show")
	if err != nil {
		return nil, nil, fmt.Errorf("读取手机网络信息失败：%v", err)
	}
	phone, subnet, err := parseWifiAddr(out)
	if err != nil {
		return nil, nil, err
	}
	local, err = pickLocalIP(localAddrs(), subnet)
	return phone, local, err
}

// allows 局域网车道只接受目标手机的连接，避免把代理暴露给同网络的其他设备。
func (l *Lane) allows(remote net.Addr) bool {
	if l.allowIP == nil {
		return true
	}
	t, ok := remote.(*net.TCPAddr)
	return ok && t.IP.Equal(l.allowIP)
}

func (l *Lane) mode() string {
	if l.allowIP != nil {
		return modeLAN
	}
	return modeUSB
}
