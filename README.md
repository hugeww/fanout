# fanout

fanout 把 VPN Gate 的公共节点转换为本机的 SOCKS5 出口。每条 Tunnel
运行在独立的 network namespace 中，因此不同出口之间互不影响。

当前版本只保留 fanout 自己的 Tunnel、统一 SOCKS5 入口和独立入口功能。
Xray、3x-ui、xray-cf-lite 的运行、安装、配置和管理界面均已禁用，不会下载
或启动任何 Xray 进程。

## 工作方式

客户端 -> 统一 SOCKS5 入口 -> 用户凭据 -> 用户专属 Tunnel -> 出口 IP
客户端 -> 独立 Tunnel SOCKS5 入口 ---------------------> 出口 IP

统一入口会先读取并验证用户名和密码，再从该用户的 Tunnel 池中按配置策略
选择一条处于 up 状态的 Tunnel。Tunnel 连接失败时直接返回错误，不回退到
母机直连。不同用户的 Tunnel 池独立管理，同一条 Tunnel 不会在用户之间共享。

## 安装

需要 Linux、root、openvpn、curl、openssl、iproute 和 iptables。
安装脚本会自动安装缺少的系统依赖，但不会安装 Xray 或任何 Xray 面板。

bash <(curl -fsSL https://raw.githubusercontent.com/hugeww/fanout/main/install.sh)

安装后运行 f 打开管理菜单。管理页面会显示统一 SOCKS5 入口、用户凭据、
Tunnel 状态和出口 IP。

## 运维

f info       # 连接信息
f list       # Tunnel 列表
f restart    # 重启
f log        # 查看日志
f update     # 更新
f uninstall  # 卸载

Tunnel 状态保存在 /var/lib/fanout/state.json，重启后会自动恢复。

## 已知限制

- 目前只转发 TCP；SOCKS5 收到域名时由本机解析。
- VPN Gate 节点可能下线或达到连接上限，健康检查会自动标记失败并按策略
  重新选择节点。
- 管理页面默认没有 HTTPS，公开部署时应放在 HTTPS 反向代理后面。

## 许可

[MIT](LICENSE)