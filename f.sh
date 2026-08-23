#!/usr/bin/env bash
# fanout 管理菜单
set -uo pipefail

WORK_DIR=/var/lib/fanout
SERVICE=fanout
BIN=/usr/local/bin/fanout
REPO="${REPO:-hugeww/fanout}"

G='\033[0;32m'; R='\033[0;31m'; Y='\033[0;33m'; B='\033[0;36m'; D='\033[2m'; N='\033[0m'

need_root() {
  [[ $EUID -eq 0 ]] || { echo -e "${R}需要 root${N}"; exit 1; }
}

# ── init 系统抽象：systemd 与 OpenRC ────────────────────
if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
  INIT_SYS=systemd
  UNIT=/etc/systemd/system/${SERVICE}.service
else
  INIT_SYS=openrc
  UNIT=/etc/init.d/${SERVICE}
fi

svc_start()   { [[ $INIT_SYS == systemd ]] && systemctl start "$SERVICE"   || rc-service "$SERVICE" start; }
svc_stop()    { [[ $INIT_SYS == systemd ]] && systemctl stop "$SERVICE"    || rc-service "$SERVICE" stop; }
svc_restart() { [[ $INIT_SYS == systemd ]] && systemctl restart "$SERVICE" || rc-service "$SERVICE" restart; }
svc_reload()  { [[ $INIT_SYS == systemd ]] && systemctl daemon-reload || true; }
svc_enable()  { [[ $INIT_SYS == systemd ]] && systemctl enable "$SERVICE" >/dev/null 2>&1 || rc-update add "$SERVICE" default >/dev/null 2>&1; }
svc_disable() { [[ $INIT_SYS == systemd ]] && systemctl disable "$SERVICE" >/dev/null 2>&1 || rc-update del "$SERVICE" default >/dev/null 2>&1; }

svc_is_enabled() {
  if [[ $INIT_SYS == systemd ]]; then
    systemctl is-enabled --quiet "$SERVICE"
  else
    rc-update show default 2>/dev/null | grep -q "^ *${SERVICE} "
  fi
}

svc_enabled_text() {
  svc_is_enabled && echo enabled || echo disabled
}

svc_status_page() {
  if [[ $INIT_SYS == systemd ]]; then
    systemctl status "$SERVICE" --no-pager
  else
    rc-service "$SERVICE" status
  fi
}

svc_logs() {
  if [[ $INIT_SYS == systemd ]]; then
    journalctl -u "$SERVICE" -n "${1:-50}" --no-pager
  else
    tail -n "${1:-50}" /var/log/${SERVICE}.log 2>/dev/null || echo "  暂无日志"
  fi
}

svc_logs_follow() {
  if [[ $INIT_SYS == systemd ]]; then
    journalctl -u "$SERVICE" -f
  else
    tail -f /var/log/${SERVICE}.log
  fi
}

svc_state() {
  if [[ $INIT_SYS == systemd ]]; then
    systemctl is-active --quiet "$SERVICE" && echo running || echo stopped
  else
    rc-service "$SERVICE" status >/dev/null 2>&1 && echo running || echo stopped
  fi
}

