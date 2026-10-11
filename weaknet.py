#!/usr/bin/env python3
"""
弱网模拟器：HTTP/HTTPS 代理 + 网页控制台，参数可在运行中动态调整，支持同时连接多台 Android 手机。

原理：每台启用弱网的手机分配一个独立的代理端口（通过 adb reverse 走 USB），并设为手机的系统代理。
每台手机一条独立的瓶颈链路（上/下行各一个 FIFO 队列），按带宽串行发送，再叠加延迟、抖动和"丢包"
（模拟 TCP 重传造成的停顿）。手机之间带宽互不抢占，参数统一，便于在同一网络条件下对比不同 App。

只对使用系统 HTTP 代理的流量生效（绝大多数 App 的 HTTP/HTTPS/WebSocket）。
UDP（音视频 RTC、QUIC）和自带网络栈、忽略系统代理的 App 不受影响。

用法：
  python3 weaknet.py                          # 启动后在控制台里勾选要走弱网的手机
  python3 weaknet.py --device                 # 只连了一台手机时直接启用它
  python3 weaknet.py --serial R5CN...,emulator-5554 --preset 3g
  浏览器打开 http://127.0.0.1:8899
  Ctrl+C 退出时，本工具启用过的手机会自动恢复。

命令行调整：
  curl -X POST 127.0.0.1:8899/api/preset/3g
  curl -X POST 127.0.0.1:8899/api/config -d '{"down_kbps":300,"delay_ms":250,"loss_pct":2}'
  curl -X POST 127.0.0.1:8899/api/device/<serial>/on      # on | off | reconnect
"""
import argparse
import asyncio
import json
import os
import random
import shutil
import signal
import subprocess
import time
from collections import deque
from urllib.parse import unquote, urlsplit

PRESETS = {
    "unlimited": {"label": "不限速", "down_kbps": 0, "up_kbps": 0, "delay_ms": 0, "jitter_ms": 0, "loss_pct": 0},
    "4g": {"label": "4G", "down_kbps": 20000, "up_kbps": 5000, "delay_ms": 25, "jitter_ms": 10, "loss_pct": 0},
    "3g": {"label": "3G", "down_kbps": 1500, "up_kbps": 500, "delay_ms": 80, "jitter_ms": 30, "loss_pct": 0.5},
    "weak": {"label": "弱网", "down_kbps": 400, "up_kbps": 200, "delay_ms": 200, "jitter_ms": 50, "loss_pct": 1},
    "edge": {"label": "2G/EDGE", "down_kbps": 200, "up_kbps": 100, "delay_ms": 300, "jitter_ms": 100, "loss_pct": 2},
    "bad": {"label": "极弱网", "down_kbps": 80, "up_kbps": 40, "delay_ms": 500, "jitter_ms": 200, "loss_pct": 5},
}

NUMERIC_KEYS = ("down_kbps", "up_kbps", "delay_ms", "jitter_ms", "loss_pct", "window_kb")


def log(msg):
    print(time.strftime("%H:%M:%S"), msg, flush=True)


def close_quietly(writer):
    try:
        writer.close()
    except Exception:
        pass


class Config:
    """所有手机共用的一套弱网参数"""

    def __init__(self, preset, window_kb, allow):
        self.down_kbps = 0
        self.up_kbps = 0
        self.delay_ms = 0  # 单程延迟，RTT 约为两倍
        self.jitter_ms = 0
        self.loss_pct = 0.0
        self.window_kb = window_kb
        self.outage = ""  # "" 正常 | "stall" 卡死 | "reset" 断开
        self.allow = allow
        self.preset = ""
        self.apply_preset(preset)

    def apply_preset(self, name):
        p = PRESETS[name]
        for k in ("down_kbps", "up_kbps", "delay_ms", "jitter_ms", "loss_pct"):
            setattr(self, k, p[k])
        self.preset = name

    def update(self, data):
        for k in NUMERIC_KEYS:
            if k in data:
                setattr(self, k, max(0.0, float(data[k])))
        if "outage" in data and data["outage"] in ("", "stall", "reset"):
            self.outage = data["outage"]
        if "allow" in data:
            raw = data["allow"]
            items = raw if isinstance(raw, list) else str(raw).split(",")
            self.allow = [a.strip() for a in items if a.strip()]
        if any(k in data for k in ("down_kbps", "up_kbps", "delay_ms", "jitter_ms", "loss_pct")):
            self.preset = ""

    def to_dict(self):
        return {k: getattr(self, k) for k in NUMERIC_KEYS + ("outage", "allow", "preset")}


