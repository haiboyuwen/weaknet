package main

import (
	"net"
	"testing"
	"time"
)

func TestParseWifiAddr(t *testing.T) {
	out := "2: rmnet_data1    inet 10.64.1.2/30 scope global rmnet_data1\\\n" +
		"45: wlan0    inet 192.168.31.5/24 brd 192.168.31.255 scope global wlan0\\       valid_lft forever preferred_lft forever\n"
	ip, subnet, err := parseWifiAddr(out)
	if err != nil || ip.String() != "192.168.31.5" || subnet.String() != "192.168.31.0/24" {
		t.Fatalf("parse: %v %v %v", ip, subnet, err)
	}
	if _, _, err := parseWifiAddr("2: rmnet_data1    inet 10.64.1.2/30 scope global rmnet_data1\n"); err == nil {
		t.Fatal("没有 Wi-Fi 时应报错")
	}
}

func TestPickLocalIPOnlyMatchingSubnet(t *testing.T) {
	_, phone, _ := net.ParseCIDR("192.168.31.5/24")
	addrs := []net.Addr{
		&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
		&net.IPNet{IP: net.ParseIP("10.0.0.8"), Mask: net.CIDRMask(24, 32)},
		&net.IPNet{IP: net.ParseIP("192.168.31.20"), Mask: net.CIDRMask(24, 32)},
	}
	ip, err := pickLocalIP(addrs, phone)
	if err != nil || ip.String() != "192.168.31.20" {
		t.Fatalf("pick: %v %v", ip, err)
	}
	if _, err := pickLocalIP(addrs[:2], phone); err == nil {
		t.Fatal("不在同一网段时应报错")
	}
}

func TestLANLaneOnlyAcceptsThePhone(t *testing.T) {
	l := newLane(newTestApp(t, "unlimited"), "phone", "Phone", 0)
	if l.mode() != modeUSB || !l.allows(&net.TCPAddr{IP: net.ParseIP("1.2.3.4")}) {
		t.Fatal("USB 车道不限制来源")
	}
	l.allowIP = net.ParseIP("192.168.31.5")
	if l.mode() != modeLAN || !l.allows(&net.TCPAddr{IP: net.ParseIP("192.168.31.5")}) || l.allows(&net.TCPAddr{IP: net.ParseIP("192.168.31.6")}) {
		t.Fatal("局域网车道只应接受目标手机")
	}
}

func TestLANLaneRejectsOtherClients(t *testing.T) {
	l := newLane(newTestApp(t, "unlimited"), "phone", "Phone", 0)
	l.listenIP, l.allowIP = net.ParseIP("127.0.0.1"), net.ParseIP("192.0.2.9")
	if err := l.start(); err != nil {
		t.Fatal(err)
	}
	defer l.stop()
	c, err := net.Dial("tcp", l.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n"))
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := c.Read(make([]byte, 16)); n != 0 || err == nil {
		t.Fatalf("非目标手机的连接应被关闭: %d %v", n, err)
	}
}

func TestSwitchModeIsNoopForSameModeAndCancelsResume(t *testing.T) {
	a := newTestApp(t, "unlimited")
	lane := newLane(a, "phone", "Phone", 0)
	a.lanes["phone"] = lane
	a.resume["phone"] = &resumeState{mode: modeLAN}
	a.switchMode("phone", modeUSB) // 已是 USB 模式：不重复启用
	if a.lanes["phone"] != lane {
		t.Fatal("同一模式不应重建通道")
	}
	if _, pending := a.resume["phone"]; pending {
		t.Fatal("用户主动启用应取消等待中的自动恢复")
	}
}