# 端口以 settings.json 为准。老版本把 -web 写死在服务文件里，
# 两处各改各的会互相拽回旧值，所以这里只认工作目录下的配置。
web_port() {
  local p
  p=$(sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' \
        "$WORK_DIR/settings.json" 2>/dev/null | head -1)
  [[ -n $p ]] && { echo "$p"; return; }
  # 兼容老安装：settings.json 还没生成时退回读服务文件
  grep -oE '\-web [0-9]+' "$UNIT" 2>/dev/null \
    | grep -oE '[0-9]+' | head -1 || echo 8899
}

public_ip() {
  curl -s --max-time 6 http://api.ipify.org 2>/dev/null || echo "<本机IP>"
}

pause() {
  echo
  read -rp "回车返回菜单..." _
}

show_info() {
  local state port bp pw ip
  state=$(svc_state); port=$(web_port)
  bp=$(cat "$WORK_DIR/basepath" 2>/dev/null || echo "-")
  pw=$(cat "$WORK_DIR/password" 2>/dev/null || echo "-")
  ip=$(public_ip)

  echo
  if [[ $state == running ]]; then
    echo -e "  状态      ${G}运行中${N}"
  else
    echo -e "  状态      ${R}已停止${N}"
  fi
  echo -e "  版本      $("$BIN" -version 2>/dev/null || echo '-')"
  echo -e "  开机自启  $(svc_enabled_text)"
  echo
  local scheme=http
  grep -q '"tls"[[:space:]]*:[[:space:]]*true' "$WORK_DIR/settings.json" 2>/dev/null && scheme=https
  echo -e "  ${B}管理地址  ${scheme}://${ip}:${port}/${bp}/${N}"
  echo -e "  ${B}访问口令  ${pw}${N}"
  echo

  local n
  n=$(ls -d /var/run/netns/fo* 2>/dev/null | wc -l | tr -d ' ')
  echo -e "  ${D}运行中的隧道: ${n}${N}"
}

list_tunnels() {
  local port bp pw ck
  port=$(web_port)
  bp=$(cat "$WORK_DIR/basepath" 2>/dev/null)
  pw=$(cat "$WORK_DIR/password" 2>/dev/null)
  ck=$(mktemp)

  curl -s --max-time 10 -c "$ck" -X POST -d "password=${pw}" \
    "http://127.0.0.1:${port}/${bp}/login" -o /dev/null
  echo
  curl -s --max-time 10 -b "$ck" "http://127.0.0.1:${port}/${bp}/api/tunnels" \
    > "$ck.json" 2>/dev/null
  rm -f "$ck"

  # 用 sed/awk 解析而不是 python3/jq：Alpine 最小安装两者都没有，
  # 为了一条列表命令再拉依赖不值当。字段固定，按对象拆行足够稳。
  if [[ ! -s "$ck.json" ]] || ! grep -q '"port"' "$ck.json" 2>/dev/null; then
    echo "  还没有隧道，去网页里添加"
  else
    printf "  %-10s%-11s%-18s%s\n" "端口" "状态" "出口 IP" "节点"
    # 按 {"slot" 切分而不是按 }：node 是嵌套对象，按 } 切会把一条记录劈成两半
    sed 's/{"slot"/\n{"slot"/g' "$ck.json" | while IFS= read -r line; do
      case "$line" in *'"slot"'*) ;; *) continue ;; esac
      p=$(echo "$line"  | sed -n 's/.*"port":\([0-9]*\).*/\1/p')
      st=$(echo "$line" | sed -n 's/.*"status":"\([^"]*\)".*/\1/p')
      ip=$(echo "$line" | sed -n 's/.*"exit_ip":"\([^"]*\)".*/\1/p')
      hn=$(echo "$line" | sed -n 's/.*"hostname":"\([^"]*\)".*/\1/p')
      [[ -z $p ]] && continue
      printf "  %-10s%-11s%-18s%s\n" "$p" "${st:--}" "${ip:--}" "${hn:--}"
    done
  fi
  rm -f "$ck.json"
}

change_port() {
  local cur new
  cur=$(web_port)
  echo
  read -rp "  新端口 (当前 ${cur}): " new
  [[ -z $new ]] && { echo "  未修改"; return; }
  if ! [[ $new =~ ^[0-9]+$ ]] || (( new < 1 || new > 65535 )); then
    echo -e "  ${R}端口不合法${N}"; return
  fi
  if ss -tln 2>/dev/null | grep -q ":${new} "; then
    echo -e "  ${R}端口 ${new} 已被占用${N}"; return
  fi
  # 写 settings.json（权威来源），并把服务文件里可能残留的 -web 一并同步，
  # 免得老安装重启后又被写死的旧端口拽回去。
  if [[ -f "$WORK_DIR/settings.json" ]]; then
    sed -i "s/\"port\"[[:space:]]*:[[:space:]]*[0-9]*/\"port\": ${new}/" "$WORK_DIR/settings.json"
  else
    printf '{\n  "port": %s,\n  "listen_addr": ""\n}\n' "$new" > "$WORK_DIR/settings.json"
    chmod 600 "$WORK_DIR/settings.json"
  fi
  sed -i "s/-web ${cur}/-web ${new}/" "$UNIT" 2>/dev/null
  svc_reload
  svc_restart
  echo -e "  ${G}已改为 ${new} 并重启${N}"
}

