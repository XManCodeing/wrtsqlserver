# OpenWrt 环境基础与 Docker 外接存储交接

> 最后更新：2026-11-02 11:17（UTC+8），补充云端构建决策及本地流程准备。
> 信息来源：用户在 OpenWrt SSH 终端手动执行命令后提供的输出，以及本项目代码与本地测试。AI 未连接或操作路由器。
> 第 2～8 节保留历史过程；当前状态与后续操作以第 11～14 节为准。快照不代表后续状态，先确认是否有变化。

## 1. 用户目标与操作边界

用户希望在 x86 OpenWrt 软路由的 Docker 中部署数据库 TCP 转发服务，利用现有 SOCKS5／HTTP 代理访问远程 VPS 数据库。

当前已完成转发程序、管理界面、独立 Redis 部署脚本和本地测试，正在准备首次真实部署。OpenWrt 尚未拉取镜像、创建容器。用户选择 GitHub 私有仓库 + Actions 在线构建 + GHCR，用户通过 SSH 手动拉取、部署；不用 Docker Desktop，也不依赖 OpenWrt 后台管理器。

必须遵守：

- 用户非常关注路由器稳定性，要求用户自己执行命令，AI 只协助分析和逐步指导；不得擅自 SSH、操作管理页面或远程执行。
- 不承诺零风险；在写盘、修改配置、启停服务之前说明影响和恢复方式。
- 用户不愿修改内置系统盘，已选择外接 U 盘承载 Docker 数据。
- 不对系统盘分区、格式化、扩容，不做 extroot，不迁移 OpenWrt 系统。
- 不擅自重启路由器、网络、防火墙或 Docker，不升级固件或 Docker。
- 不一次下发大批修改命令；分阶段执行，核对输出后再继续。
- 不读取或索要密码、私钥、订阅链接、节点凭据；配置备份可能含敏感信息，不要求用户发回。
- 设备名可能因重启、拔插而改变；任何破坏性操作前重新核对型号、容量、分区与挂载状态。
- 此次 U 盘格式化已完成，不能在新对话中再次要求格式化。

## 2. 基础硬件与系统

已确认：

- 终端提示符：`root@OpenWrt:~#`。
- 架构：x86_64，Docker 报告 `linux/amd64`。
- CPU：4 个逻辑 CPU，具体型号未知。
- Docker 报告内存：8,273,719,296 字节，约 7.7 GiB。
- 当时运行时间：122 天 3 小时 55 分钟。
- 当时负载：`0.15, 0.18, 0.09`，仅为一个时点快照。
- 未配置活动交换空间：`/proc/swaps` 只有表头。
- `lsblk` 不存在；`fdisk`、`block`（尚待实际调用确认）、`mkfs.ext4` 的情况见后文。

`free -m` 实际输出：

```text
              total        used        free      shared  buff/cache   available
Mem:        8079804      261900     7393728       22984      424176     7688884
Swap:             0           0           0
```

这些数值与 KiB 口径吻合，不应按 MiB 解读为数 TB 内存。可用内存当时约 7.3 GiB。

未知：OpenWrt／衍生固件具体版本、内核版本、主路由或旁路由、拨号职责、网卡信息、代理插件和内核版本、其他业务负载、现场恢复能力。

## 3. Docker 环境

已执行 `command -v docker`、`docker version`、`docker info`、`docker ps -a`。

- Docker 路径：`/usr/bin/docker`。
- Client 和 Server 均可正常返回版本，服务端可连接。
- Docker Engine：20.10.17，API 1.41，最低 API 1.12。
- Go：go1.20.2。
- Client Git commit：100c701；Server Git commit：a89b842。
- 构建时间：2023-06-30。
- containerd：1.6.6。
- runc：1.1.2。
- docker-init：0.19.0。
- Context：default。
- Client Experimental：true；Server Experimental：false。
- Storage Driver：`vfs`。
- Docker Root Dir：`/opt/docker`。
- Cgroup Driver：`cgroupfs`；Cgroup Version：1。
- `docker ps -a` 只有表头，没有容器。
- 用户表示此前未部署过镜像；未独立检查镜像、卷、构建缓存清单，不能据此保证数据目录完全为空。
- `docker compose version` 报告无 compose 子命令。
- `command -v docker-compose` 无输出。

注意：