class Link:
    """一条单向瓶颈链路：按带宽串行发送，再交给连接按延迟投递。"""

    def __init__(self, cfg, key):
        self.cfg = cfg
        self.key = key  # "down_kbps" / "up_kbps"
        self.queue = asyncio.Queue()
        self.queued = 0
        self.sent = 0
        self.host_sent = {}
        self.host_queued = {}

    @property
    def rate(self):
        kbps = getattr(self.cfg, self.key)
        return kbps * 1000 / 8 if kbps > 0 else 0

    async def run(self):
        while True:
            pipe, data = await self.queue.get()
            while self.cfg.outage == "stall":
                await asyncio.sleep(0.1)
            rate = self.rate
            if data and rate:
                await asyncio.sleep(len(data) / rate)
            n = len(data)
            self.queued -= n
            self.sent += n
            self.host_sent[pipe.host] = self.host_sent.get(pipe.host, 0) + n
            self.host_queued[pipe.host] = self.host_queued.get(pipe.host, 0) - n
            pipe.schedule(data)


class Pipe:
    """一个连接的一个方向：reader -> link -> writer，保证同一连接内按序投递。"""

    def __init__(self, lane, link, writer, host, on_dead):
        self.lane = lane
        self.cfg = lane.cfg
        self.link = link
        self.writer = writer
        self.host = host
        self.on_dead = on_dead
        self.inflight = 0
        self.total = 0
        self.last_at = 0.0
        self.can_send = asyncio.Event()
        self.can_send.set()
        self.closed = False

    @property
    def window(self):
        return max(4096, int(self.cfg.window_kb * 1024))

    async def enqueue(self, data):
        n = len(data)
        self.inflight += n
        self.total += n
        self.link.queued += n
        self.link.host_queued[self.host] = self.link.host_queued.get(self.host, 0) + n
        if self.inflight >= self.window:
            self.can_send.clear()
        await self.link.queue.put((self, data))

    async def pump(self, reader):
        try:
            while not self.closed:
                await self.can_send.wait()
                data = await reader.read(4096)
                if not data:
                    break
                await self.enqueue(data)
        except Exception:
            pass
        await self.link.queue.put((self, b""))

    def schedule(self, data):
        loop = asyncio.get_running_loop()
        cfg = self.cfg
        delay = cfg.delay_ms / 1000
        if cfg.jitter_ms:
            delay += random.uniform(0, cfg.jitter_ms) / 1000
        if data and cfg.loss_pct and random.random() < cfg.loss_pct / 100:
            # TCP 之上无法真正丢包，这里用一次重传超时的停顿来模拟丢包的效果
            delay += max(0.2, cfg.delay_ms * 2 / 1000 * 2)
            self.lane.loss_events += 1
        # 延迟/抖动变化时不能乱序：投递时间严格递增（同时模拟了 TCP 的队头阻塞）。
        # 相同时刻的定时回调在 asyncio 中不保证先后顺序，所以至少比上一块晚 1 微秒
        at = max(loop.time() + delay, self.last_at + 1e-6)
        self.last_at = at
        loop.call_at(at, self.deliver, data)

    def deliver(self, data):
        if data:
            self.inflight -= len(data)
            if self.inflight < self.window:
                self.can_send.set()
        if self.closed:
            return
        if not data:
            self.closed = True
            close_quietly(self.writer)
            return
        if self.writer.is_closing():
            # 对端已关闭（如客户端取消下载）：关掉整个连接，不再往已断开的 socket 写，也让服务器停止发送
            self.closed = True
            self.on_dead()
            return
        try:
            self.writer.write(data)
        except Exception:
            self.closed = True
            self.on_dead()


