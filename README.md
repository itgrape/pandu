# pandu

`pandu` 用于把本地文件和游戏存档备份到百度网盘，也可以从网盘增量恢复。交互式终端、SSH 和定时任务使用同一套命令。

## 构建

需要 Go 1.26.2 或更新版本。项目固定了包含令牌刷新接口的 [百度网盘官方 Go SDK 提交](https://github.com/baidu-netdisk/baidu-drive-sdk-go/tree/472e4a1b111eac1d6307e38802f8fe7adf7e8c21)，命令和补全由 [Cobra](https://cobra.dev/) 提供。

```sh
go build -o pandu .
go test ./...
```

在 macOS 等系统上构建 Linux x86-64 服务器版本。

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/pandu-linux-amd64 .
```

ARM64 服务器使用 `GOARCH=arm64`。其他目标见 [Go 构建环境文档](https://go.dev/doc/install/source#environment)。

## 配置与登录

在 [百度网盘开放平台](https://pan.baidu.com/union/) 创建应用，将应用的 AppKey 和 SecretKey 填入本地 `config.json`。所用应用需要有权访问你的备份目录。下文以 `/apps/你的应用名` 为例。

```json
{
  "app_key": "你的 AppKey",
  "secret_key": "你的 SecretKey"
}
```

默认配置路径为 Go 的用户配置目录下的 `pandu/config.json`。Linux 通常为 `~/.config/pandu/config.json`，macOS 通常为 `~/Library/Application Support/pandu/config.json`。可用 `--config` 或 `PANDU_CONFIG` 指定路径。在 Unix 上将配置文件权限设为 `0600`。

```sh
chmod 600 /绝对路径/config.json
./pandu --config /绝对路径/config.json login
```

登录命令打印授权链接和设备码。在另一台有浏览器的设备上打开链接确认，SSH 中的命令会等待授权结果。授权文件写到配置旁的 `config.json.token.json`。后续命令读取该文件，并在令牌即将过期或服务返回令牌过期时自动刷新。程序还在同目录保存增量记录 `config.json.sync.json`，并用文件锁防止同一配置下的任务重叠。请将这些文件留在备份目录之外。

## 命令

```sh
pandu ls '/apps/你的应用名'
pandu ls -r '/apps/你的应用名/saves'
pandu put ./Saves '/apps/你的应用名/saves'
pandu get '/apps/你的应用名/saves' ./RestoredSaves
pandu put ./Saves/slot1.sav '/apps/你的应用名/saves/slot1.sav'
pandu put --dry-run --delete ./Saves '/apps/你的应用名/saves'
pandu completion zsh
```

- 网盘路径是从 `/` 开始的绝对路径。目录同步将源目录的内容写入指定目标目录，保留子目录和空目录。单文件使用完整的目标文件名。目录路径末尾的斜杠不改变含义。
- 每次成功传输后，程序记录本地内容的 SHA-256 和网盘文件版本。后续运行跳过内容和云端版本均匹配的文件，修改时间相同的内容变更也能识别。首次运行、重新登录或缺少增量记录时，会传输无法确认相同的文件。上传时使用网盘返回的分片列表补传，下载按文件流式写入，完成后替换目标。
- `put` 默认向标准错误输出上传阶段和待传分片进度。定时任务可以加 `--no-progress`，只记录每个文件的结果和最终汇总。
- 源文件覆盖目标同名文件。默认保留目标端多余条目。加 `--delete` 可镜像目录，删除在文件传输成功后执行，目标根目录禁止使用该选项。可先加 `--dry-run` 查看计划。遇到同名文件与目录冲突时命令报错。
- 路径中的空格可用引号包住。程序同步常规文件与目录，遇到源符号链接或特殊文件时报告错误。

## 在服务器上定时备份

先以运行定时任务的用户执行一次 `pandu --config /srv/pandu/config.json login`，再用同一用户编辑自己的 crontab。

```sh
crontab -e
```

将下面这一行加入编辑器并保存。任务每天凌晨 3 点运行，路径按实际安装位置替换。

```cron
0 3 * * * /usr/local/bin/pandu --config /srv/pandu/config.json put /srv/game/saves /apps/my-app/saves >> /srv/pandu/backup.log 2>&1
```

命令失败会返回非零退出码。对于正在写入的游戏存档，使用游戏服务提供的存档导出或文件系统快照作为源目录，可获得一致的备份。