- 版本较旧，当前没有升级计划或授权。
- 容器实际启动能力、资源限制效果、网络行为均未验证。
- `vfs` 通常比 `overlay2` 更占空间；没有修改驱动，也未验证外接盘上是否能使用 `overlay2`。
- 不要把“服务端可连接”写成“Docker 全部验证通过”。

## 4. 内置系统盘：不得改动

当时设备：`/dev/sda`。

- 型号：`SSD 128GB`。
- 容量：128,035,676,160 字节，119.24 GiB。
- 扇区数：250,069,680；逻辑／物理扇区均 512 字节。
- 分区表：DOS／MBR。
- Disk identifier：`0x9d36a790`。

分区布局：

```text
Device     Boot  Start     End Sectors  Size Id Type
/dev/sda1  *       512  131583  131072   64M 83 Linux
/dev/sda2       132096 1180671 1048576  512M 83 Linux
```

`sda2` 后约 118.68 GiB 未分配到分区；不能保证其中无历史数据。用户明确选择不利用这部分空间，避免修改系统盘分区表。

关键挂载：

- `/dev/root` → `/rom`，squashfs，只读；164MB、100% 属只读固件的常见表现。
- `/dev/loop0` → `/overlay`，f2fs；总约 346MB，已用 259.6MB，可用 86.4MB。
- `overlayfs:/overlay` → `/`，系统可写叠加层。
- `overlayfs:/overlay` → `/opt/docker`，与系统使用相同 upperdir/workdir，Docker 当前仍消耗系统可写空间。
- `/dev/sda1` → `/boot`，ext4；挂载表中 `/boot` 条目重复出现，原因未调查。
- `/dev/sda1` → `/mnt/sda1`，ext4。
- `/tmp` 为 tmpfs，约 3.9GB，不是持久磁盘。
- `loop0` 不是独立物理硬盘，不能与 sda 容量相加。

风险：只有约 86.4MB 系统可写余量，在迁移与保护完成前禁止拉取镜像或创建容器。

## 5. 外接 U 盘：已完成格式化和临时挂载

身份核验结果：

- 当时整盘设备：`/dev/sdb`；分区：`/dev/sdb1`。
- Vendor：SanDisk。
- Model：Ultra USB 3.0。
- 标称 32GB，实际 30,752,636,928 字节，28.64 GiB。
- 60,063,744 个 512 字节扇区。
- DOS 分区表；Disk identifier：`0xbc0e360e`。
- `sdb1`：Start 2048，End 60063743，Sectors 60061696，约 28.6GiB。

原状态：

- 用户曾将其用作 Hive OS 启动盘。
- 原文件系统为 NTFS，标签 `HIVE`。
- 原挂载位置 `/mnt/sdb1`，驱动 ntfs3。
- 原分区类型 ID 为 7（HPFS/NTFS/exFAT）。
- 用户已明确确认这只 U 盘全部数据可以清除。

已由用户依次执行：

1. `umount /mnt/sdb1`，成功，随后检查挂载表确认已卸载。
2. `mkfs.ext4 -L docker-data -m 1 /dev/sdb1`，确认 NTFS 覆盖提示后格式化成功。
3. `mkdir -p /mnt/docker-usb`。
4. `mount -t ext4 -o noatime /dev/sdb1 /mnt/docker-usb`，成功。

格式化结果：

- mke2fs 1.46.5（30-Dec-2021）。
- 标签：`docker-data`。
- UUID：`d7f45318-dc5b-49db-8250-defb536790c6`。
- ext4，4KB 块，7,507,712 blocks，1,880,480 inodes。
- 保留空间比例 1%；日志、inode 表、超级块创建均成功，无错误输出。
- 未修改分区表，原 ID 7 尚未更改；不要据分区类型描述误判仍为 NTFS。
- `/usr/sbin/mkfs.ext4` 已存在；`/proc/filesystems` 包含 ext4，且实际挂载已成功。

最新挂载与空间：

```text
/dev/sdb1 /mnt/docker-usb ext4 rw,noatime 0 0
Filesystem                Size      Used Available Use% Mounted on
/dev/sdb1                28.0G     24.0K     27.7G   0% /mnt/docker-usb
```

当前挂载最初通过手动命令建立；23:49 已保存专用 UUID 持久挂载配置，但未验证开机自动挂载。没有迁移 Docker，没有磁盘压力测试。普通 U 盘耐久性、掉盘风险仍存在，不能与 SSD 等同。

## 6. 持久挂载配置