class Lane:
    """一台手机的弱网通道：独立的代理端口和独立的上下行链路"""

    def __init__(self, app, serial, name, port):
        self.app = app
        self.cfg = app.cfg
        self.serial = serial
        self.name = name
        self.port = port
        self.up = Link(self.cfg, "up_kbps")
        self.down = Link(self.cfg, "down_kbps")
        self.active = set()
        self.loss_events = 0
        self.history = deque(maxlen=180)
        self.host_rates = {}
        self.server = None
        self.tasks = []

    async def start(self):
        self.server = await asyncio.start_server(self.handle, "127.0.0.1", self.port)
        self.tasks = [asyncio.ensure_future(t) for t in (self.up.run(), self.down.run(), self.sample())]

    def stop(self):
        if self.server:
            self.server.close()
        self.reset_all()
        for t in self.tasks:
            t.cancel()

    def event(self, msg):
        self.app.event(f"[{self.name}] {msg}")

    def reset_all(self):
        for client_w, up_w in list(self.active):
            close_quietly(client_w)
            close_quietly(up_w)
        self.active.clear()

    async def handle(self, client_r, client_w):
        if self.cfg.outage == "reset":
            close_quietly(client_w)
            return
        try:
            head = await asyncio.wait_for(client_r.readuntil(b"\r\n\r\n"), 30)
            method, target, _ = head.split(b"\r\n", 1)[0].decode("latin1").split(" ", 2)
        except Exception:
            close_quietly(client_w)
            return

        initial = b""
        if method == "CONNECT":
            host, port = target.rsplit(":", 1)
            port = int(port)
        else:
            u = urlsplit(target)
            host, port = u.hostname or "", u.port or 80
            path = (u.path or "/") + ("?" + u.query if u.query else "")
            initial = head.replace(target.encode("latin1"), path.encode("latin1"), 1)

        if not self.app.allowed(host):
            self.event(f"拦截 {host}")
            try:
                client_w.write(b"HTTP/1.1 403 Forbidden\r\n\r\n")
            except Exception:
                pass
            close_quietly(client_w)
            return

        try:
            addr = await self.app.resolve(host)
            up_r, up_w = await asyncio.wait_for(asyncio.open_connection(addr, port), 15)
        except Exception as e:
            self.event(f"连接失败 {host}:{port} {type(e).__name__}")
            try:
                client_w.write(b"HTTP/1.1 502 Bad Gateway\r\n\r\n")
            except Exception:
                pass
            close_quietly(client_w)
            return

        conn = (client_w, up_w)
        self.active.add(conn)
        start = time.time()
        # 近似 TCP 握手的一个 RTT
        await asyncio.sleep(self.cfg.delay_ms * 2 / 1000)

        def on_dead():
            close_quietly(client_w)
            close_quietly(up_w)

        up_pipe = Pipe(self, self.up, up_w, host, on_dead)
        down_pipe = Pipe(self, self.down, client_w, host, on_dead)
        if method == "CONNECT":
            client_w.write(b"HTTP/1.1 200 Connection Established\r\n\r\n")
        else:
            await up_pipe.enqueue(initial)

        await asyncio.gather(up_pipe.pump(client_r), down_pipe.pump(up_r))
        self.active.discard(conn)
        if down_pipe.total > 64 * 1024:
            self.event(f"{host} 下行 {down_pipe.total / 1024:.0f} KB，{time.time() - start:.1f} s")

    async def sample(self):
        last_up, last_down, last_host = 0, 0, {}
        windows = {}
        while True:
            await asyncio.sleep(1)
            up_rate = (self.up.sent - last_up) / 1024
            down_rate = (self.down.sent - last_down) / 1024
            last_up, last_down = self.up.sent, self.down.sent
            rate = self.down.rate
            qdown = max(0, self.down.queued) / rate if rate else 0
            self.history.append([round(time.time()), round(down_rate, 1), round(up_rate, 1), round(qdown, 2)])
            for h, total in self.down.host_sent.items():
                w = windows.setdefault(h, deque(maxlen=5))
                w.append(total - last_host.get(h, 0))
            last_host = dict(self.down.host_sent)
            self.host_rates = {h: sum(w) / len(w) / 1024 for h, w in windows.items() if w}

    def stats(self):
        rate = self.down.rate
        hosts = []
        for h, r in self.host_rates.items():
            q = max(0, self.down.host_queued.get(h, 0))
            if r > 0.05 or q > 1024:
                hosts.append({"host": h, "kbs": round(r, 1), "queued_kb": round(q / 1024)})
        hosts.sort(key=lambda x: -x["kbs"])
        return {
            "conns": len(self.active),
            "queue_down_kb": round(max(0, self.down.queued) / 1024),
            "queue_down_s": round(max(0, self.down.queued) / rate, 2) if rate else 0,
            "loss_events": self.loss_events,
            "history": list(self.history),
            "hosts": hosts[:12],
        }