reset_password() {
  local pw
  echo
  read -rp "  新口令 (留空则随机生成): " pw
  if [[ -z $pw ]]; then
    pw=$(head -c 9 /dev/urandom | od -An -tx1 | tr -d ' \n')
  fi
  umask 077
  echo "$pw" > "$WORK_DIR/password"
  svc_restart
  echo -e "  ${G}新口令: ${pw}${N}"
}

reset_basepath() {
  local bp
  echo
  read -rp "  新访问路径 (留空则随机生成): " bp
  if [[ -z $bp ]]; then
    rm -f "$WORK_DIR/basepath"
    svc_restart
    sleep 2
    bp=$(cat "$WORK_DIR/basepath" 2>/dev/null)
  else
    bp=${bp#/}; bp=${bp%/}
    umask 077
    echo "$bp" > "$WORK_DIR/basepath"
    svc_restart
  fi
  echo -e "  ${G}新路径: /${bp}/${N}"
}

ipv6_state() {
  local a d
  a=$(sysctl -n net.ipv6.conf.all.disable_ipv6 2>/dev/null || echo 0)
  d=$(sysctl -n net.ipv6.conf.default.disable_ipv6 2>/dev/null || echo 0)
  [[ "$a" == 1 && "$d" == 1 ]] && echo disabled || echo enabled
}

toggle_ipv6() {
  local conf=/etc/sysctl.d/99-fanout-ipv6.conf
  echo
  if [[ $(ipv6_state) == disabled ]]; then
    read -rp "  当前已禁用 IPv6，要重新启用吗？[y/N]: " yes
    [[ ${yes,,} == y ]] || { echo "  已取消"; return; }
    rm -f "$conf"
    sysctl -qw net.ipv6.conf.all.disable_ipv6=0
    sysctl -qw net.ipv6.conf.default.disable_ipv6=0
    sysctl -qw net.ipv6.conf.lo.disable_ipv6=0
    echo -e "  ${G}已重新启用 IPv6${N}"
    return
  fi

  echo -e "  ${D}母机有全局 IPv6 时，没走隧道的流量可能从 IPv6 出去，暴露真实地址。${N}"
  read -rp "  确认禁用整机 IPv6？[y/N]: " yes
  [[ ${yes,,} == y ]] || { echo "  已取消"; return; }

  cat > "$conf" <<EOF
net.ipv6.conf.all.disable_ipv6 = 1
net.ipv6.conf.default.disable_ipv6 = 1
net.ipv6.conf.lo.disable_ipv6 = 1
EOF
  sysctl -qw net.ipv6.conf.all.disable_ipv6=1
  sysctl -qw net.ipv6.conf.default.disable_ipv6=1
  sysctl -qw net.ipv6.conf.lo.disable_ipv6=1
  svc_restart >/dev/null 2>&1
  echo -e "  ${G}已禁用 IPv6（重启后依然生效）${N}"
}

show_links() {
  echo
  echo -e "  项目    ${B}https://github.com/hugeww/fanout${N}"
  echo
  echo -e "  ${D}用着有问题、或者想要什么功能，去群里说或提 issue。${N}"
}

# 打印从本地安全访问 fanout SOCKS5 的 SSH 本地转发命令。
# 必须用 -L 把远端 SOCKS5 端口引到本地；
# -D 是让 SSH 自己当 SOCKS5，会绕过 fanout 的隧道，出口会变成服务器 IP。
tunnel_hint() {
  local ip sshport tport sport
  ip=$(public_ip)
  # 服务器 SSH 端口：优先读 -p 参数，其次 22
  sshport=$(grep -oE '^Port [0-9]+' /etc/ssh/sshd_config 2>/dev/null | awk '{print $2}' | head -1)
  [[ -n $sshport ]] || sshport=22

  echo
  echo -e "  ${B}在本地电脑执行（把 1080 换成任意空闲本地端口）：${N}"

  echo
  echo -e "  ${D}方式 A · 独立隧道入口（浏览器 / Playwright / Camoufox 用，免认证）${N}"
  echo -e "  ${D}隧道端口在 f list 里可见（如 29148/37281/55424）。${N}"
  read -rp "  独立隧道 SOCKS5 端口（留空跳过）: " tport
  if [[ -n $tport ]]; then
    echo
    echo -e "    ssh -N -L 1080:127.0.0.1:${tport} root@${ip} -p ${sshport}"
    echo
    echo -e "  客户端填（监听地址设为仅本机时无需用户名密码）："
    echo -e "    socks5://127.0.0.1:1080"
  else
    echo -e "  ${D}（已跳过）${N}"
  fi

  echo
  echo -e "  ${D}方式 B · 统一入口（监听地址设为仅本机时免认证，否则需用户名密码）${N}"
  echo -e "  ${D}统一入口端口在管理界面里可见。${N}"
  read -rp "  统一入口端口（留空跳过）: " sport
  if [[ -n $sport ]]; then
    echo
    echo -e "    ssh -N -L 1080:127.0.0.1:${sport} root@${ip} -p ${sshport}"
    echo
    echo -e "  客户端填："
    echo -e "    socks5://127.0.0.1:1080"
    echo -e "    用户名/密码 统一入口里的用户名密码（监听地址设为仅本机时无需）"
  else
    echo -e "  ${D}（已跳过）${N}"
  fi

  echo
  echo -e "  ${D}安全说明：流量走 SSH 加密；免认证只在端口监听 127.0.0.1 时成立"
  echo -e "  （设置 → 各隧道 SOCKS5 监听地址，或统一入口监听地址，选“仅本机”）。"
  echo -e "  别在 NAT 里映射 SOCKS5 端口，公网就扫不到。${N}"
}

# settings.json 里的 tls 开关。传 1 开 HTTPS、0 关，空参数只读。
# 直接重写整个文件保证 JSON 整洁，同时保留现有 port/listen_addr。
set_tls() {
  local f="$WORK_DIR/settings.json"
  local want="${1:-}"
  local port listen tls
  port=$(sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$f" 2>/dev/null | head -1)
  listen=$(sed -n 's/.*"listen_addr"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$f" 2>/dev/null | head -1)
  tls=false
  if [[ -n "$want" ]]; then
    [[ "$want" == "1" ]] && tls=true
    [[ -n $port ]] || port=8899
    [[ -n $listen ]] || listen=""
    printf '{\n  "port": %s,\n  "listen_addr": "%s",\n  "tls": %s\n}\n' \
      "$port" "$listen" "$tls" > "$f"
    chmod 600 "$f"
  fi
  grep -q '"tls"[[:space:]]*:[[:space:]]*true' "$f" 2>/dev/null && echo on || echo off
}

cert_info() {
  [[ -f "$WORK_DIR/web.crt" ]] || { echo "  没有证书（web.crt）"; return; }
  openssl x509 -in "$WORK_DIR/web.crt" -noout -subject -dates 2>/dev/null \
    | sed 's/^/  /'
}

gen_cert_here() {
  if ! command -v openssl >/dev/null 2>&1; then
    echo -e "  ${R}找不到 openssl${N}"; return
  fi
  local cn
  read -rp "  证书域名/CN（留空用本机 IP）: " cn
  if [[ -z $cn ]]; then
    cn=$(curl -s --max-time 5 http://api.ipify.org 2>/dev/null || hostname -f || echo localhost)
  fi
  if ! openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
      -keyout "$WORK_DIR/web.key" -out "$WORK_DIR/web.crt" \
      -subj "/CN=${cn}" >/dev/null 2>&1; then
    echo -e "  ${R}生成失败${N}"; return
  fi
  chmod 600 "$WORK_DIR/web.key" "$WORK_DIR/web.crt"
  set_tls 1
  svc_restart >/dev/null 2>&1
  echo -e "  ${G}已生成自签证书（CN=${cn}）并启用 HTTPS${N}"
}

manage_cert() {
  local cur
  cur=$(set_tls)
  echo
  if [[ $cur == on ]]; then
    echo -e "  HTTPS 已启用（端口 $(web_port)）"
  else
    echo -e "  HTTPS 未启用（当前 http://$(web_port)/）"
  fi
  echo
  if [[ -f "$WORK_DIR/web.crt" ]]; then
    echo -e "  当前证书："
    cert_info
  else
    echo -e "  ${D}暂无证书${N}"
  fi
  echo
  echo "  1) 上传证书（Cert + Key）"
  echo "  2) 生成自签证书"
  echo "  3) 启用 HTTPS"
  echo "  4) 停用 HTTPS（回 http）"
  echo "  5) 删除证书"
  echo "  0) 返回"
  read -rp "  选择: " c
  case "$c" in
    1)
      read -rp "  证书文件路径: " cert
      read -rp "  私钥文件路径: " key
      if [[ ! -f $cert || ! -f $key ]]; then
        echo -e "  ${R}文件不存在${N}"; return
      fi
      # 先校验配对，不配对不上就装
      if ! openssl x509 -noout -in "$cert" >/dev/null 2>&1; then
        echo -e "  ${R}证书文件无效${N}"; return
      fi
      if ! openssl x509 -in "$cert" -noout -pubkey 2>/dev/null \
           | openssl md5 | awk '{print $2}' | grep -q \
           "$(openssl pkey -in "$key" -pubout 2>/dev/null | openssl md5 | awk '{print $2}')"; then
        echo -e "  ${R}证书和私钥不匹配${N}"; return
      fi
      install -m 600 "$cert" "$WORK_DIR/web.crt"
      install -m 600 "$key"  "$WORK_DIR/web.key"
      set_tls 1
      svc_restart >/dev/null 2>&1
      echo -e "  ${G}证书已安装并启用 HTTPS${N}"
      ;;
    2)
      gen_cert_here
      ;;
    3)
      if [[ -f "$WORK_DIR/web.crt" && -f "$WORK_DIR/web.key" ]]; then
        set_tls 1
        svc_restart >/dev/null 2>&1
        echo -e "  ${G}已启用 HTTPS${N}"
      else
        echo -e "  ${R}没有证书，先上传或生成${N}"
      fi
      ;;
    4)
      set_tls 0
      svc_restart >/dev/null 2>&1
      echo -e "  ${G}已停用 HTTPS，回到 http://$(web_port)/${N}"
      ;;
    5)
      read -rp "  确认删除证书？[y/N]: " yes
      [[ ${yes,,} == y ]] || { echo "  已取消"; return; }
      rm -f "$WORK_DIR/web.crt" "$WORK_DIR/web.key"
      set_tls 0
      svc_restart >/dev/null 2>&1
      echo -e "  ${G}证书已删除，回 http${N}"
      ;;
    0|*) ;;
  esac
}