最初只读查询的 `uci show fstab`（以下系统条目未改动）：

```text
fstab.@global[0]=global
fstab.@global[0].anon_swap='0'
fstab.@global[0].anon_mount='1'
fstab.@global[0].auto_swap='1'
fstab.@global[0].auto_mount='1'
fstab.@global[0].delay_root='5'
fstab.@global[0].check_fs='0'
fstab.@mount[0]=mount
fstab.@mount[0].target='/overlay'
fstab.@mount[0].uuid='6b62e3a6-9e5c-4f68-95d1-b08ca44dc2dc'
fstab.@mount[0].enabled='1'
fstab.@mount[1]=mount
fstab.@mount[1].target='/boot'
fstab.@mount[1].uuid='84173db5-fa99-e35a-95c6-28613cc79ea9'
fstab.@mount[1].enabled='1'
fstab.@mount[2]=mount
fstab.@mount[2].target='/rom'
fstab.@mount[2].uuid='36f2a931-0474621e-ed30959c-a2c2fa56'
fstab.@mount[2].enabled='1'
```

23:49 用户已新增并执行 `uci commit fstab` 保存以下配置，随后 `uci changes fstab` 无输出，`uci show fstab.docker_usb` 核对一致：

```text
fstab.docker_usb=mount
fstab.docker_usb.target='/mnt/docker-usb'
fstab.docker_usb.uuid='d7f45318-dc5b-49db-8250-defb536790c6'
fstab.docker_usb.fstype='ext4'
fstab.docker_usb.options='noatime'
fstab.docker_usb.enabled='1'
```

只保存配置，未执行 `block mount`、服务重启或路由器重启；开机挂载尚未验证。匿名自动挂载仍开启，不应擅自关闭全局自动挂载或覆盖其他条目。

## 7. Docker 配置及启动脚本的重要行为

已查看 `/etc/init.d/dockerd` 全文；本文记录关键行为，不复制整个脚本。

`uci -q get dockerd.globals.data_root` 返回：

```text
/opt/docker/
```

启动机制：

- `USE_PROCD=1`，`START=99`。
- 生成配置目录 `/tmp/dockerd`，配置文件 `/tmp/dockerd/daemon.json`。
- `boot()` 先调用 `uciadd`，再通过 `rc_procd start_service` 启动。
- `uciadd` 可以新增／修改 Docker 网络接口、docker0 网桥、防火墙 zone，提交 network/firewall 并调用 `reload_config`。
- `ucidel` 可以删除相关网络、防火墙配置，同样会 reload；不要随意调用。
- `start_service()` 先执行 `process_config`，再通过 procd 启动 `/usr/bin/dockerd --config-file=...`。
- 原脚本没有 U 盘检查；2026-11-02 00:03 已在 `boot()`、`start_service()`、`reload_service()` 开头接入 `/bin/sh /root/docker-usb-mount-check.sh`。失败时记录日志并返回 1，`boot()` 的检查在 `uciadd` 之前。`START=99` 不等于挂载成功保证。
- 设置了 nofile 限额；没有在该脚本中看到 procd respawn 设置。
- `reload_service()` 重新处理配置并向 dockerd 发信号；配置了 dockerd UCI reload trigger。

配置生成：

- 若 `dockerd.globals.alt_config_file` 非空且对应文件存在，优先链接该文件，跳过常规 UCI 配置生成。
- 23:45 已查询 `alt_config_file`，无输出，未发现非空替代配置路径；修改后仍需核对实际生成配置及运行状态。
- 常规路径读取 data_root，默认 `/opt/docker/`。
- `iptables` 默认启用；还处理 bip、DNS、监听 hosts、镜像源、IPv6 等。
- 可按 firewall 配置添加 DOCKER-USER 阻断规则。
- 此脚本常规生成逻辑未提供 storage-driver 字段，不能假设任意 UCI storage_driver 选项会生效。

安全含义：不只是 `boot()`，Docker 守护进程启动本身及配置处理也可能影响防火墙／容器网络；不能保证 restart 不影响网络。

## 8. 最新执行状态（2026-11-02 08:43 更新）

**用户于 08:41 表示已准备好并继续；08:42 手动执行一次 `/etc/init.d/dockerd restart`，挂载检查 PASS；08:43 确认 Docker 服务端可连接，数据根目录已切换至 U 盘。没有重启软路由。**

实测：

