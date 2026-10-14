#!/usr/bin/env node
// npm 分发入口：运行包内对应平台的 weaknet 二进制。只用 ES5 语法，兼容 Node 8 及以上。
'use strict';
var spawn = require('child_process').spawn;
var fs = require('fs');
var path = require('path');
var https = require('https');
var pkg = require('../package.json');

var targets = {
    darwin: { x64: 'weaknet-darwin-amd64', arm64: 'weaknet-darwin-arm64' },
    win32: { x64: 'weaknet-win32-x64.exe', arm64: 'weaknet-win32-arm64.exe' }
};
var registries = ['https://registry.npmjs.org', 'https://registry.npmmirror.com'];
var args = process.argv.slice(2);

if (args[0] === 'update') {
    update();
} else {
    run();
}

function run() {
    var name = targets[process.platform] && targets[process.platform][process.arch];
    if (!name) {
        console.error('weaknet 暂不支持 ' + process.platform + ' ' + process.arch);
        process.exit(1);
    }
    var binary = path.join(__dirname, name);
    if (!fs.existsSync(binary)) {
        console.error('找不到 weaknet 程序：' + binary);
        console.error('请重新安装：npm install -g weaknet');
        process.exit(1);
    }
    var child = spawn(binary, args, { stdio: 'inherit' });
    // Ctrl+C 会同时发给本进程和模拟器：本进程不退出，等模拟器恢复手机代理后随之退出。
    // 只发给本进程的终止信号转发给模拟器；Windows 上模拟器自己会收到控制台事件，转发反而会强制结束它。
    ['SIGINT', 'SIGBREAK', 'SIGTERM', 'SIGHUP'].forEach(function (signal) {
        try {
            process.on(signal, function () {
                if (process.platform !== 'win32' && (signal === 'SIGTERM' || signal === 'SIGHUP')) {
                    try { child.kill(signal); } catch (e) { /* 已退出 */ }
                }
            });
        } catch (e) { /* 平台不支持该信号 */ }
    });
    child.on('error', function (err) {
        console.error('启动 weaknet 失败：' + err.message);
        process.exit(1);
    });
    child.on('exit', function (code) {
        process.exit(typeof code === 'number' ? code : 1);
    });
}

// npm 安装时由本脚本更新：Windows 上运行中的 exe 无法被覆盖，不能让 weaknet 程序自己执行 npm install。
function update() {
    console.log('当前版本：v' + pkg.version);
    console.log('正在检查更新...');
    latestVersion(0, function (err, latest) {
        if (err) {
            console.error('查询最新版本失败：' + err.message);
            process.exit(1);
        }
        console.log('最新版本：v' + latest);
        if (latest === pkg.version) {
            console.log('已是最新版本。');
            return;
        }
        console.log('执行：npm install -g weaknet@latest');
        // Windows 上 npm 是 npm.cmd，新版 Node 需要经 shell 启动 .cmd。
        var npm = spawn('npm', ['install', '-g', 'weaknet@latest'], { stdio: 'inherit', shell: process.platform === 'win32' });
        npm.on('error', function (e) {
            console.error('执行 npm 失败：' + e.message);
            process.exit(1);
        });
        npm.on('exit', function (code) {
            if (code !== 0) {
                console.error('更新失败。如果模拟器正在运行，请先退出再执行 weaknet update。');
            }
            process.exit(typeof code === 'number' ? code : 1);
        });
    });
}

// 官方源在国内可能很慢，失败时再查镜像。
function latestVersion(index, done) {
    if (index >= registries.length) {
        done(new Error('所有 npm 源都无法访问'));
        return;
    }
    var finished = false;
    var next = function (err, version) {
        if (finished) return;
        finished = true;
        if (err) {
            latestVersion(index + 1, function (e, v) { done(e ? err : null, v); });
        } else {
            done(null, version);
        }
    };
    var req = https.get(registries[index] + '/weaknet/latest', function (res) {
        var body = '';
        res.setEncoding('utf8');
        res.on('data', function (chunk) { body += chunk; });
        res.on('end', function () {
            if (res.statusCode !== 200) {
                next(new Error(registries[index] + ' 返回 HTTP ' + res.statusCode));
                return;
            }
            try {
                var version = JSON.parse(body).version;
                if (version) next(null, version); else next(new Error('未返回版本号'));
            } catch (e) {
                next(e);
            }
        });
    });
    req.setTimeout(8000, function () { req.destroy(); next(new Error(registries[index] + ' 超时')); });
    req.on('error', function (e) { next(e); });
}