# 老版本把 -web 写死在服务文件里，和 settings.json 互相拽回旧值。
# 更新时把端口搬进配置再从服务文件里摘掉，之后只认一处。
migrate_port_to_settings() {
  local unit_port
  unit_port=$(grep -oE '\-web [0-9]+' "$UNIT" 2>/dev/null | grep -oE '[0-9]+' | head -1)
  [[ -z $unit_port ]] && return

  if [[ ! -f "$WORK_DIR/settings.json" ]]; then
    printf '{\n  "port": %s,\n  "listen_addr": ""\n}\n' "$unit_port" > "$WORK_DIR/settings.json"
    chmod 600 "$WORK_DIR/settings.json"
  fi
  sed -i "s/-web ${unit_port} //" "$UNIT"
  svc_reload
  echo "  已把端口 ${unit_port} 迁移到 settings.json"
}

do_update() {
  local arch goarch tmp
  arch=$(uname -m)
  case "$arch" in
    x86_64) goarch=amd64 ;;
    aarch64|arm64) goarch=arm64 ;;
    *) echo -e "  ${R}不支持的架构 ${arch}${N}"; return ;;
  esac

  echo -e "\n  当前 $("$BIN" -version 2>/dev/null || echo '-')"
  tmp=$(mktemp -d)
  echo "  正在下载最新版..."
  if ! curl -fsSL "https://github.com/${REPO}/releases/latest/download/fanout-linux-${goarch}.tar.gz" \
       -o "$tmp/f.tar.gz"; then
    echo -e "  ${R}下载失败${N}"; rm -rf "$tmp"; return
  fi
  tar xzf "$tmp/f.tar.gz" -C "$tmp"
  svc_stop
  install -m 755 "$tmp/fanout" "$BIN"
  migrate_port_to_settings
  svc_start
  rm -rf "$tmp"
  echo -e "  ${G}已更新到 $("$BIN" -version 2>/dev/null)${N}"
}