```text
DockerRootDir=/mnt/docker-usb/docker StorageDriver=overlay2
Filesystem                Size      Used Available Use% Mounted on
/dev/sdb1                28.0G    344.0K     27.7G   0% /mnt/docker-usb
```

- 新数据目录已经实际生效，不再是仅保存配置。
- 未手动指定新存储驱动，Docker 在新目录使用 `overlay2`，此前为 `vfs`。
- SSH 可继续执行查询；普通上网、原代理和后台是否正常尚待用户确认，不能据 SSH 可用宣布网络全部正常。
- 旧 `/opt/docker` 未删除。没有执行镜像或容器测试。

以下网络检查为重启前快照，不能当作重启后状态：

- 只有默认 bridge、host、none 网络；默认 bridge 元数据网段为 `172.17.0.0/16`，网关 `172.17.0.1`，但系统实际不存在 `docker0`，原因未确认。
- LAN 网段显示为 `192.168.10.0/24`；公网 IP 用户已脱敏，不能当作真实地址记录或用于连接。
- 存在 `fwmark 0x1` 查表 100 的策略，表 100 为 `local default dev lo`，可能用于透明代理，不修改。
- `dockerd.globals.bip` 无输出，`dockerd.globals.iptables=1`。
- `net.ipv4.ip_forward=1`；FORWARD 默认 DROP，前部已有 DOCKER-USER、DOCKER-ISOLATION-STAGE-1 和 docker0 规则，后部为 fw3 转发规则。DROP 不代表所有流量被阻断，不得擅自改 ACCEPT 或清空规则。
- 后续启动可能重建 docker0 并调整防火墙，因此仍存在网络中断风险。

历史记录：用户曾于 00:19 暂停，后于 08:41 明确继续。保持 U 盘连接，不能运行中拔盘。旧数据目录保留用于后续核对与回退。

- 已创建 `/mnt/docker-usb/docker`（mkdir 指定权限 700），并提交 `dockerd.globals.data_root='/mnt/docker-usb/docker'`。
- 提交后曾确认运行目录仍为 `/opt/docker`；08:43 已确认切换为 `/mnt/docker-usb/docker`。

新增验证与改动：

- `docker image ls`、`docker volume ls` 均仅表头，无镜像、数据卷；此前无容器。
- `/usr/bin/mountpoint`、`/usr/bin/readlink`、`/sbin/block` 存在；`diff` 不存在。
- `/etc/init.d/fstab enabled` 返回 0，服务已启用开机启动，未实际重启验证。
- U 盘随机小文件创建、写入、读取和删除测试通过；不是压力测试或持久落盘保证。
- `/root/docker-usb-mount-check.sh` 已保存，权限 600。检查挂载点、实际挂载设备 UUID、ext4 和 rw；默认检查 `/mnt/docker-usb`，可传其他路径供独立测试。
- 独立检查：正式路径 PASS、退出码 0；`/tmp` 路径 FAIL、退出码 1。尚未测试错误 UUID 分支或实际缺盘启动。
- 候选文件 `/tmp/dockerd.usb-candidate` 经语法检查及全文人工核对，仅三处新增检查。
- 已检查原启动脚本备份非空、语法通过，再通过 `/etc/init.d/.dockerd-usb-new` 同目录替换正式 `/etc/init.d/dockerd`，权限 755；输出 `PASS: startup guard installed; Docker was not restarted`。
- 停止逻辑未改；安装检查时未重启，08:42 已单独重启一次 Docker。保护只是入口检查，不覆盖直接运行 dockerd、检查后挂载变化、运行中掉盘或固件升级覆盖脚本等情况。

用户已返回以下执行结果：

- 三条 `cp -ip` 备份命令已执行且无报错，目标分别为 `/root/fstab.before-docker-usb`、`/root/dockerd.before-docker-usb`、`/root/dockerd-init.before-docker-usb`；尚未单独校验文件内容或确认下载到电脑。
- `uci changes fstab`、`uci changes dockerd` 均无输出，没有待提交修改。
- `uci -q get dockerd.globals.alt_config_file` 无输出，未发现非空替代配置路径。
- `block info /dev/sdb1` 成功，确认 UUID 为 `d7f45318-dc5b-49db-8250-defb536790c6`，LABEL 为 `docker-data`，TYPE 为 `ext4`，MOUNT 为 `/mnt/docker-usb`。

以下仍未执行或未确认：

