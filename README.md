# MeBox

<p align="center">
  <img src="web/public/brand/logo-192.png" width="96" height="96" alt="MeBox Logo" />
</p>

<h3 align="center">面向 NAS 与家庭影音场景的私人媒体中心</h3>

<p align="center">
  <strong>媒体库 · 刮削整理 · 网盘 STRM · 兼容 Emby/Jellyfin 客户端 · 远程 Emby 挂载 · 多用户权限 · Docker 一键部署</strong>
</p>

<p align="center">
  <a href="#项目简介">项目简介</a> ·
  <a href="#快速开始">快速开始</a> ·
  <a href="#部署档位">部署档位</a> ·
  <a href="#鸣谢">鸣谢</a> ·
  <a href="#开发构建">开发构建</a> ·
  <a href="README_EN.md">English</a> ·
  <a href="CONTRIBUTING.md">贡献规范</a> ·
  <a href="https://t.me/MeBoxGroup">Telegram 群组</a>
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat-square&logo=go&logoColor=white" />
  <img alt="React" src="https://img.shields.io/badge/React-18-61DAFB?style=flat-square&logo=react&logoColor=111827" />
  <img alt="Docker" src="https://img.shields.io/badge/Docker-ready-2496ED?style=flat-square&logo=docker&logoColor=white" />
  <img alt="License" src="https://img.shields.io/badge/License-GPL--3.0-blue?style=flat-square" />
</p>

---

## 项目简介

