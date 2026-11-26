# wrtsqlserver

## Ubuntu：映射端口自动放行

适用于 `/www/wwwroot/db-relay` 的 host 网络部署。先升级到包含监听快照的新业务镜像，再将同版 Ubuntu 部署包解压到 root 管理的目录，以 root 在解压目录运行 `sh install-ubuntu-firewall.sh`。安装一次即可；仅在后台更新业务镜像不会自动安装宿主服务。无需重建 Redis，也不要清空原 UFW 规则。

安装器从已有 `data/ubuntu.vars` 的 `LAN_BIND_IP` 和 Ubuntu 网卡的实际前缀确定放行范围。它要求 UFW 已启用，不会替你开启 UFW。映射启停、改端口或改白名单后，systemd 监听快照变动，并每 30 秒补查一次。实际放行范围不超过映射白名单与本机局域网的交集。只处理非特权 TCP 端口（1024–65535），保留管理端口及 6379、8080、18080；不接管 SSH、Redis 或其他进程的监听。

验证时，先启用一条新端口映射，再运行 `sudo ufw status`，从白名单内的另一台电脑连接该端口；随后停用映射，确认程序创建的放行规则消失。手工规则不会随映射停用而删除。排错用 `journalctl -u db-relay-firewall.service -n 30 --no-pager`。

撤销接管：先执行 `sudo systemctl disable --now db-relay-firewall.path db-relay-firewall.timer` 和 `sudo systemctl stop db-relay-firewall.service`，再清理程序规则。当前小主机对应的清理命令为 `sudo /usr/bin/python3 -I -B /usr/local/lib/db-relay/ubuntu-firewall.py --project-dir /www/wwwroot/db-relay --lan-ip 192.168.10.10 --lan-cidr 192.168.10.0/24 --cleanup`；其他机器使用 `/etc/db-relay-firewall.conf` 中的实际地址和网段。