- 用户已表示在后台系统菜单做了配置备份；备份时间、下载保存位置及包含范围未核对，不等同整盘镜像。
- U 盘持久挂载项已新增并提交，开机自动挂载尚未验证。
- Docker data_root 已实际切换为 `/mnt/docker-usb/docker`，当前使用 overlay2。
- 缺盘启动检查已安装且正常启动路径通过，但未做服务入口的实际拒绝启动验证；未实现运行中掉盘处理，未验证开机挂载。
- 已单独重启一次 Docker；未重启路由器、未拉取镜像、未创建容器。
- 未测试 HTTP／SOCKS5 到数据库的连通性。

## 9. 历史准备顺序（当前部署顺序见第 13 节）

以下为早期准备计划，迁移和启动保护已经完成的部分不重复执行：

1. 确认设备和挂载状态未变化，完成必要备份并保存到电脑。
2. 检查 UCI 待提交修改、Docker 替代配置及 U 盘 UUID。
3. 专用 UUID 挂载项已保存，不重复新增；后续择机验证自动挂载，不覆盖现有 fstab。
4. 设计并验证缺盘保护：挂载点必须确为指定外接文件系统，否则拒绝启动 Docker，避免写入系统盘同名目录。
5. 分清“启动时缺盘检查”和“运行中掉盘处理”：前者不能代替后者。不得以运行中拔 U 盘作为首个测试。
6. 再规划 Docker 数据目录迁移、存储驱动选择、有限日志及资源限制；先核实镜像／卷是否需要保留。
7. 单独评估 Docker 网络方案与现有路由／代理规则，避免路由回环、意外公网暴露及防火墙变化。
8. 最后进行有限规模容器验证，不能把挂载成功等同于容器部署全部通过。

任何配置改动均应有回退步骤；不以重启整个路由器作为默认生效手段。后续固件升级可能覆盖启动脚本或改变设备识别，需要重新检查保护措施。

## 10. 数据库转发服务需求与本机开发

用户最新确认：

- 代码位于本项目 `src/`，前端 `src/frontend`，后端 `src/backend`。
- Vue 3 + TypeScript + Element Plus；Go 后端（用户允许 Node/Go，由实现选择 Go）。
- Apple 官网式克制风格，中性灰白、蓝色主操作、10px 圆角、中等偏紧凑 PC 表格；默认浅色，支持深色／跟随系统。详见根 `AGENTS.md`。
- 网页配置 SOCKS5 代理与多条端口映射，提供登录、状态和手动连通性测试。
- 显式端口映射而非透明截获：一个监听入口对应一个固定数据库目标；后端项目改连路由器入口。多个数据库可使用不同入口端口。
- 首版使用 SOCKS5，不自动修改 OpenWrt 路由、防火墙，不自动扩展 Docker 发布端口。
- 本机开发不等于已部署到 OpenWrt。路由器的实际代理地址、认证、数据库目标仍待用户配置。
- 第一版已实现：认证与 CSRF、SOCKS5 配置和凭据加密存储、多映射监听与来源限制、启停/删除、连接测试、运行状态、统一明暗主题。
- 构建入口：`src/frontend` 执行 `npm ci` 和 `npm run build`；`src/backend` 执行 `go test -race ./...`、`go vet ./...`、`go build .`。
- 后端运行参数：`ADMIN_PASSWORD_FILE`（优先，文件中至少12字节密码）或 `ADMIN_PASSWORD`；`LISTEN_ADDR` 默认 `127.0.0.1:8080`；`APP_DATA_DIR` 默认 `./data`；`WEB_DIR` 默认 `./web`；`MAX_CONNECTIONS` 默认1024，部署模板限制128。
- 数据目录内 `secret.key` 与 `config.enc` 必须一起备份；本地密钥与密文同盘并不能防止获得整个数据目录的人解密，仍依赖文件权限和设备访问控制。
- `src/Dockerfile` 多阶段构建单镜像，`src/compose.yaml` 为待审部署模板，并未在路由器运行；路由器尚无 Compose，不能直接照搬执行。
- 模板采用非root、只读根文件系统、cap_drop ALL、CPU/内存/PID/日志限制。管理入口为指定LAN地址18080，映射预留3306–3310；容器内映射监听0.0.0.0且须配置来源CIDR。网页新增端口不会自动发布Docker端口。
- 管理端当前为HTTP，密码和配置在传输中不加密，只供受信网络或安全隧道访问；不能向公网开放。反向代理TLS接入尚需单独适配同源校验，不宣称已支持。
- 已通过本机 Go race测试、vet、Linux amd64交叉编译和前端类型/生产构建；本机未安装Docker，镜像构建和容器限制未实测。
- 使用指定浏览器实看登录页浅/深色；因登录凭据未提供，未继续登录，不声明后台表格、编辑弹窗、1280px及跟随系统模式完成视觉验收。截图在被忽略的 `src/.local/`，临时预览服务已停止。
- 没有对真实代理/VPS做连接测试，没有部署或更改OpenWrt。