class App:
    def __init__(self, cfg, dns, adb, base_port):
        self.cfg = cfg
        self.dns = dns
        self.adb = adb
        self.base_port = base_port
        self.lanes = {}  # serial -> Lane
        self.devices = {}  # serial -> {"serial", "model", "state", "proxy"}
        self.busy = {}  # serial -> action
        self.dns_cache = {}
        self.events = deque(maxlen=300)

    def event(self, msg):
        line = f"{time.strftime('%H:%M:%S')} {msg}"
        self.events.appendleft(line)
        log(msg)

    def allowed(self, host):
        allow = self.cfg.allow
        return not allow or any(host == a or host.endswith("." + a) for a in allow)

    async def resolve(self, host):
        """用指定 DNS 解析（绕开本机代理软件的 fake-ip），失败时退回系统解析。"""
        if not self.dns or not shutil.which("dig"):
            return host
        cached = self.dns_cache.get(host)
        if cached and cached[1] > time.time():
            return cached[0]
        try:
            proc = await asyncio.create_subprocess_exec(
                "dig", "+short", "+time=2", "+tries=1", f"@{self.dns}", host, "A",
                stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.DEVNULL)
            out, _ = await proc.communicate()
            ips = [l for l in out.decode().split() if l.count(".") == 3 and l.replace(".", "").isdigit()]
            if ips:
                self.dns_cache[host] = (ips[0], time.time() + 300)
                return ips[0]
        except Exception:
            pass
        return host

    # ---- adb ----

    def run_adb(self, serial, *args, timeout=20):
        cmd = [self.adb] + (["-s", serial] if serial else []) + list(args)
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
        return r.returncode, (r.stdout + r.stderr).strip()

    async def adb_async(self, serial, *args, timeout=20):
        return await asyncio.get_running_loop().run_in_executor(
            None, lambda: self.run_adb(serial, *args, timeout=timeout))

    async def refresh_devices(self):
        if not self.adb:
            return
        code, out = await self.adb_async(None, "devices", "-l", timeout=10)
        found = {}
        for line in out.splitlines()[1:]:
            parts = line.split()
            if len(parts) < 2:
                continue
            serial, state = parts[0], parts[1]
            model = next((p.split(":", 1)[1] for p in parts[2:] if p.startswith("model:")), serial)
            found[serial] = {"serial": serial, "model": model.replace("_", " "), "state": state,
                             "proxy": self.devices.get(serial, {}).get("proxy", "")}
        for serial, d in found.items():
            if d["state"] == "device":
                _, proxy = await self.adb_async(serial, "shell", "settings", "get", "global", "http_proxy", timeout=8)
                d["proxy"] = proxy
        gone = [s for s in self.lanes if s not in found]
        for s in gone:
            self.event(f"[{self.lanes[s].name}] 手机已断开 USB，停止弱网通道（手机上的代理设置需重新连接后关闭）")
            self.lanes.pop(s).stop()
        self.devices = found

    async def poll_devices(self):
        while True:
            try:
                await self.refresh_devices()
            except Exception as e:
                log(f"refresh devices failed: {e}")
            await asyncio.sleep(5)

    def next_port(self):
        used = {lane.port for lane in self.lanes.values()}
        port = self.base_port
        while port in used:
            port += 1
        return port

    async def enable(self, serial):
        if serial in self.lanes:
            return
        d = self.devices.get(serial)
        if not d or d["state"] != "device":
            self.event(f"[{serial}] 设备不可用（未连接或未授权调试）")
            return
        lane = Lane(self, serial, d["model"], self.next_port())
        try:
            await lane.start()
        except OSError as e:
            self.event(f"[{lane.name}] 端口 {lane.port} 启动失败：{e}")
            return
        code1, out1 = await self.adb_async(serial, "reverse", f"tcp:{lane.port}", f"tcp:{lane.port}")
        code2, out2 = await self.adb_async(serial, "shell", "settings", "put", "global", "http_proxy",
                                           f"127.0.0.1:{lane.port}")
        if code1 != 0 or code2 != 0:
            lane.stop()
            self.event(f"[{lane.name}] 启用失败：{out1 or out2}")
            return
        self.lanes[serial] = lane
        self.event(f"[{lane.name}] 已启用弱网，端口 {lane.port}")

    async def disable(self, serial):
        lane = self.lanes.pop(serial, None)
        port = lane.port if lane else None
        await self.adb_async(serial, "shell", "settings", "put", "global", "http_proxy", ":0")
        await self.adb_async(serial, "shell", "settings", "delete", "global", "http_proxy")
        if port:
            await self.adb_async(serial, "reverse", "--remove", f"tcp:{port}")
            lane.stop()
        name = lane.name if lane else self.devices.get(serial, {}).get("model", serial)
        self.event(f"[{name}] 已关闭弱网，系统代理已清除")

    async def reconnect(self, serial):
        """关开 WiFi，让已建立的连接断开后改走代理"""
        await self.adb_async(serial, "shell", "svc", "wifi", "disable")
        await asyncio.sleep(3)
        await self.adb_async(serial, "shell", "svc", "wifi", "enable")
        self.event(f"[{self.devices.get(serial, {}).get('model', serial)}] 已关开 WiFi")

    async def device_action(self, serial, action):
        if self.busy.get(serial):
            return
        fn = {"on": self.enable, "off": self.disable, "reconnect": self.reconnect}.get(action)
        if not fn:
            return
        self.busy[serial] = action
        try:
            await fn(serial)
        finally:
            self.busy.pop(serial, None)
            await self.refresh_devices()

    def reset_all(self):
        for lane in self.lanes.values():
            lane.reset_all()
        self.event("断开全部连接")

    # ---- 控制台 ----

    def state(self):
        devices = []
        for serial, d in sorted(self.devices.items()):
            lane = self.lanes.get(serial)
            devices.append(dict(d, enabled=bool(lane), port=lane.port if lane else None,
                                busy=self.busy.get(serial, ""),
                                foreign=bool(not lane and d.get("proxy") not in ("", "null", ":0"))))
        return {
            "config": self.cfg.to_dict(),
            "presets": {k: v["label"] for k, v in PRESETS.items()},
            "adb": bool(self.adb),
            "devices": devices,
            "lanes": [{"serial": s, "name": l.name, "port": l.port, "stats": l.stats()} for s, l in self.lanes.items()],
            "events": list(self.events)[:60],
        }

    async def handle_ui(self, reader, writer):
        try:
            head = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), 10)
            lines = head.decode("latin1").split("\r\n")
            method, path, _ = lines[0].split(" ", 2)
            length = 0
            for line in lines[1:]:
                if line.lower().startswith("content-length:"):
                    length = int(line.split(":", 1)[1])
            body = await reader.readexactly(length) if length else b""
            status, ctype, payload = await self.route(method, path, body)
        except Exception as e:
            status, ctype, payload = "400 Bad Request", "text/plain; charset=utf-8", str(e).encode()
        writer.write(f"HTTP/1.1 {status}\r\nContent-Type: {ctype}\r\nContent-Length: {len(payload)}\r\n"
                     "Cache-Control: no-store\r\nConnection: close\r\n\r\n".encode() + payload)
        try:
            await writer.drain()
        finally:
            close_quietly(writer)

    async def route(self, method, path, body):
        js = "application/json; charset=utf-8"
        ok = lambda obj: ("200 OK", js, json.dumps(obj, ensure_ascii=False).encode())
        if method == "GET" and path == "/":
            return "200 OK", "text/html; charset=utf-8", PAGE.encode()
        if method == "GET" and path == "/api/state":
            return ok(self.state())
        if method == "POST" and path == "/api/config":
            data = json.loads(body or b"{}")
            before = self.cfg.outage
            self.cfg.update(data)
            if self.cfg.outage == "reset" and before != "reset":
                self.reset_all()
            self.event(f"参数 {json.dumps(data, ensure_ascii=False)}")
            return ok(self.cfg.to_dict())
        if method == "POST" and path.startswith("/api/preset/"):
            name = path.rsplit("/", 1)[1]
            if name not in PRESETS:
                return "404 Not Found", js, b'{"error":"unknown preset"}'
            self.cfg.apply_preset(name)
            self.event(f"切换档位 {PRESETS[name]['label']}")
            return ok(self.cfg.to_dict())
        if method == "POST" and path.startswith("/api/device/"):
            parts = path.split("/")  # ['', 'api', 'device', serial, action]
            if len(parts) == 5:
                asyncio.ensure_future(self.device_action(unquote(parts[3]), parts[4]))
                return ok({"ok": True})
        return "404 Not Found", "text/plain", b"not found"