do_uninstall() {
  local yes
  echo
  read -rp "  确认卸载？隧道和配置都会删除 [y/N]: " yes
  [[ ${yes,,} == y ]] || { echo "  已取消"; return; }

  svc_stop >/dev/null 2>&1
  svc_disable
  # 清掉残留的 netns 与 veth
  for ns in $(ip netns list 2>/dev/null | awk '{print $1}' | grep '^fo[0-9]'); do
    ip netns del "$ns" 2>/dev/null
  done
  for l in $(ip -o link show 2>/dev/null | awk -F': ' '{print $2}' | grep '^fov[0-9]'); do
    ip link del "$l" 2>/dev/null
  done
  rm -f "$UNIT" "$BIN" /usr/local/bin/f
  rm -rf "$WORK_DIR"
  svc_reload
  echo -e "  ${G}已卸载${N}"
  exit 0
}

menu() {
  while true; do
    clear
    echo -e "${B}  fanout${N}  ${D}VPN Gate 出口扇出网关${N}"
    show_info
    echo -e "${D}  ─────────────────────────────${N}"
    echo "   1) 启动          2) 停止"
    echo "   3) 重启          4) 查看日志"
    echo
    echo "   5) 隧道列表      6) 连接信息"
    echo "   7) 隧道访问（本地安全连 SOCKS5）"
    echo
    echo "   8) 改端口        9) 改口令"
    echo "  10) 改访问路径   11) 证书管理"
    echo "  12) 开机自启开关"
    echo
    echo "  13) 更新         14) 卸载"
    echo "  15) 项目信息"
    echo "   0) 退出"
    echo -e "${D}  ─────────────────────────────${N}"
    read -rp "  选择: " choice

    case "$choice" in
      1) svc_start   && echo -e "\n  ${G}已启动${N}"; pause ;;
      2) svc_stop    && echo -e "\n  ${Y}已停止${N}"; pause ;;
      3) svc_restart && echo -e "\n  ${G}已重启${N}"; pause ;;
      4) echo; svc_logs 40; pause ;;
      5) list_tunnels; pause ;;
      6) show_info; pause ;;
      7) tunnel_hint; pause ;;
      8) change_port; pause ;;
      9) reset_password; pause ;;
      10) reset_basepath; pause ;;
      11) manage_cert; pause ;;
      12)
        if svc_is_enabled; then
          svc_disable
          echo -e "\n  ${Y}已关闭开机自启${N}"
        else
          svc_enable
          echo -e "\n  ${G}已开启开机自启${N}"
        fi
        pause ;;
      13) do_update; pause ;;
      15) show_links; pause ;;
      14) do_uninstall; pause ;;
      0) exit 0 ;;
      *) ;;
    esac
  done
}

need_root

# 带参数时当普通命令用，不进菜单
case "${1:-}" in
  start)    svc_start ;;
  stop)     svc_stop ;;
  restart)  svc_restart ;;
  status)   svc_status_page ;;
  log)      svc_logs_follow ;;
  info)     show_info ;;
  list)     list_tunnels ;;
  tunnel)   tunnel_hint ;;
  update)   do_update ;;
  cert)     manage_cert ;;
  uninstall) do_uninstall ;;
  "")       menu ;;
  *)
    echo "用法: f [start|stop|restart|status|log|info|list|tunnel|update|cert|uninstall]"
    echo "不带参数进入交互菜单"
    ;;
esac