- 用户已表示 OpenWrt 提供可调用的 HTTP 和 SOCKS5 代理；地址、端口、认证方式均未提供。
- 设想：多端口 TCP 转发，每个监听入口对应远程数据库目标，出站优先走 SOCKS5。
- HTTP 方式需确认 CONNECT 及数据库目标端口放行，不是发送 HTTP SQL 请求。
- 不解析 SQL，不自动重放查询或事务，数据库密码仍由应用驱动管理。
- 长连接需考虑超时、Keepalive、并发上限、日志脱敏和退出清理。
- 首版已确定 Go + Vue 3 单镜像；最终监听地址、发布端口和真实数据库目标需在部署前核对。
- TLS 域名校验、MongoDB 副本集／Redis Cluster 节点发现等仍需按实际数据库确认。

## 11. 新 AI 对话的起点

当前准确状态（2026-11-02 11:17）：

- Docker 20.10.17 已使用 `/mnt/docker-usb/docker`、overlay2；USB UUID 正确，ext4 rw，可用约 27.7GB。原 `/opt/docker` 保留，禁止重新迁移或格式化。
- LAN 已确认 `br-lan`、`192.168.10.1/24`，docker0 为 `172.17.0.1/16`。机器承担 PPPoE WAN，公网地址不写入本文。
- 11:04 监听快照未发现 18080、13306、6379 占用；宿主机 8080 已被占用，但管理容器内部 8080 映射到 LAN 18080，不要求占用宿主机 8080。启动前再检查。
- `/proc/meminfo`：MemTotal 8079804 kB，MemAvailable 7696788 kB，SwapTotal 0 kB。约 7.7 GiB 总内存、7.3 GiB 可用，不按 `free -m` 标题误读单位。
- 系统可写层仅约 86.3MB 余量；构建包、凭据、镜像、服务数据放到已核验 USB，不能写到 `/root` 或系统盘作为长期存储。
- 用户 11:05 确认 `docker image ls` 空；此前 `docker ps -a` 空。尚无真实容器测试，未验证资源约束、LAN 访问或镜像仓库网络。
- 主机和 BusyBox 均没有 stat；已确认 docker、ip、awk、mountpoint、readlink、block、wget、tar、sha256sum 可用。更新还需要 cmp，尚待路由器检查。
- USB 开机挂载与缺盘启动的真实验证仍未完成；运行中掉盘保护未实现。不得通过运行中拔盘测试。
- 已有本地页面预览与前后端测试，但不等于路由器部署验证。Redis 页面浅/深色空状态看过；真实键值、清库及 1280px/跟随系统等完整视觉验收未完成。
- 用户已创建私有仓库 `https://github.com/XManCodeing/wrtsqlserver`，本机 gh 已核对 Private/ADMIN。远程初始 README 和历史保留，仓库根仅限本项目。
- 用户于 11:22 明确授权提交、推送和首次构建。首发目标 `ghcr.io/xmancodeing/wrtsqlserver:1.0.0`；是否构建成功以 Actions 实际结果为准，未授权自动部署路由器。

## 12. Docker 自绑定挂载与缺失 stat 的兼容

用户提供的 `/proc/self/mountinfo`：

```text
46 23 8:17 / /mnt/docker-usb rw,noatime - ext4 /dev/sdb1 rw
44 46 8:17 /docker /mnt/docker-usb/docker rw,noatime shared:1 - ext4 /dev/sdb1 rw
```

这是同一 USB 上 `/docker` 子目录的自绑定形式，不是额外系统盘。不得卸载该挂载，不重启 Docker 来“修复”。

本地 `src/deploy/common.sh` 已调整：