def find_adb(explicit):
    candidates = [explicit, shutil.which("adb")]
    for env in ("ANDROID_HOME", "ANDROID_SDK_ROOT"):
        if os.environ.get(env):
            candidates.append(os.path.join(os.environ[env], "platform-tools", "adb"))
    candidates += [os.path.expanduser("~/Developer/Sdk/Android/sdk/platform-tools/adb"),
                   os.path.expanduser("~/Library/Android/sdk/platform-tools/adb")]
    for c in candidates:
        if c and os.path.exists(c):
            return c
    return None


async def main():
    p = argparse.ArgumentParser(description="弱网模拟器")
    p.add_argument("--port", type=int, default=8888, help="代理起始端口，每台手机依次占用一个")
    p.add_argument("--ui-port", type=int, default=8899, help="控制台端口")
    p.add_argument("--preset", default="weak", choices=list(PRESETS))
    p.add_argument("--window-kb", type=float, default=128, help="每连接在途数据上限，近似 TCP 拥塞窗口")
    p.add_argument("--allow", default="", help="只放行这些域名后缀（逗号分隔），默认全部放行")
    p.add_argument("--dns", default="223.5.5.5", help="解析目标域名用的 DNS，留空用系统解析")
    p.add_argument("--device", action="store_true", help="只连了一台手机时，启动后直接对它启用弱网")
    p.add_argument("--serial", default="", help="启动后直接启用这些手机（逗号分隔的序列号）")
    p.add_argument("--adb", default="", help="adb 路径，默认自动查找")
    args = p.parse_args()

    cfg = Config(args.preset, args.window_kb, [a.strip() for a in args.allow.split(",") if a.strip()])
    adb = find_adb(args.adb)
    if not adb:
        log("找不到 adb，手机需要手动配置代理（--adb 可指定路径）")
    app = App(cfg, args.dns, adb, args.port)
    ui = await asyncio.start_server(app.handle_ui, "127.0.0.1", args.ui_port)

    loop = asyncio.get_running_loop()
    stop = loop.create_future()
    for sig in (signal.SIGINT, signal.SIGTERM):
        loop.add_signal_handler(sig, lambda: stop.done() or stop.set_result(None))

    await app.refresh_devices()
    online = [s for s, d in app.devices.items() if d["state"] == "device"]
    wanted = [s.strip() for s in args.serial.split(",") if s.strip()]
    if not wanted and args.device:
        if len(online) == 1:
            wanted = online
        elif len(online) > 1:
            log("连接了多台手机，不会自动选择，请用 --serial 指定或在控制台里启用：")
    for serial in wanted:
        await app.enable(serial)
    for s, d in app.devices.items():
        log(f"  {d['model']}  {s}  {d['state']}{'  [弱网]' if s in app.lanes else ''}")
    asyncio.ensure_future(app.poll_devices())

    log(f"档位 {PRESETS[args.preset]['label']}，控制台 http://127.0.0.1:{args.ui_port}")
    try:
        await stop
    finally:
        log("退出中，恢复已启用弱网的手机...")
        for serial in list(app.lanes):
            await app.disable(serial)
        ui.close()


