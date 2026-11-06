# backup-tool

MySQL 备份小工具，统一使用本机 `mysqldump`，通过数据库地址、端口和账号连接本机或远程 MySQL。Docker 中的 MySQL 通过可访问的映射端口连接。备份保存到运行本工具的机器，可选同步到 OSS，按天清理本机旧备份。错误发钉钉，普通成功不发，每周发送一次汇总。

## Linux 安装和运行

以下以 Linux 服务器、Bash、安装到 `/opt/backup-tool` 为例。所有命令均在 root 用户的终端中执行，安装、更新、手工运行和 cron 都使用 root。

GitHub [Releases](https://github.com/realheyu/backup-tool/releases/latest) 提供 Linux amd64/arm64 的静态二进制、运行包和 `SHA256SUMS`。推荐下载 `.tar.gz` 运行包，包含：

```text
backup-tool                  程序
backup-tool.example.toml     配置模板
README.md                    说明
LICENSE                      项目许可
THIRD_PARTY_NOTICES.txt       第三方许可
```

Linux x86_64 下载 amd64 运行包，Linux aarch64 下载 arm64 运行包。用 `uname -m` 查看架构。运行不需要安装 Go；运行本工具的机器需要安装与源数据库兼容的 MySQL 客户端，并能执行 `mysqldump --version`。客户端未加入 PATH 时，可在 `[mysql]` 的 `dump_binary` 中填写可执行文件的绝对路径。

在 Linux 服务器上先确认架构和客户端：

```bash
id -u  # root 应输出 0
uname -m
mysqldump --version
```

`install` 可以同时复制文件和设置权限，程序使用 `755`，包含密码的配置使用 `600`，无需再单独执行 `chmod`。

### Debian 安装 MySQL 客户端

客户端优先与源数据库使用相同系列。以下以 MySQL 8.0 源库、Debian 12（`bookworm`）、x86_64（`amd64`）、root 用户为例，安装包含 `mysqldump` 的 MySQL 8.0 客户端，安装位置是运行备份工具的机器。

此示例适用于 [Debian 12 的官方 MySQL 8.0 仓库](https://repo.mysql.com/apt/debian/dists/bookworm/mysql-8.0/binary-amd64/Packages)。[Debian 13 的官方仓库](https://repo.mysql.com/apt/debian/dists/trixie/Release)目前未提供 MySQL 8.0 系列，不适用此安装示例。

本工具使用 Oracle MySQL 的导出参数（包括 `--set-gtid-purged=OFF`）。Debian 的 [`default-mysql-client`](https://packages.debian.org/trixie/default-mysql-client) 指向 MariaDB，按下面的 [MySQL 官方 APT 安装方式](https://dev.mysql.com/doc/mysql-apt-repo-quick-guide/en/)安装 `mysql-community-client`。

先添加官方仓库：

```bash
apt-get update
apt-get install --no-remove ca-certificates wget gnupg lsb-release
wget -O /root/mysql-apt-config.deb https://dev.mysql.com/get/mysql-apt-config_0.8.40-1_all.deb
dpkg -i /root/mysql-apt-config.deb
```

仓库配置界面中选择 `MySQL 8.0` 系列，再选择 `Ok`。这里选择的是软件包来源，实际安装的组件由后面的命令决定。示例仓库配置包为 `0.8.40-1`；如有更新，以[官方下载页](https://dev.mysql.com/downloads/repo/apt/)的包名为准。如果界面没有 8.0 选项，先核对 Debian 版本和仓库提供情况。

更新软件包信息，确认 `Candidate`（候选版本）为 `8.0.x`：

```bash
apt-get update
apt-cache policy mysql-community-client
```

确认版本后，仅安装客户端并检查：

```bash
apt-get install --no-remove --no-install-recommends mysql-community-client
command -v mysqldump
mysqldump --version
```

`--no-remove` 会在需要删除现有软件包时停止安装。上述命令不安装 MySQL 服务端；检查输出应显示 Oracle MySQL `8.0.x`。需要固定客户端路径时，在现有 `[mysql]` 配置中设置 `dump_binary = "/usr/bin/mysqldump"`。

### 首次安装

下载并校验运行包，下面以 Linux x86_64、首版 `v0.1.0` 为例；ARM64 将 `arch` 改成 `arm64`，升级时修改 `version`：

```bash
version=v0.1.0
arch=amd64
base="https://github.com/realheyu/backup-tool/releases/download/$version"
mkdir -p "/root/backup-tool-$version-$arch"
cd "/root/backup-tool-$version-$arch"
curl -fLO "$base/backup-tool-linux-$arch.tar.gz"
curl -fLO "$base/SHA256SUMS"
awk -v file="backup-tool-linux-$arch.tar.gz" '$2 == file' SHA256SUMS > package.sha256
test -s package.sha256 && sha256sum -c package.sha256 && tar -xzf "backup-tool-linux-$arch.tar.gz"
```

解压成功后准备配置。使用源码仓库时，先在 Linux 上按下文“修改源码”构建程序，再首次准备配置；已有配置直接沿用：

```bash
if [ ! -e backup-tool.toml ]; then
  install -m 600 backup-tool.example.toml backup-tool.toml
fi
```

下面将文件安装到 `/opt/backup-tool`，目录和新安装的文件归 root 所有。已有目标配置会保留：

```bash
install -d -m 700 -o root -g root /opt/backup-tool
install -m 755 ./backup-tool /opt/backup-tool/backup-tool
install -m 644 README.md LICENSE THIRD_PARTY_NOTICES.txt /opt/backup-tool/
if [ ! -e /opt/backup-tool/backup-tool.toml ]; then
  install -m 600 ./backup-tool.toml /opt/backup-tool/backup-tool.toml
fi
```

修改 `/opt/backup-tool/backup-tool.toml`，填入任务名、业务数据库、OSS 和钉钉等配置。只需编辑这一份配置；例如 `backup_dir = "./backups"` 对应 `/opt/backup-tool/backups`。

### 检查与运行

```bash
/opt/backup-tool/backup-tool --version
/opt/backup-tool/backup-tool --config /opt/backup-tool/backup-tool.toml --check
/opt/backup-tool/backup-tool --config /opt/backup-tool/backup-tool.toml
```

`--check` 只检查配置，不检查 `mysqldump` 是否安装、不连接数据库或更新统计；检查成功不发消息，配置错误仍尝试告警。`backup_dir` 和 `mysql.defaults_file` 的相对路径以 TOML 所在目录为准。

### 更新程序

以 root 用户进入新版运行包目录，更新程序和说明文件，沿用原配置、备份及周统计：

```bash
install -m 755 ./backup-tool /opt/backup-tool/backup-tool
install -m 644 README.md LICENSE THIRD_PARTY_NOTICES.txt /opt/backup-tool/
/opt/backup-tool/backup-tool --version
/opt/backup-tool/backup-tool --config /opt/backup-tool/backup-tool.toml --check
```

## 备份来源和密码

只提供一种备份方式：在运行本工具的机器上执行 `mysqldump`，直接连接数据库。配置中的 `host`、`port`、`user`、`socket` 都是本机客户端的连接参数。

| 数据库位置 | 连接设置 |
| --- | --- |
| 本机 MySQL | `host = "127.0.0.1"`、实际端口；也可只设置本机 `socket` |
| 远程 MySQL | 远程数据库地址、端口；需从本机可达且账号允许连接 |
| 本机或远程 Docker MySQL | 宿主机可达地址、映射后的端口，如 `13306` |

例如备份远程 MySQL：

```toml
[mysql]
host = "10.0.0.10"
port = 3306
user = "backup"
password_env = "MYSQL_BACKUP_PASSWORD"
databases = ["app"]
```

配置 `host` 或 `port` 时使用 TCP；`socket` 只用于本机，不能与 `host`、`port` 同时设置。SQL 在本机压缩为 `.sql.gz` 并校验，源数据库服务器无需安装本工具，也不会在源服务器生成备份文件。

旧配置迁移：删除 `[mysql]` 中的 `container`、`ssh_host`、`ssh_port`、`ssh_key`、`separate`，包括值为空的字段，并改填数据库连接参数和明确的数据库列表。`[sync]` 只支持 `type = "none"` 或 `"oss"`，删除其中所有 `ssh_host`、`ssh_dir`、`ssh_port`、`ssh_key` 字段。保留旧字段会报未知配置项错误。

- 已有客户端凭据：密码相关字段留空，沿用本机执行用户的客户端配置，如 `~/.my.cnf`、`.mylogin.cnf`；也可用 `defaults_file` 指定本机文件。
- 显式密码：选择 `password`、`password_env` 或运行时的 `--ask-password`。不与 `defaults_file` 同用。

```bash
/opt/backup-tool/backup-tool --config /opt/backup-tool/backup-tool.toml --ask-password
```

输入不回显，需要本机提供 `stty`；cron 中使用客户端配置、TOML 或环境变量。`password_env` 从运行本工具的本机环境读取。显式密码通过本机 0600 临时配置文件传入，不放进命令行参数，导出结束后删除。

## 备份和同步

`databases` 必须明确填写要备份的数据库，例如 `["db1", "db2"]`。固定按顺序逐库导出，每库一个压缩文件，不合并，不要求多个库快照时间一致。空列表会报错，不会自动导出全部数据库。日常备份应只列自己的业务库。

固定使用 `--single-transaction --skip-lock-tables`，不主动使用表锁或全局读锁；`single_transaction` 默认 `true`，旧配置中的 `false` 会报错。事务快照适用于 InnoDB，MyISAM 等非事务表在业务写入时不保证一致性，也不会自动切换到锁表备份。备份期间避开改表等 DDL。导出仍会消耗数据库 CPU、磁盘和网络资源，无法保证业务零影响；对主库负载敏感时可将备份来源配置为数据库副本。导出包含存储过程、事件、触发器，GTID_PURGED 固定为 OFF。这是逻辑备份，不包含服务器配置和 binlog，不支持时间点恢复。[MySQL 选项说明](https://dev.mysql.com/doc/refman/8.4/en/mysqldump.html)

同步只支持两种设置：`none` 只保存在本机，`oss` 使用内置的[阿里云官方 OSS Go SDK V2](https://github.com/aliyun/alibabacloud-oss-go-sdk-v2)上传。数据文件先传，`backup.json` 最后传；OSS 缺少该清单表示同步未完成。上传以 SDK 结果判断成功，并使用默认 CRC64 传输校验，不额外下载远端文件校验。

兼容仅上传权限：不列举、读取或删除远端对象；分片上传失败时也不自动取消上传，残留分片由 OSS 管理侧处理。

OSS 配置示例：

```toml
[sync]
type = "oss"
oss_bucket = "your-bucket"
oss_prefix = "mysql"
oss_region = "cn-hangzhou"
# oss_endpoint = "https://oss-cn-hangzhou-internal.aliyuncs.com"
```

`oss_bucket` 只填 Bucket 名称，不含 `oss://` 或路径。`oss_prefix` 是可选目录前缀，首尾 `/` 会去除，不允许空目录段、`.`、`..`、空格或 URL；留空时直接在 Bucket 根目录下按任务名组织文件。上传路径为 `oss://oss_bucket/oss_prefix/name/备份ID/文件名`。不同服务器和 MySQL 实例应使用不同 `name`。

`oss_region` 填 bucket 所在地域，默认通过 HTTPS 访问该地域的公网地址；同地域内网访问时可设置 `oss_endpoint`。大文件自动分片上传，无需安装 OSS 命令行工具。仅支持 AccessKey ID 和 Secret，默认读取 `OSS_ACCESS_KEY_ID`、`OSS_ACCESS_KEY_SECRET`；也可在本地 TOML 中成对填写 `oss_access_key_id`、`oss_access_key_secret`。TOML 凭据优先使用，不与环境变量混用。不支持 STS 临时凭据，也不读取环境变量中的临时令牌。

旧配置 `oss_uri = "oss://your-bucket/mysql"` 改成 `oss_bucket = "your-bucket"` 和 `oss_prefix = "mysql"`，并删除 `oss_uri`。旧字段 `oss_uri`、`oss_binary`、`oss_config`、`oss_session_token` 均不再支持。

## 清理和失败处理

`keep_days = 7` 删除超过 7×24 小时的本机备份，必须为 1～36500，不支持用 `0` 永久保留。进入备份流程并取得目录锁后，无论导出、同步成功还是失败，结束时都会尝试清理。保留期按目录名中的北京时间（UTC+8）计算，旧版 UTC 时间加随机值的目录也继续识别，不依赖同步标记或文件校验结果；同步失败、清单损坏或丢失、意外中断残留的 `.partial` 目录到期都会删除。

只清理当前 `backup_dir/name` 下，符合本工具命名格式的目录；目录内的文件随目录一起删除，请不要混放其他文件。旧脚本的散落文件和命名不匹配的目录不删除，符号链接不作为备份目录处理，也不会跟随目录内的符号链接删除外部文件。某个目录清理出错时继续尝试其余目录，并汇总错误。OSS 上的同步副本不自动删除。

导出失败不会保存成正式备份；同步失败的本地副本在保留期内保留，到期自动清理，不自动重传。备份、同步、清理、凭据、目录锁、统计等运行错误都会尝试发钉钉告警，备份错误和清理错误都会保留。配置检查或命令行参数错误在能读取机器人配置时也会告警。运行失败退出 1，命令行用法错误退出 2。清理随程序运行执行，需要 cron 等定期调用才能持续清理。

备份目录为 `backup_dir/name/2026-11-06_11-32-02/`，固定使用 UTC+8 的 `年-月-日_时-分-秒`，不带随机值，不受服务器本地时区影响。OSS 的备份批次目录使用同一个名称。若同一任务同一秒已有备份目录或临时目录，会报错，不覆盖已有备份；旧目录和远端对象不会自动改名。

目录内有压缩文件、记录 SHA256 的 `backup.json` 和同步成功标记 `synced.json`；无需同步时也会写该标记，它不决定保留期。不同配置请使用不同 `name`。`timeout` 是每次导出或传输的上限。数据库连接中断或导出失败时，本次不完整文件会从本地移除。强制终止或断电可能留下本机临时密码文件，需要人工确认清理。

## 钉钉通知和周汇总

在 `[dingtalk]` 中配置机器人 webhook，以及可选的加签 secret；也可通过 `webhook_env`、`secret_env` 指定本机环境变量名。

消息中的“服务器”取自运行本工具的机器的系统主机名（Go 的 `os.Hostname()`），可执行 `hostname` 查看。例如 Linux 服务器的主机名为 `backup-01`，通知中就显示 `backup-01`；数据库来源由 `[mysql]` 中的地址决定，任务由 `name` 区分。

- 错误发生后尝试发送告警，不等周汇总；一次任务中的多个备份/清理错误会一起报告。
- 普通成功不发消息。首次运行开始统计，每满 7×24 小时，在下一次执行结束时发送一次汇总，包括执行次数、成功次数、失败次数、最近成功和执行时间。无需额外增加 cron 任务。
- 汇总中的成功表示备份、同步和清理流程全部成功；通知本身出错会单独告警。有效配置下启动的失败尝试也计入统计；配置和参数错误只告警，不更新统计。
- 汇总收到钉钉成功响应后才开始新的统计周期。发送失败会告警并保留计数，下次执行重试，不会按成功次数反复发消息。

统计保存在 `backup_dir/name/notification-state.json`，只有累计计数和时间，每次覆盖更新，不保存运行明细或密码。它和 `.notify.lock` 是少量固定状态文件，不是持续增长的日志，也不会随旧备份一起清理。进程中断留下的临时统计文件会在下次更新时回收。删除状态文件会重新开始 7 天统计；状态损坏会报错告警，不会静默清空。

每次通知最多等待 10 秒。未配置 webhook 会明确报无法发送通知；配置文件无法读取、机器人不可用、断网或断电时，无法保证消息送达。告警发送失败只输出错误，避免递归发送告警。程序停止执行时也无法发送周汇总；需要保持 cron 正常运行。

## 日志

程序只输出到终端，不创建日志文件：备份进度、路径、同步结果和清理记录输出到标准输出，失败信息输出到标准错误。不配置 systemd 或额外日志服务，也不保留历史运行日志，因此没有日志文件持续增长或清理的问题。错误和每周汇总通过钉钉 Markdown 消息发送：标题、任务名、服务器和北京时间（UTC+8）统一展示，错误详情使用引用块，周汇总分开展示执行次数与时间记录。超长内容会截断，密码和密钥仍会隐藏；通知失败信息也输出到标准错误。

## 定时执行和恢复

在 Linux 服务器上以 root 手工确认可用后，执行 `crontab -e`，编辑 root 用户的定时任务，例如每天服务器当地时间 03:00 执行：

```cron
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
0 3 * * * /opt/backup-tool/backup-tool --config /opt/backup-tool/backup-tool.toml > /dev/null 2>&1
```

cron 不继承交互终端环境，使用环境变量时需另外提供。示例将输出丢弃，避免生成日志文件或 cron 邮件；失败仍会尝试发钉钉通知，若通知也失败则不保留历史记录。排查问题时在终端手动执行命令，不加输出重定向。

恢复先核对 SHA256，再导入隔离的测试实例：

```bash
# 使用 Bash；restore-client.cnf 必须指向隔离的恢复目标。
set -o pipefail
gzip -dc 001-db1.sql.gz | mysql --defaults-file=/secure/restore-client.cnf
```

导入会执行备份中的建库、删表和事件定义。先与源实例保持相同版本，并检查业务数据和恢复耗时。文件校验不能代替实际恢复演练。

## 修改源码

源码按入口、配置、备份、MySQL 调用、同步和通知拆成几个文件，附带测试。需要 Go 1.27.3 或更新版本及 Make；首次构建会通过 go.mod 下载 TOML 解析器、OSS SDK 及其依赖。构建和安装方式参考同目录的 certsync，构建产物不提交到仓库。

```bash
make check                   # go vet + go test -race
make build                   # 本机程序 ./backup-tool
./backup-tool --version
make build-linux-amd64       # dist/backup-tool-linux-amd64
make build-linux-arm64       # dist/backup-tool-linux-arm64
make release VERSION=v0.1.0  # 两种架构的二进制、运行包、许可和 SHA256SUMS，均在 dist/
cd dist && shasum -a 256 -c SHA256SUMS
```

Makefile 固定使用 `CGO_ENABLED=0` 静态编译，使用 `-trimpath` 并去除调试符号；版本默认取 `git describe --tags --always --dirty`，可用 `VERSION` 显式指定。直接 `go build .` 的版本显示为 `dev`。

在目标 Linux 服务器的源码目录中，以 root 执行 `make install` 可构建并安装到 `/opt/backup-tool`。`PREFIX` 可修改安装目录，`DESTDIR` 可指定暂存根目录。安装目录权限为 `700`、程序为 `755`，首次从模板创建的配置为 `600`；再次安装保留已有配置和备份。修改安装目录时，运行命令及 cron 中的路径也要同步修改。`make clean` 删除本机程序和 `dist/` 构建产物。

支持 Linux 和 macOS，暂不支持 Windows。自动化测试使用模拟命令和模拟 HTTP 传输，不连接真实数据库、OSS 或钉钉；上线前请完成一次实际备份、同步和隔离恢复。

## 自动构建和发布

[GitHub Actions](.github/workflows/ci.yml) 在推送 `main`、提交 PR 或手工运行时，执行 Linux/macOS 的 `make check`，构建 Linux amd64/arm64 产物并保存到 Actions 的 `backup-tool-linux` artifact。所有构建使用 `go.mod` 中的 Go 版本。

推送 `v*` tag 后，两种系统的测试和构建全部通过才发布 GitHub Release，附带：

- `backup-tool-linux-amd64`、`backup-tool-linux-arm64`：可直接安装的静态二进制。
- `backup-tool-linux-amd64.tar.gz`、`backup-tool-linux-arm64.tar.gz`：程序、配置模板、说明和许可。
- `THIRD_PARTY_NOTICES.txt`、`SHA256SUMS`：依赖许可和上述文件的 SHA256 校验。

发布时将版本号替换成待发布版本；tag 对应的版本会写入程序的 `--version` 输出：

```bash
make check
git add .
git commit -m "Release v0.1.0"
git tag -a v0.1.0 -m "Release v0.1.0"
git push origin main v0.1.0
```

工作流使用 GitHub 提供的 `GITHUB_TOKEN`，仅发布任务申请 `contents: write`，无需额外配置发布密钥。

## 第三方依赖许可

完整第三方许可随运行包提供，见 [THIRD_PARTY_NOTICES.txt](THIRD_PARTY_NOTICES.txt)。分发单独二进制时，也请附带该文件。

OSS SDK `github.com/aliyun/alibabacloud-oss-go-sdk-v2` 使用 [Apache License 2.0](https://github.com/aliyun/alibabacloud-oss-go-sdk-v2/blob/v1.6.1/LICENSE)，其依赖 `golang.org/x/time` 使用 [BSD 许可](https://cs.opensource.google/go/x/time/+/refs/tags/v0.4.0:LICENSE)。分发程序时应保留相关许可声明。

TOML 解析器 `github.com/BurntSushi/toml` 的许可如下，仅适用于该依赖：

```text
The MIT License (MIT)

Copyright (c) 2013 TOML authors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
```