- 保留 UUID、rw/ext4、设备、权限、符号链接及 Docker 数据根检查。
- 仅检查精确 Docker 根路径时，允许 mountinfo 中唯一 USB 根和唯一直接子挂载，要求相同设备号和来源、子根 `/docker`，父子关系正确、ext4 且可读写。
- 其他业务目录中的嵌套挂载仍拒绝；不能把自绑定例外扩展到 Redis 或转发数据。
- stat 缺失时调用脚本旁的 `deploy-stat`。源码 `src/backend/cmd/deploy-stat`，Go 静态 linux/amd64 小工具，仅读取白名单元数据；不安装到系统目录，不执行挂载或网络操作。
- 云端部署附件会包含该工具；本地编译二进制在 Git 忽略列表，避免提交生成物。
- 本地 Go race/vet、挂载伪造与缺失 stat 测试、部署隔离回归通过；工具尚未在路由器实际运行。

## 13. 私有仓库、在线构建和首次手动部署

### 13.1 仓库和构建流程

仓库根必须是 `tools/opernwrtdocker`，不是整个 `/Volumes/OMG/code`，也不是只上传 `src`。用户自行创建空的 Private 仓库，不必预先生成 README、License 或 gitignore。

本地已准备 `.github/workflows/publish.yml`：

- 推送 `v1.0.0` 这样的 tag，或在 Actions 手动输入 `1.0.0` 触发；只接受三段数字版本。
- 普通分支 push 不自动发布，不使用 latest。每次发布用新版本；标签本身不是不可变保证，禁止重用版本，部署同时核对 digest。
- Node 22 前端测试/生产构建，Go race/vet，Python 部署 mock 测试必须通过后才发布。
- 使用 `src/Dockerfile` 构建 linux/amd64，镜像为 `ghcr.io/<小写owner>/<小写repo>:<版本>`。
- 使用运行时 GITHUB_TOKEN 的 packages:write 推送，不在仓库保存个人 token；未授予路由器访问权限，不包含 SSH 部署步骤。
- 为旧 Docker 20.10 关闭 provenance/sbom 附加清单；实际拉取兼容性仍须测试。
- Actions 附件 `db-relay-deploy-<版本>` 含脚本包、SHA256SUMS、IMAGE.txt（tag 与 digest）。保留 30 天，用户要下载长期留存。
- 脚本包只含 common.sh、redis-init.sh、relay-init.sh、relay-update.sh、redis.conf、deploy-stat；不含密码、业务数据或镜像。
- Actions 附件不是网页上传更新包；网页更新包另需专用 Ed25519 签名，禁止混用。

上传前检查：node_modules、dist、.local、.env*、数据目录、日志、密码、私钥、Docker 登录配置、本地截图及压缩包均不上传。忽略规则不能代替人工核对暂存清单和 diff。禁止读取凭据文件来核对内容，也不要把其他项目加入仓库。

### 13.2 私有镜像与凭据

- Private 代码仓库和 GHCR 包的访问权限分别核对，不能默认等同；首次发布后检查包保持 Private，绑定正确仓库，Actions 有写权限。
- 路由器只使用具备 read:packages 的拉取凭据（按 GitHub 账户/组织政策配置，必要时授权 SSO），不使用写包 token。组织权限限制或额度问题需要用户处理，不擅自公开镜像。
- 登录使用 password-stdin；密码不放命令行、脚本或聊天。Docker 登录配置默认不是加密保险箱，USB 凭据目录 0700、文件 0600。
- 在确认 USB 挂载后，用户设置 DOCKER_CONFIG 指向 USB 专用受限目录；不得默认把凭据存入 `/root/.docker`。
- 私有仓库 Actions 的免费额度和计费按账户实际配置核对，不承诺无限免费。

### 13.3 首次部署顺序（逐步指导，不整段自动执行）