**MeBox** 是一个自托管私人媒体管理系统，适合 NAS、小主机、家庭共享和多端播放场景。本项目由 [MediaStationGo](https://github.com/ShukeBta/MediaStationGo) fork 并持续二开维护，在保留「一套服务覆盖网页、手机、电视与第三方播放器」思路的同时，围绕网盘播放、任务队列、远程挂载和权限体系做了大量增强。

你可以把 MeBox 理解为：

- 一个带现代 Web UI 的**媒体库后台**
- 一个兼容 Emby/Jellyfin 客户端的**协议网关**
- 一个连接本地硬盘、下载目录与网盘存储的**整理与播放入口**

### 核心能力

| 模块 | 说明 |
| --- | --- |
| **媒体库** | 电影、电视剧、动漫、综艺、音乐与自定义库；多根目录、扫库、海报墙、继续观看 |
| **元数据刮削** | TMDb、Bangumi、Douban、TheTVDB、Fanart 等；支持 NFO、手动匹配、刮削队列 |
| **播放** | 网页播放器、HLS 转码、弹幕、字幕、播放配置档、观看历史与收藏 |
| **Emby/Jellyfin 客户端兼容** | 内置完整 Emby 服务端协议实现：Infuse、SenPlayer、Fileball、Emby/Jellyfin 官方客户端等可直接把本服务当作 Emby 服务器添加，使用 MeBox 账号登录，海报墙、进度同步、多用户无缝衔接 |
| **远程 Emby 挂载** | 将远程 Emby 媒体库挂载到本地界面统一浏览（无需单独开 Emby 客户端） |
| **网盘与 STRM** | OpenList、CloudDrive2、115、WebDAV 等；STRM 同步、上传/下载队列、直链/302 播放 |
| **下载与整理** | 下载目录定时自动整理（智能分类、自动注册媒体库）、文件管理器（复制/移动/硬链/软链） |
| **用户与权限** | 管理员/普通用户、有效期、成人内容开关、播放配置 PIN、细粒度操作权限 |
| **运维能力** | 统一任务队列、存储统计、DLNA 投屏、系统设置与日志 |

### 技术栈

- **后端**：Go · Gin · GORM · SQLite / PostgreSQL · 可选 Redis · 可选 OpenSearch
- **前端**：React 18 · Vite · TypeScript · Tailwind CSS · Zustand
- **部署**：Docker Compose 多档模板，支持 amd64 / arm64 镜像与单文件可执行发布

### 本地 fork：AMS 中文字幕补丁

本 fork 已迁入 AMS `/root/zhsubs/mebox-src` 的实际中文字幕业务补丁，后续发布应构建本 fork，而不是假定上游 `latest` 镜像包含这些变更。本次源码同步不代表已更新在线容器。

- 新建用户仅在 `SubtitleChineseMode` 为空时默认使用 `simplified`；显式偏好和旧用户记录保持不变，不做数据库回填。
- 外挂字幕按「简体 → 泛中文 → 繁体 → 其他」稳定排序，同类保持发现顺序。简体/泛中文标签为 `简体中文`，繁体为 `繁體中文`，英语为 `English`；Emby 语言代码归一为中文 `chi`、英语 `eng`。
- Emby 播放载荷默认选择第一条最优简体/泛中文字幕；只有繁体或其他语言时不设置默认字幕。`DefaultSubtitleStreamIndex` 对应实际 `MediaStreams.Index` 和字幕下载索引（无音轨从 1 开始，有音轨从 2 开始）。

| 环境变量 | 默认值 | 覆盖行为 |
| --- | --- | --- |
| `MEBOX_EMBY_SUBTITLE_LANGUAGE` | `chi` | 覆盖 Emby 用户配置的字幕语言；`-` 表示空语言偏好 |
| `MEBOX_EMBY_SUBTITLE_MODE` | `Always` | 覆盖 Emby 用户配置的字幕模式，例如 `Default`、`None` |

环境变量会去除首尾空白，未设置或仅空白时使用默认值；只影响 Emby 用户配置，不修改网页用户偏好或外挂字幕排序。

外挂格式仍仅支持 `.srt`、`.ass`、`.ssa`、`.vtt`，不新增 `.sub`、`.idx`、`.sup` 等格式。文件名语言识别保持云端原逻辑：例如 `Movie.zh-CN.srt`、`Movie.zh_Hant.ass` 可识别；`Movie.zh-CN.default.srt`、`Movie.zh-Hans.forced.ass` 的尾缀会被识别为 `default`/`forced`，并非中文，本轮不扩展此规则。

### 请求与播放性能追踪

设置 `MEBOX_PERFORMANCE_TRACE=true` 后启动核心，即为每个请求（含静态文件和健康检查）生成服务端 `X-Request-ID`。
完整追踪使用现有独立 INFO 兼容日志 sink，默认写入 `/data/logs/emby-compat.log`，`msg=performance`；
不受普通应用 WARN 级别过滤，沿用现有日志轮转。默认关闭；关闭时没有 trace 对象、额外计时或流包装。
新追踪字段不记录 URL/query、请求体、认证头、SQL 参数、媒体路径或上游直链。日志仍应作为私有运维数据保管。

- `db.sql`：SQL 次数及累计／最大时长，包含 GORM callback、pool 获取与结果扫描；另记录 SQLite 写闸、BEGIN、busy retry 和 backoff。
- `playback.*`、`subtitle.*`、`hls.*`、`ffprobe.*`：播放准备、版本查询／过滤、缓存 hit/miss、目录扫描、文件操作、转码启动／ready／旧 job 退出、探测排队及执行。
- `stream.read/seek` 与 `http.write/flush`：区分本地读盘等待与向客户端写入的回压；上游单独记录 `upstream.read`。
- `upstream.*`：标准 `httptrace` 的连接获取、连接复用、DNS、TCP、TLS、首个响应字节；115 另记录请求队列和重试。

`trace.metrics` 的 `total_ns/max_ns` 和慢事件的 `offset_ns/duration_ns` 均为纳秒。时长为 inclusive，可嵌套、并发重叠，
不能相加冒充请求总时长。每请求最多 64 类指标、32 个 ≥100ms 慢事件；截断有明确计数，不逐 chunk 写日志。
`http.response_ready` 是应用准备提交响应的时间，**不是客户端 TTFB**；边缘缓存命中还可能复用旧响应里的请求 ID。
必须把真实客户端的 TTFB、持续 Range 读取、Nginx／Tunnel 和主机观测一起比对，不能仅看服务端总耗时判断卡顿。

#### 按用户名记录详细业务请求

设置 `MEBOX_USER_REQUEST_LOG=true` 后，所有完成的 HTTP 请求（含失败、匿名、静态资源和健康检查）单独写入
`<app.data_dir>/logs/user-requests/requests.jsonl`；AMS 容器为 `/data/logs/user-requests/requests.jsonl`。
默认关闭，与 `MEBOX_PERFORMANCE_TRACE` 独立；不会写到 stdout 或普通应用日志，普通日志级别不影响采集。
`Dockerfile.ams` 用 `USER_REQUEST_LOG=true` 显式启用。目录 `0700`、活动与轮转文件 `0600`，拒绝目录／文件软链接。

- `user_id` 和 `username` 来自已认证账号及数据库用户记录，不信任客户端提交的用户名或 `UserId`。API／Emby
  复用已有账号查询；未查询用户的管理路由仅补一次只读查找，不增加用户名缓存或修改 JWT。成功登录／注册也关联真实账号。
  `username_state=known/anonymous/unavailable` 区分已解析、匿名、数据库不可用／用户已不存在；不可用时保留真实 ID，不猜名字。
- 保存原始／归一化路径、重复 query 值、脱敏请求头、客户端／设备／会话／profile、业务正文、状态、响应头／字节数、耗时与取消。
  `request_id` 对应服务端 `X-Request-ID`，开启性能追踪时与 trace 同 ID；Emby 大小写重分发不重复记录。
  `requested_media_source_id` 是请求选项；trace 的 `playback.*.selected` 计数是协商首选建议，不等于客户端实际使用；实际来源须结合拉流路径与响应内容类型核对。
- JSON 保留业务字段和标量类型、大整数精度，例如 `MinSegments="1"`、codec／声道／播放参数；form 同样按字段脱敏。
  密码／PIN／token／Cookie／Authorization／API key／签名密钥、签名 URL 凭据、敏感 `key/value` 设置及嵌套 JSON 字符串
  统一脱敏。认证头中的客户端信息另行解析保存；日志不是可直接复用认证凭据的原始网络抓包。
- 仅被处理器读取的 JSON／form 正文采集，最多 1 MiB，不预读／额外 drain、不改变 handler 收到的字节／读错误。`text/plain` 中完整可解析的 JSON 对象／数组也采集并同样脱敏；普通纯文本、JSON 标量或畸形正文不落原文。
  `body_state` 明确标记 `complete`、`empty`、`unread`、`incomplete`、`truncated`、`invalid_json` 或 `unsupported`；读错误另记 `body_read_error`。
  未读完、超限、畸形、压缩／二进制／multipart 正文不写原始片段，避免截断时漏出凭据。影片、字幕、图片等响应正文从不采集。
  流式请求在结束／取消时写完成记录，耗时不是客户端首帧或解码性能。
- 固定每文件 100 MiB、10 份备份（本目录约 1.1 GiB），轮转时清理超过 14 天的备份；不是所有记录严格 14 天 TTL。
  复用现有同步轮转器，没有队列静默丢弃；运行时磁盘写错误进入 stderr，不能保证磁盘故障时仍有完整日志。

日志包含用户名、观看请求和设备信息，仅供私有运维，不能进入 Git、公开报告或普通日志导出；需要长周期统计时应先生成聚合结果。

AMS 核心与网页更新使用 `Dockerfile.ams`：先构建本 fork 的 `web/dist`，再以 `CGO_ENABLED=0` 构建静态 `mebox`，
在私有临时 build context 放入二进制、Dockerfile 与 `web/dist`（保持此目录结构）。显式提供 `AMS_BASE_IMAGE`
为已核验的本地 AMS 基线镜像、`REVISION` 为构建来源 SHA，观测版额外提供 `PERFORMANCE_TRACE=true`。
Dockerfile 以 `COPY --chmod=755` 安装 `/usr/local/bin/mebox`，并将新网页复制到 `/app/web/dist`；保留基线的
FFmpeg、entrypoint 和系统包，**不保留旧网页**。发布等待目标 image 与 healthy 同时满足，不能仅看旧容器健康。
最终按实际镜像 ID 发布，不移动 `latest` 或覆盖原镜像。现有数据库、JWT 密钥和六个 bind 保持，升级前另做 WAL 一致性备份。

#### AMS 实测与原画无损封装

2026-10-09 的请求回放使用 AMS 的真实本地 AV1 文件；117 个客户端请求均与服务端 trace 关联。
同样的 2 MiB Range 在 AMS 本机最慢首字节为 23.8 ms，客户端公网最慢为 2541.8 ms；
一次客户端总耗时 6.02 s 的传输，源站处理只有 8.7 ms。不能把跨境传输等待归因于 SQLite 或读盘。
公网 HTTP/2、HTTP/3 与 SSH 绕行均只有约 8–11 Mbps 的单连接有效吞吐；协议切换不是已验证的根治方案。

原始 MKV 的浏览器拖动还出现音频缓冲落后于视频。禁用缓存、同一 SSH 对照链路、同一影片的两轮实验中，
原 MKV 拖动到 4800 秒等待 12.98/13.16 s，保留 AV1/FLAC 压缩包的 faststart MP4 为 2.74/2.67 s。
重新封装为 MKV 没有得到同样改善。**这些是对照链路结果，不是公网延迟承诺**：正式公网 MP4 的三次拖动仍为
4.14/3.66/5.05 s，其中首个视频数据到达占 3.21/3.25/4.27 s。封装修复不等于消除链路等待。

进一步缩小读取窗口也不能仅看一次拖动数字。每轮重建 video 元素、使用新 URL 并关闭 HTTP 缓存的公网对照中，
2 MiB 有界 Range 的两次拖动为 4.54/1.75 s，但冷加载增至 17.17/14.60 s，且出现后续缓冲不足；未上线这个实验。
同一个 video 元素重复拖到已缓冲位置的几十毫秒结果不属于冷测，不能作为优化收益。生产仍使用标准 Range 行为。

获明确授权的单片试点保留原 MKV、原媒体 ID 和历史，增加可选的 `.browser.mp4` 版本；
138625 个视频包、67756 个音频包的顺序 SHA-256 摘要均与原片一致，没有重编码或降低画质。
原片含 5 条 ASS 字幕及字体附件，MP4 不应吞掉它们：字幕另存为同前缀 ASS 外挂，原容器和附件继续保留。
不要直接使用 `-map 0:v -map 0:a` 覆盖原片，也不要在未验证音轨、字幕与客户端兼容性前批量接入。
该试点由现有扫描器发现并接入版本分组，未重置审核、账户、媒体任务或原片记录。

Emby 播放源会将 ffprobe 的 `matroska,webm`、`mov,mp4,m4a,3gp,3g2,mj2` 别名集合规范成单个客户端容器名，
保留 WebM/MOV 等实际扩展名；不把逗号列表当作容器或 `/Videos/.../stream` 扩展名，也不回填数据库。

#### 软件原画 fMP4 HLS VOD（2026-10-10 补充）

实际选中的配置是 **2 秒目标分片**（边界受原视频关键帧约束），不是现场视频转码。离线包复用原 media ID 的
`quality=prepared` HLS 播放端点及现有 auth/profile 校验；playlist、init 和分片均受保护，不向外部 URI 转发凭据，
也不公开 `source.json`。服务仅读取已准备资产，不启动现场 FFmpeg，因此 `direct_only` 也允许使用。
清单为 init／分片绑定 `prepared_v` 包版本；资产请求必须匹配当前包，防止旧清单混入新包，也避免回滚后条件缓存复用错误字节。
浏览器必须通过现有 hls.js 的 MSE 与完整 codec capability 判定；forced-direct、VR 或不支持的客户端不强制切换。
fatal 错误回退原文件并保留位置，明确提示不会自动视频转码。逻辑 media ID、数据库身份与 history 不变；使用外挂／文本字幕，不烧录。
Emby 协议播放入口也读取 PlaybackInfo 的 `DeviceProfile`，或复用同用户、同设备经 `/Sessions/Capabilities/Full` 上报的短期能力；GET／POST、根路由及 `/emby` 大小写兼容入口共用此协商，不按客户端名称分支。
明确支持相应 codec／fMP4 HLS 的客户端优先协商 2 秒 prepared VOD，使用标准 `TranscodingUrl`／HLS 字段，但不会启动现场转码；否则按已声明的原生容器能力优先选择前置 cues 的 copy-only MKV，再选择 faststart MP4。未限制容器的 `DirectPlayProfile(Type=Video)` 保持既有通配语义；没有能力声明不等于通配支持。音频转换仅存在于 HLS，并在来源名称明确标记。
未知能力保留原文件默认，明确 source／其他音轨选择不暗换，声道数、码率、codec/profile/bit depth 与声明的必要条件不满足则不自动选优化来源。显式选择和自动默认的字幕都需要客户端支持现有外挂交付，不自动烧录；明确关闭字幕不阻止 prepared。
自动协商命中 prepared 时只返回这一个可用表示，默认 `MediaSources[].Id` 保留从条目选择的原逻辑来源 ID，真实表示由带明确资源 selector 的 URL 指定；不再把另一个可直接播放的原版候选放进同一自动响应让客户端重新选回。没有可用能力则仅返回原来源，未通过条件的 prepared 不作为自动候选。明确 `MediaSourceId=<原 ID>`／其他音轨仍请求原文件；明确 `:<mp4|mkv|hls>` 请求对应资源，原生 selector 也须通过能力和字幕条件，不能借此绕过限制。版本和历史身份不重建。
协商按需构建来源：兼容 HLS 命中后不再构建未使用的 MP4，prepared 命中后不再枚举／构建原版本；只有回退或明确原版选择才查询原版本。没有跨请求缓存，不跳过源 fingerprint、能力／字幕校验或文件权限检查；这降低的是服务器协商开销，不是已证明的设备首帧提速。
`TranscodingProfile.MaxAudioChannels` 遵循 Emby schema 的字符串契约；非空上限必须为有效正整数且匹配实际音轨，否则不选 prepared。离线播放不消费 `MinSegments`，因此不为它建立强类型绑定：Hills 的 `"MinSegments":"1"` 不应阻断 PlaybackInfo，实际用于权限／选源的字段校验仍保留。
prepared MKV／MP4／HLS 的 `Protocol=Http`、`IsRemote=true` 与 `Path` 指向同一个带鉴权的真实播放端点；不会把原文件路径放进预制来源。遵循 HTTP `Path` 的客户端可直接消费已协商资源，原版来源身份／路径和明确音轨选择不变。容器名称、扩展名和 MIME 与实际字节一致，也不在缺少设备身份的原版拉流请求上猜测账号内其他设备的能力。Web 前端仍使用原有 HLS／原版路径，不强推 MKV。
这些是协议协商保证，不代表所有硬件／解码器或 Hills 设备已经逐一实测改善。

可选 `MEBOX_PREPARED_MEDIA_BASE_URL=https://37.48.70.166`（配置键 `prepared_media_base_url`）只为 Emby PlaybackInfo 已按上述规则选中的原音轨 prepared MKV／MP4 切换媒体入口；缺省为空，继续使用现有入口。必须是可信 HTTPS origin，不能带用户凭据、query、fragment 或业务 path（仅允许空 path 或 `/`），无效配置使服务启动明确失败；不能以全局 `app.server_url` 代替它。旧 MP4 专属配置不是兼容别名，升级须与独立入口配对切换。
选中来源的 `Path` 和 `DirectStreamUrl` 都指向 `/prepared-media/<原 ID>/stream.<mkv|mp4>`，保留资源 selector、访问 token 和播放 profile／PIN query；第三方绝对 URL 不附加本服务 JWT。该入口只读代理既有受保护的 `/Videos/<原 ID>/stream.<mkv|mp4>`，不缓存鉴权或媒体，不提供登录／API／写入操作。原始文件、明确其他音轨、未知或不支持的能力、HLS 和字幕继续使用原入口；协商及每次原生拉流均检查当前 profile／PIN／库权限，撤销立即生效，包代际和 fidelity 门槛不变。
公网直连改善已有同包同恢复点的桌面软件输出证据；它不是 Android 可见首帧承诺，也不代表手机或所有解码器已经验收。

“原画”只保证视频不重编码：原视频经独立 copy-only MP4 规范化后，与 prepared 的逐包 SHA-256、数量、顺序一致；
另校验 PTS/DTS、源 fingerprint、codec/colors、各分片独立关键帧、完整时间轴与媒体包位置（不能落入 init）。
原始影片、所有原音轨、字幕和附件仍保留在原文件中。兼容包只选择首条视频／首条音轨：AAC-LC 原包复制，
FLAC、AC3、EAC3 或非 LC AAC 在离线准备时转换为 **AAC-LC 兼容音频**，不改变声道数或采样率；元数据和网页明确标出音频转换。
AC3 `5.1(side)` 转换时显式逐路映射为规范 AAC `5.1`，保留六路信号，但侧／后环绕 speaker 标签不同，不能称空间布局完全相同。
这不是全部音频无损，也不是多音轨 HLS 切换；需要原音轨时仍选原文件。仅接受工具支持的 AV1/H.264/HEVC；
复制 AAC-LC 必须具有标准 ASC 显式声道配置，并通过真实 ASC 哈希和全包校验；不猜测布局。AAC-PCE（声道配置 0）包虽可通过完整性复核，真实网页 MSE 仍出现失败，因此准备工具拒绝发布该配置，保留原文件直连，不推断声道后重混音。
分片使用原生 `skip_sidx` 避免 DASH sidx 改写 AAC 边界时间；时间轴超过严格门槛的源拒绝 prepared，继续原文件直连，不添加自定义 MP4 修复器。
没有完成全客户端、全部电影或字幕字体视觉验收。

离线 CLI 位于本仓库 `scripts/prepare_playback.py`，须在 **host 上使用 Python 3、FFmpeg、ffprobe**；AMS 容器没有 Python。
从本仓库根目录执行下面命令。`SOURCE_HOST` 必须由实际 Docker bind 映射反查到 host 的真实常规文件，不能照抄容器内路径、
使用 STRM／远程 URL 或 symlink；`CACHE_HOST` 是实际 cache bind 的 host 目录，`MEDIA_ID` 是已有原媒体 ID，不是新建记录。
确保输出与源隔离且容器运行用户可读；不要覆盖原片或公开缓存目录。

```bash
umask 077
read -r -p 'Host source file: ' SOURCE_HOST
read -r -p 'Host cache directory: ' CACHE_HOST
read -r -p 'Existing media ID: ' MEDIA_ID
mkdir -p "$CACHE_HOST/prepared-hls"
python3 scripts/prepare_playback.py --source "$SOURCE_HOST" \
  --output "$CACHE_HOST/prepared-hls/$MEDIA_ID" --segment-seconds 2
```

原音轨快速 MP4 使用同一 CLI，保持 `--format` 默认 HLS 不变：

```bash
mkdir -p "$CACHE_HOST/prepared-mp4"
rtk proxy python3 scripts/prepare_playback.py --source "$SOURCE_HOST" \
  --output "$CACHE_HOST/prepared-mp4/$MEDIA_ID" --format mp4
```

该版本仅复制首条视频／首条音频（AAC／FLAC／AC3／EAC3），不重编码、不混音、不猜测 PCE 声道布局；其他音轨、章节、字幕与附件保留在原文件。优化 MP4 不复制章节，避免 muxer 自动生成未请求的数据轨。
Native MP4 使用原生 `-copyts` 保留原始时间轴／负音频 preroll，不使用会截短部分源末视频 sample 的 `-start_at_zero`；HLS 既有归零与校验逻辑不变。
半秒 chunk 聚合曾将实际影片前置索引缩小约 56%，但 Hills 真机恢复播放产生额外反向读取，公网交叉测量没有稳定首帧收益，现已撤回该参数并恢复旧包。保留默认紧密音视频交错、普通 faststart MP4、完整 payload／时间轴门槛与原文件入口；不以索引尺寸代替设备首帧验收。
验证 moov 在 mdat 前、非碎片化、逐包 payload／数量／顺序、配置、PTS／DTS／结束同步及源 fingerprint；任一严格门槛不满足就拒绝发布，原文件继续可选。复制相同压缩包不保证不同 demuxer 的首尾 trimming 完全相同。
共享原生服务只读取对应 `prepared-<mp4|mkv>/<原 ID>/source.json` 与 `stream.<mp4|mkv>`，保留既有媒体可见性与播放 profile 权限，支持标准 Range／HEAD／条件请求；私有 no-cache 与包代际 ETag 防止回滚误用旧缓存。不调用 runtime FFmpeg／ffprobe／mkvinfo。
Emby 兼容入口要求访问 token，仍拒绝用途限定的外链 token；后者只能访问原有 `/api` 播放端点且绑定单片，不能借 prepared 获取账号 API 权限。

前置 cues 的 MKV 使用同一脚本，另需已安装的原生 `mkvinfo`（可用 `--mkvinfo` 指定路径）；必须从原片生成，不能把 MP4 的恢复解码行为作为原片替代基准：

```bash
rtk proxy mkdir -p "$CACHE_HOST/prepared-mkv"
rtk proxy python3 scripts/prepare_playback.py --source "$SOURCE_HOST" \
  --output "$CACHE_HOST/prepared-mkv/$MEDIA_ID" --format mkv \
  --resume-seconds 1855
```

`--resume-seconds` 是该片已知恢复点的附加验收参数，不改变媒体时间轴或播放器；只适用于 MKV，未知恢复点可省略。MKV 同样只复制首条视频／音频，保留原文件所有额外资源；除完整逐包、配置、时间轴、源 fingerprint 与 cues-before-first-Cluster 门槛外，还比对原片与输出的全片默认解码 PCM、片中／片尾及指定恢复点 PCM。任何不一致都拒绝发布；不使用 `skip_manual`、噪声生成选项或样本修复来让检查通过。恢复点检查不是对全部可能 seek 或全部解码器的穷举保证。


已有目录会完整复核，返回 `status=unchanged` 而不重打；改变 `--segment-seconds` 也不会重建已有包。
需要改参数时先在独立目录生成并验证，停止该片读取后再按授权维护流程替换；不要直接覆盖已发布缓存。
该流程不操作数据库，不触发扫描建新身份。私有证据放在忽略的 `.transcode-state/ams-performance/`，目录 `0700`、文件 `0600`；
公开报告不得含 token、带 query 的播放 URL、账户或完整媒体路径。

真实公网冷媒体对照中，2 秒 VOD 的两轮 3600／4800 秒 seek 恢复约 1.50–1.70 秒，而启动仍为 1.568／4.944 秒。
完整 UI 的另一次 2 秒配置启动 decoded 为 3.350 秒，原 ID 自动续播至 5289.666 秒；这不是随机页面 AB，不能承诺亚秒级。
没有部署缺乏独立收益的 progressive 参数；微片配置曾真实解析失败并已拒绝。测量方法、失败与发布修复见
[AMS 播放性能研究](../../docs/ams-playback-performance-study.md#17-2026-10-10-补充软件原画-vod-与真实网页验证)。

---

## 快速开始

推荐使用 Docker Compose。仓库提供四份**互相独立**的完整模板，无需 `.env` 即可起步。

```bash
mkdir -p MeBox && cd MeBox

# 最省心：单镜像 + 内置 SQLite
curl -fsSL https://raw.githubusercontent.com/truewhile/MeBox/main/docker-compose.simple.yml -o docker-compose.yml

# 或多用户场景：PostgreSQL 第一档
# curl -fsSL https://raw.githubusercontent.com/truewhile/MeBox/main/docker-compose.yml -o docker-compose.yml

docker compose up -d
```

浏览器访问：

```text
http://服务器IP:18080
```

默认账号：`admin` / `admin123`（首次登录后请立即修改密码）

> 💡 **Emby 用户无缝切换**：MeBox 完整兼容 Emby/Jellyfin 客户端协议。手机、电视、平板上的 Infuse、SenPlayer、Fileball、Emby/Jellyfin 官方客户端，直接按「添加 Emby 服务器」填入 `http://服务器IP:18080`，用 MeBox 账号登录即可，无需改变原有使用习惯。

镜像地址：

```text
ghcr.io/truewhile/mebox:latest
```

---

## 部署档位

按机器资源选择档位。每份 Compose 文件均可单独使用，**不要**叠加多个 `-f`。

| 档位 | 配置文件 | 组件 | 适合场景 |
| --- | --- | --- | --- |
| 单镜像档 | `docker-compose.simple.yml` | MeBox + SQLite | 新手、单人、低配 NAS，只想一个容器跑起来 |
| 第一档 | `docker-compose.yml` | MeBox + PostgreSQL | 大多数家庭 NAS，多用户更稳 |
| 第二档 | `docker-compose.standard.yml` | + Redis | 多用户、Emby 客户端频繁刷新、首页/列表访问多 |
| 第三档 | `docker-compose.search.yml` | + OpenSearch | 超大媒体库、复杂全文搜索（内存占用更高） |

### 单镜像档要点

- 只启动 **一个** MeBox 容器，数据在 `./data/mebox.db`
- 通常只需改端口与媒体目录挂载
- **不要**设置 `MEBOX_DATABASE_DSN`，否则会切到 PostgreSQL

```yaml
ports:
  - "18080:8080"
volumes:
  - ./data:/data          # 必须备份
  - ./cache:/cache        # 可重建
  - ./media:/media        # 改成你的媒体目录
```

网页添加媒体库时填写容器内路径，例如 `/media`、`/media/电影`。

### PostgreSQL 档位要点

- 主库在 `./postgres`，配置与密钥在 `./data`
- 若存在旧版 `./data/mebox.db`，首次启动会自动迁移到 PostgreSQL
- 迁移完成后可将 `MEBOX_DATABASE_DB_PATH` 改为不存在路径，避免重复检查：

```yaml
MEBOX_DATABASE_DB_PATH: /data/no-sqlite-migration.db
```

### 必须备份与可重建

| 路径 | 说明 |
| --- | --- |
| `./data` | JWT 密钥、运行配置、SQLite 主库或迁移源 |
| `./postgres` | PostgreSQL 主库（PG 档位） |
| `./cache` | 海报/转码缓存，可重建 |
| `./redis` | 热缓存，可重建 |
| `./opensearch` | 搜索索引，可重建 |

### 更新镜像

```bash
docker compose pull mebox
docker compose up -d --no-deps mebox
```

日常更新只拉 `mebox` 服务即可，不要随意 `docker compose pull` 升级 PostgreSQL/Redis/OpenSearch 基础镜像。

---

## 路径映射

Docker 部署最常见的问题是路径填错。记住：

- `volumes` **左侧**是宿主机真实路径，**右侧**是容器内路径
- 网页后台添加媒体库时，应填写**容器内**路径（如 `/media/电影`）
- 若使用自动整理/下载入库，`MEBOX_MEDIA_DIR` 与 `MEBOX_DOWNLOAD_DIR` 需与挂载一致

NAS 示例：

```yaml
volumes:
  - /vol1/1000/Media:/media
  - /vol1/1000/Downloads:/downloads
environment:
  MEBOX_MEDIA_DIR: /vol1/1000/Media
  MEBOX_MEDIA_CONTAINER_DIR: /media
  MEBOX_DOWNLOAD_DIR: /vol1/1000/Downloads
  MEBOX_DOWNLOAD_CONTAINER_DIR: /downloads
```

---

## 首次使用建议

1. **创建媒体库** → 填写 `/media/...` → 执行扫库
2. **配置元数据源** → 系统设置中添加 TMDb、Bangumi 等 API
3. **（可选）配置下载目录自动整理** → 文件管理中将下载目录设为整理源，下载完成后自动分类入库
4. **（可选）配置网盘账号** → STRM 管理中添加 OpenList / 115 / WebDAV 等
5. **第三方播放器** → 以 Emby 服务器添加 `http://服务器IP:18080`，使用 MeBox 账号登录

---

## 常见问题

**扫库或入库很慢？**  
先确认路径映射与数据库档位。网盘扫描还受接口限速与目录规模影响；大库可考虑第二档 Redis 或第三档 OpenSearch。

**下载目录文件没有被自动整理？**  
确认下载目录已通过 `volumes` 挂进容器，且 `MEBOX_DOWNLOAD_*` 环境变量对应正确。MeBox 负责目录整理入库，qBittorrent 等下载器按普通软件自行部署即可。

**硬链接失败（cross-device link）？**  
硬链接要求源与目标在同一文件系统/子卷；跨盘、跨 btrfs 子卷或网盘挂载时请改用复制或软链接。

**日志保留时间太短？**

默认应用日志为 `20MB x 5`，容器 stdout 日志为 `20m x 3`。排障时可在 compose 中调大
`MEBOX_LOGGING_MAX_SIZE_MB`、`MEBOX_LOGGING_MAX_BACKUPS` 与服务的 `logging.options.max-size/max-file`。

**第三方播放器连不上？**  
确认地址为 `http://IP:18080`，使用 MeBox 用户账号；反代部署需正确配置外部 URL 与 HTTPS 头。

---

## 开发构建

后端通过 `go:embed` 嵌入 `web/dist`，**编译前必须先构建前端**。
前端构建要求 Node.js `20.19+` 或 `22.12+`。

```bash
npm --prefix web ci
npm --prefix web run build

go test ./...
go run ./cmd/server          # http://127.0.0.1:8080
npm --prefix web run dev     # http://127.0.0.1:3000
```

AMS 的 Linux amd64 静态核心在上述前端构建之后生成（其他目标须匹配实际 host 架构）：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o mebox ./cmd/server
```

将此 `mebox`、`Dockerfile.ams` 和 `web/dist` 放入私有 build context，按前文指定的 build args 构建候选镜像；
只发布已核验的目标镜像，不用单独更新 binary 的方式留下旧网页。

CI 会在 Release 中提供 Windows / Linux / macOS 的 amd64、arm64 单文件可执行程序。

Windows 本地打包：

```powershell
.\scripts\build-windows.ps1 -Version dev
```

Windows 可执行程序使用项目 Logo，不显示控制台窗口；启动后会常驻系统托盘。托盘菜单可打开 MeBox、切换开机自启、查看日志、重启或退出。

---

## 鸣谢

MeBox 在 [MediaStationGo](https://github.com/ShukeBta/MediaStationGo) 的基础上 fork 并持续演进。感谢上游项目在媒体库架构、Emby 协议兼容和自托管体验上的奠基工作。

项目中许多网盘同步、STRM 与媒体整理相关的设计与实现，也参考了 [qmediasync](https://github.com/qicfan/qmediasync)。感谢该项目的思路与实践经验。

---

## 贡献与反馈

提交 Issue 或 Pull Request 前，请阅读 [贡献规范](CONTRIBUTING.md) 与 [安全策略](SECURITY.md)。

- Bug 请附部署方式、复现步骤与相关日志
- 功能建议请说明使用场景与期望行为
- PR 请从独立分支发起，提交前运行 `go test ./...` 与 `npm --prefix web run build`

---

## Star History

<a href="https://www.star-history.com/?repos=truewhile%2FMeBox&type=date&legend=top-left">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/chart?repos=truewhile/MeBox&type=date&theme=dark&legend=top-left" />
   <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/chart?repos=truewhile/MeBox&type=date&legend=top-left" />
   <img alt="Star History Chart" src="https://api.star-history.com/chart?repos=truewhile/MeBox&type=date&legend=top-left" />
 </picture>
</a>

---

## 许可证

本项目采用 [GPL-3.0](LICENSE) 许可证。

---

## 赞赏

如果 MeBox 帮你把家庭影音折腾明白了，欢迎请作者喝杯咖啡 ☕

<p align="center">
  <img src="docs/images/donation-qr.png" width="320" alt="WhileTrue 的赞赏码" />
</p>

<p align="center">
  <strong>Telegram 交流群</strong>：<a href="https://t.me/MeBoxGroup">https://t.me/MeBoxGroup</a><br/>
  使用问题、功能建议、更新动态，欢迎来群里聊
</p>