PAGE = r"""<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>弱网模拟器</title>
<style>
:root{--bg:#f6f7f9;--card:#fff;--ink:#1d2129;--quiet:#6b7280;--line:#e3e6ea;--accent:#2f6fde;--warn:#d97706;--good:#16a34a}
@media (prefers-color-scheme:dark){:root{--bg:#121417;--card:#1b1e23;--ink:#e6e8eb;--quiet:#9aa1ab;--line:#2c3138;--accent:#5b8ff0}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.5 -apple-system,BlinkMacSystemFont,"PingFang SC",sans-serif}
main{max-width:1120px;margin:0 auto;padding:20px 16px;display:grid;gap:16px;grid-template-columns:minmax(0,380px) minmax(0,1fr)}
@media (max-width:860px){main{grid-template-columns:1fr}}
h1{font-size:18px;margin:0 0 4px}h2{font-size:14px;margin:0 0 12px;color:var(--quiet);font-weight:600}
.col{display:grid;gap:16px;align-content:start}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:16px}
.presets{display:grid;grid-template-columns:repeat(3,1fr);gap:8px}
button{font:inherit;border:1px solid var(--line);background:var(--card);color:var(--ink);border-radius:8px;padding:6px 10px;cursor:pointer}
button:hover{border-color:var(--accent)}button.on{background:var(--accent);border-color:var(--accent);color:#fff}
button:disabled{opacity:.5;cursor:default}
.field{display:grid;grid-template-columns:96px 1fr 72px;gap:8px;align-items:center;margin:10px 0}
.field label{color:var(--quiet)}.field input[type=range]{width:100%}
.field input[type=number]{width:72px;font:inherit;padding:4px 6px;border:1px solid var(--line);border-radius:6px;background:var(--card);color:var(--ink)}
.row{display:flex;gap:8px;flex-wrap:wrap}
textarea{width:100%;font:12px ui-monospace,monospace;padding:6px;border:1px solid var(--line);border-radius:6px;background:var(--card);color:var(--ink)}
.dev{border:1px solid var(--line);border-radius:8px;padding:10px;margin-bottom:8px}
.dev .t{display:flex;justify-content:space-between;gap:8px;align-items:baseline}
.dev b{font-size:14px}.dev small{color:var(--quiet);font:12px ui-monospace,monospace}
.tag{font-size:12px;border-radius:10px;padding:1px 8px;border:1px solid var(--line);color:var(--quiet);white-space:nowrap}
.tag.on{border-color:var(--good);color:var(--good)}.tag.warn{border-color:var(--warn);color:var(--warn)}
.dev .row{margin-top:8px}
.tabs{display:flex;gap:8px;flex-wrap:wrap;margin-bottom:12px}
.kpis{display:grid;grid-template-columns:repeat(4,1fr);gap:8px;margin-bottom:12px}
.kpi{border:1px solid var(--line);border-radius:8px;padding:8px 10px}.kpi b{display:block;font-size:20px}.kpi span{color:var(--quiet);font-size:12px}
canvas{width:100%;height:180px;display:block}
table{width:100%;border-collapse:collapse;font-size:13px}td,th{padding:5px 6px;border-bottom:1px solid var(--line);text-align:left}
th{color:var(--quiet);font-weight:500}td.n{text-align:right;font-variant-numeric:tabular-nums}
.bar{height:6px;background:var(--accent);border-radius:3px;opacity:.6}
.events{font:12px ui-monospace,monospace;color:var(--quiet);max-height:240px;overflow:auto;white-space:pre-wrap}
.status{font-size:12px;color:var(--quiet);margin-top:4px}.legend{font-size:12px;color:var(--quiet);margin-top:6px}
.legend i{display:inline-block;width:10px;height:3px;margin:0 4px 2px 10px;vertical-align:middle}
.empty{color:var(--quiet);font-size:13px}
</style></head><body><main>
<section class="col">
 <div class="card"><h1>弱网模拟器</h1><div class="status" id="summary"></div></div>
 <div class="card"><h2>手机（只有启用的手机走弱网，每台独立带宽）</h2><div id="devices"></div></div>
 <div class="card"><h2>档位（所有启用的手机共用）</h2><div class="presets" id="presets"></div></div>
 <div class="card"><h2>参数（拖动即时生效）</h2><div id="fields"></div>
  <h2 style="margin-top:16px">网络状态</h2>
  <div class="row" id="outage"><button data-o="">正常</button><button data-o="stall">卡死（信号极差）</button><button data-o="reset">断开（断网）</button></div>
  <h2 style="margin-top:16px">域名白名单（留空 = 全部放行）</h2>
  <textarea id="allow" rows="2" placeholder="例如 yallajalsat.com,yallajalsat.live"></textarea>
  <div class="row" style="margin-top:8px"><button id="saveAllow">保存白名单</button></div>
 </div>
</section>
<section class="col">
 <div class="card">
  <div class="tabs" id="tabs"></div>
  <div id="laneBody">
   <div class="kpis">
    <div class="kpi"><b id="kDown">-</b><span>下行 KB/s</span></div>
    <div class="kpi"><b id="kQueue">-</b><span>下行排队（秒）</span></div>
    <div class="kpi"><b id="kConns">-</b><span>活动连接</span></div>
    <div class="kpi"><b id="kLoss">-</b><span>丢包停顿次数</span></div>
   </div>
   <canvas id="chart"></canvas>
   <div class="legend"><i style="background:var(--accent)"></i>下行 KB/s<i style="background:var(--warn)"></i>排队秒数（右轴）</div>
  </div>
  <div class="empty" id="noLane">还没有手机启用弱网。在左侧"手机"里点"启用弱网"。</div>
 </div>
 <div class="card" id="hostCard"><h2>按域名的下行流量（近 5 秒）</h2><table><thead><tr><th>域名</th><th class="n">KB/s</th><th class="n">排队 KB</th><th style="width:30%"></th></tr></thead><tbody id="hosts"></tbody></table></div>
 <div class="card"><h2>事件</h2><div class="events" id="events"></div></div>
</section>
</main>
<script>
const F=[["down_kbps","下行 kbps",0,30000,50],["up_kbps","上行 kbps",0,10000,50],["delay_ms","单程延迟 ms",0,2000,10],
["jitter_ms","抖动 ms",0,1000,10],["loss_pct","丢包 %",0,30,0.5],["window_kb","连接窗口 KB",16,1024,16]];
const $=id=>document.getElementById(id);let state=null,editing=0,sel=null;
const esc=s=>String(s).replace(/[&<>"']/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]));
const post=(u,b)=>fetch(u,{method:"POST",body:b?JSON.stringify(b):""}).then(()=>refresh());
const dev=(s,a)=>post(`/api/device/${encodeURIComponent(s)}/${a}`);
$("fields").innerHTML=F.map(([k,l,mn,mx,st])=>`<div class="field"><label>${l}</label><input type="range" id="r_${k}" min="${mn}" max="${mx}" step="${st}"><input type="number" id="n_${k}" min="${mn}" step="${st}"></div>`).join("");
let timer=null;const send=(k,v)=>{editing=Date.now();clearTimeout(timer);timer=setTimeout(()=>post("/api/config",{[k]:Number(v)}),150)};
F.forEach(([k])=>{$("r_"+k).oninput=e=>{$("n_"+k).value=e.target.value;send(k,e.target.value)};$("n_"+k).onchange=e=>{$("r_"+k).value=e.target.value;send(k,e.target.value)}});
document.querySelectorAll("#outage button").forEach(b=>b.onclick=()=>post("/api/config",{outage:b.dataset.o}));
$("saveAllow").onclick=()=>post("/api/config",{allow:$("allow").value});
function draw(h){const c=$("chart"),r=devicePixelRatio||1,W=c.clientWidth,H=c.clientHeight;if(!W)return;c.width=W*r;c.height=H*r;const g=c.getContext("2d");g.scale(r,r);
 const cs=getComputedStyle(document.documentElement),col=n=>cs.getPropertyValue(n).trim();g.clearRect(0,0,W,H);
 const pad=28,n=180,maxD=Math.max(10,...h.map(p=>p[1]))*1.15,maxQ=Math.max(1,...h.map(p=>p[3]))*1.15;
 g.strokeStyle=col("--line");g.lineWidth=1;for(let i=0;i<=4;i++){const y=8+(H-pad-8)*i/4;g.beginPath();g.moveTo(pad,y);g.lineTo(W-pad,y);g.stroke()}
 g.fillStyle=col("--quiet");g.font="11px sans-serif";g.fillText(maxD.toFixed(0),2,14);g.fillText(maxQ.toFixed(1)+"s",W-pad+2,14);g.fillText("-3 分钟",pad,H-8);g.fillText("现在",W-pad-24,H-8);
 const X=i=>pad+(W-2*pad)*(i+n-h.length)/(n-1);
 const line=(idx,max,color)=>{g.strokeStyle=color;g.lineWidth=1.6;g.beginPath();h.forEach((p,i)=>{const y=8+(H-pad-8)*(1-p[idx]/max);i?g.lineTo(X(i),y):g.moveTo(X(i),y)});g.stroke()};
 line(1,maxD,col("--accent"));line(3,maxQ,col("--warn"))}
function renderDevices(){const ds=state.devices;
 if(!state.adb){$("devices").innerHTML=`<div class="empty">找不到 adb，请用 --adb 指定路径</div>`;return}
 if(!ds.length){$("devices").innerHTML=`<div class="empty">没有检测到手机（USB 连接并允许调试后会自动出现）</div>`;return}
 $("devices").innerHTML=ds.map(d=>{const ready=d.state==="device",busy=!!d.busy;
  const tag=d.enabled?`<span class="tag on">弱网中 · 端口 ${d.port}</span>`:d.foreign?`<span class="tag warn">已有其他代理 ${esc(d.proxy)}</span>`:ready?`<span class="tag">未启用</span>`:`<span class="tag warn">${esc(d.state)}</span>`;
  const s=esc(d.serial);
  return `<div class="dev"><div class="t"><div><b>${esc(d.model)}</b><br><small>${s}</small></div>${tag}</div><div class="row">`+
   (d.enabled?`<button ${busy?"disabled":""} onclick="dev('${s}','off')">关闭弱网</button><button ${busy?"disabled":""} onclick="dev('${s}','reconnect')">强制重连（关开 WiFi）</button>`
    :`<button ${busy||!ready?"disabled":""} onclick="dev('${s}','on')">启用弱网</button>`+(d.foreign?`<button ${busy?"disabled":""} onclick="dev('${s}','off')">清除代理</button>`:""))+
   (busy?`<span class="status">执行中：${esc(d.busy)}…</span>`:"")+`</div></div>`}).join("")}
function renderLane(){const lanes=state.lanes;
 if(!lanes.find(l=>l.serial===sel))sel=lanes.length?lanes[0].serial:null;
 $("tabs").innerHTML=lanes.map(l=>`<button class="${l.serial===sel?"on":""}" onclick="sel='${esc(l.serial)}';render()">${esc(l.name)}</button>`).join("");
 const lane=lanes.find(l=>l.serial===sel);$("laneBody").style.display=lane?"":"none";$("noLane").style.display=lane?"none":"";$("hostCard").style.display=lane?"":"none";
 if(!lane)return;const s=lane.stats,last=s.history.at(-1)||[0,0,0,0];
 $("kDown").textContent=last[1].toFixed(1);$("kQueue").textContent=s.queue_down_s.toFixed(1);$("kConns").textContent=s.conns;$("kLoss").textContent=s.loss_events;
 const mx=Math.max(1,...s.hosts.map(h=>h.kbs));$("hosts").innerHTML=s.hosts.map(h=>`<tr><td>${esc(h.host)}</td><td class="n">${h.kbs}</td><td class="n">${h.queued_kb}</td><td><div class="bar" style="width:${100*h.kbs/mx}%"></div></td></tr>`).join("")||`<tr><td colspan="4" class="empty">暂无流量</td></tr>`;
 draw(s.history)}
function render(){const c=state.config;
 $("presets").innerHTML=Object.entries(state.presets).map(([k,l])=>`<button class="${c.preset===k?"on":""}" onclick="post('/api/preset/${k}')">${l}</button>`).join("");
 if(Date.now()-editing>1500)F.forEach(([k])=>{$("r_"+k).value=c[k];$("n_"+k).value=c[k]});
 document.querySelectorAll("#outage button").forEach(b=>b.classList.toggle("on",b.dataset.o===c.outage));
 if(document.activeElement!==$("allow"))$("allow").value=c.allow.join(",");
 $("summary").textContent=`下行 ${c.down_kbps||"不限"} kbps · 上行 ${c.up_kbps||"不限"} kbps · RTT ≈ ${c.delay_ms*2} ms · 抖动 ${c.jitter_ms} ms · 丢包 ${c.loss_pct}%`+(c.outage?` · ${c.outage==="stall"?"卡死中":"已断开"}`:"")+` · ${state.lanes.length} 台手机走弱网`;
 renderDevices();renderLane();$("events").textContent=state.events.join("\n")}
async function refresh(){try{state=await (await fetch("/api/state")).json();render()}catch(e){$("summary").textContent="连接不上模拟器"}}
refresh();setInterval(refresh,1000);
</script></body></html>
"""

if __name__ == "__main__":
    asyncio.run(main())