1. 本地云端构建成功，用户保存与版本配套的附件、digest；确认 OpenWrt 上网、原代理和管理界面基线正常。
2. 重新确认 USB UUID、mountinfo、DockerRootDir、空间、LAN、端口和必要工具。缺少挂载即停止，不创建同名系统盘目录。
3. 上传脚本附件到 USB 新的版本目录，校验 SHA256SUMS 再解压。测试 deploy-stat，只运行只读检查，不先 start。
4. 确认能访问 GHCR 和 Docker Hub；设置 USB 内只读拉取凭据，用户手动拉取应用固定版本和 `redis:7.4.7-alpine`，检查平台和应用 RepoDigests 与 IMAGE.txt 一致。网络失败先停，不自动修改 DNS、代理插件或防火墙。
5. 公共变量为 LAN_INTERFACE=br-lan、LAN_BIND_IP=192.168.10.1、LAN_INTERFACE_CONFIRMED=yes。明确这是已确认 LAN，不是因为地址私有就默认安全。
6. Redis 使用 REDIS_LAN_ACCESS_CONFIRMED=yes，先 redis-init.sh check，再单独 start。确认 6379 的 PING、持久化路径、容器资源及网络状态。
7. 用户在 USB 私有目录准备管理密码文件（至少 12 个非空白字节、父目录 root:0700、文件 root:0600），不发送内容。配置 ADMIN_PASSWORD_SOURCE 及完整版本 RELAY_IMAGE，先 relay-init.sh check，再 start。
8. 访问 `http://192.168.10.1:18080` 登录；容器转发监听设为 `0.0.0.0:13306`，来源 CIDR 明确配置，开发服务连接 `192.168.10.1:13306`。真实代理地址和数据库目标由用户填写，不猜测已有监听端口用途。
9. Redis 地址为 `192.168.10.1:6379`、默认无密码；仅在无业务的测试库使用临时键验证状态/键值列表/TTL/单库清空。清空整个所选库，不是筛选结果，禁止拿业务库试验。
10. 验收真实连接、内存/CPU/日志限制与原网络业务；失败保留现场，按单服务处理，不重启整个路由器或 Docker。

容器固定为 `--restart no`，首次测试不自动开机启动。后续若需要自动恢复，单独设计 USB 检查顺序，不能擅自改 always。

安全边界：脚本不会直接执行路由/防火墙命令，但 Docker 发布端口本身可能新增 NAT 规则；不能承诺“不影响任何网络规则”。绑定 LAN IP 不等于来源 ACL，无密码 Redis 必须处于受信网络；不要据此宣称所有 WAN 路径已封堵。

## 14. 后续更新、回退与独立 Redis

### 数据边界

- Redis：容器 redis-dev，数据 `/mnt/docker-usb/redis-dev/data`，配置目录独立。Redis 7.4.7-alpine、数据上限128MB/noeviction、容器256MB、RDB开启、AOF关闭；快照不是零数据丢失保证。
- 转发：容器 db-relay，`/mnt/docker-usb/db-relay/data` 保存 `config.enc` 和 `secret.key`，必须成对备份；admin_password 单独保护。
- 发布端口以 `common.sh` 为准：LAN 18080→8080 和 13306→13306。旧 compose 模板的 3306–3310 不是当前手动部署方案，不照搬。
- relay-init/update 只操作转发服务；不得重跑 redis-init 来更新转发，不执行 compose down -v、全局 prune 或清空 USB。

### 手动更新

1. 云端发布新的唯一版本，下载对应附件、核对镜像 digest，用户手动 pull 不会自动更新运行容器。
2. 先检查现有事务、回退容器与业务连接。指定新 RELAY_IMAGE、已确认 LAN 环境，运行 relay-update.sh check，只读通过后再 apply。
3. apply 会停止转发以备份一致的配置/密钥，旧容器改名 db-relay-rollback，启动候选并做健康检查；会中断转发连接，需要重新登录，内存探测历史丢失，Redis不参与。
4. 失败时脚本尝试恢复原转发容器和一致数据；恢复失败则停止并保留现场，不声称保证自动恢复。成功后旧容器、事务目录和备份仍保留。
5. 需要回退时明确执行 relay-update.sh rollback；可能丢弃新版期间的配置修改，先确认影响。回退不应触碰 Redis。
6. 保留的 transaction 或回退容器会阻止下次更新，这是安全限制。下一次更新前先核对、备份并制定显式归档步骤，当前没有一键确认/清理旧事务功能，不随意 rm -rf。

网页“程序更新”目前只支持上传/验签/暂存专用签名包，不会执行部署，不会从 GHCR 自动升级。当前选择的常规发布路径是 GHCR + SSH 手动部署，网页更新不是必须步骤。

### 尚未验证

GitHub 私有仓库已对接，Actions 正在准备首次触发，GHCR 首次发布和私有拉取未验证；OpenWrt 从未启动本项目容器。真实资源限制、Docker 健康检查、更新/回退、Redis 数据持久化和网络隔离均须逐步实测。本地测试只代表代码和模拟场景通过。
