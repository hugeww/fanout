# fanout

fanout 把 VPN Gate 的公共节点转换为本机的 SOCKS5 / HTTP 出口。每条 Tunnel
运行在独立的 network namespace 中，因此不同出口之间互不影响。

当前版本只保留 fanout 自己的 Tunnel、统一入口和独立入口功能。
Xray、3x-ui、xray-cf-lite 的运行、安装、配置和管理界面均已禁用，不会下载
或启动任何 Xray 进程。

## 工作方式

客户端 -> 统一入口（SOCKS5 或 HTTP CONNECT）-> 用户凭据 -> 用户专属 Tunnel -> 出口 IP
客户端 -> 独立 Tunnel SOCKS5 入口 ---------------------------------------> 出口 IP

统一入口共用一个端口：先看第一个字节，`0x05` 走 SOCKS5，其余按 HTTP
代理处理（`CONNECT` 转发 HTTPS，绝对路径 `GET/POST` 转发明文 HTTP）。
认证通过后从该用户的 Tunnel 池中按配置策略选择一条处于 up 状态的
Tunnel。Tunnel 连接失败时直接返回错误，不回退到母机直连。不同用户的
Tunnel 池独立管理，同一条 Tunnel 不会在用户之间共享。

## 安装

需要 Linux、root、openvpn、curl、openssl、iproute 和 iptables。
安装脚本会自动安装缺少的系统依赖，但不会安装 Xray 或任何 Xray 面板。

bash <(curl -fsSL https://raw.githubusercontent.com/hugeww/fanout/main/install.sh)

安装后运行 f 打开管理菜单。管理页面会显示统一入口、用户凭据、
Tunnel 状态和出口 IP。复制用户凭据时会同时给出 `socks5://` 和 `http://`
两种地址。

## 本地安全连 SOCKS5

将设置里的“各隧道 SOCKS5 监听地址”改为“仅本机 127.0.0.1”后，在本地电脑执行 SSH 本地转发：

    ssh -N -L 1080:127.0.0.1:<隧道端口> root@服务器IP

客户端填 `socks5://127.0.0.1:1080` 即可，**无需 SOCKS5 用户名口令**。这是因为仅本机监听时信任 SSH 连接作为认证边界，并且 Playwright/Camoufox 不支持 SOCKS5 用户名口令，免认证方可直接使用。仍监听 0.0.0.0 或具体外网 IP 时，各隧道依然要求用户名口令。

统一入口同样支持该模式：把“统一入口配置 → 监听地址”也选成“仅本机 127.0.0.1”，
入口即免认证、自动汇聚所有 Tunnel 轮询出网，无需为每个用户配置用户名密码。
同一端口可填 `socks5://127.0.0.1:1080` 或 `http://127.0.0.1:1080`（HTTP
代理，HTTPS 网站走 CONNECT）。Playwright 也可用 HTTP 代理带用户名口令：
`http://user:pass@host:port`。仍监听外网时统一入口照旧要求用户名密码并按
用户路由到专属 Tunnel。

## 运维

f info       # 连接信息
f list       # Tunnel 列表
f restart    # 重启
f log        # 查看日志
f update     # 更新
f uninstall  # 卸载

Tunnel 状态保存在 /var/lib/fanout/state.json，重启后会自动恢复。

## 已知限制

- 目前只转发 TCP；SOCKS5 / HTTP 收到域名时由本机解析。
- 独立 Tunnel 入口仍只提供 SOCKS5；HTTP/HTTPS（CONNECT）只在统一入口上。
- VPN Gate 节点可能下线或达到连接上限，健康检查会自动标记失败并按策略
  重新选择节点。
- 管理页面默认没有 HTTPS，公开部署时应放在 HTTPS 反向代理后面。

## 许可

[MIT](LICENSE)