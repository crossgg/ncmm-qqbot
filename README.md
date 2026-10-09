# NCMM QQ 机器人伴生插件 (ncmm-qqbot)

<p align="center">
  <b>专为 <a href="https://github.com/3899/ncmm">NCMM (网易云音乐任务助手)</a> 量身打造的 QQ 机器人伴生插件</b><br>
  基于腾讯官方 <a href="https://bot.q.qq.com/wiki/develop/api-v2/">QQ 机器人开放平台 API v2</a> 开发，支持 QQ 私聊自动识别更新 Cookie、扫码登录、任务通知推送与远程控制。
</p>

<p align="center">
  <a href="https://github.com/crossgg/ncmm-qqbot/releases"><img src="https://img.shields.io/github/v/release/crossgg/ncmm-qqbot?color=10b981&label=Release" alt="Release"></a>
  <a href="https://github.com/crossgg/ncmm-qqbot/actions"><img src="https://img.shields.io/github/actions/workflow/status/crossgg/ncmm-qqbot/release.yml?label=CI%2FCD" alt="Build Status"></a>
  <img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="License">
</p>

---

## ✨ 核心特性

* 🍪 **智能识别并更新 Cookie（双重方式）**：
  * **文件发送（强烈推荐）**：直接向机器人发送包含 Cookie 的【`.txt`】或【`.json`】文本文件（如电脑端直接拖入、手机端选文件发送），秒级解析并自动绑定；**彻底避开 QQ 平台针对超长密文的文本内容安全拦截**！
  * **文本指令**：在 QQ 私聊中直接发送 Cookie 文本（或发送 `/cookie <内容>`）；
  * 插件自动调用网易云底层接口获取对应 UID 与昵称，**智能比对系统中的主账号与各乐迷团辅助小号并精准覆盖写入**，无需手动指定或查找账号！
* 📱 **QQ 私聊一键授权登录**：
  * 在 QQ 私聊发送 `/login` 或 `/qrcode`，插件毫秒级生成官方授权登录链接；
  * 用户在手机 QQ 直接点击链接唤起网易云音乐 App 确认授权，插件自动捕获成功状态并持久化 Cookie。
* 🔔 **Notify 消息中继推送**：
  * 内置本地监听接口（`http://127.0.0.1:5606/notify`）；
  * `ncmm` 宿主在每日打卡任务完成或发生异常报警时，自动将排版精美的卡片推送到管理员 QQ 私聊。
* ⚙️ **全可视化在线配置（零手动编辑文件）**：
  * **宿主 WebUI 弹窗集成**：直接在 `ncmm web` 插件中心点击 **【⚙️ 在线配置】** 即可修改 AppID、AppSecret、管理员 OpenID 等，保存后自动重载生效；
  * **独立 Web 控制台**：浏览器直接访问 `http://127.0.0.1:5606` 也可进行参数配置、凭据连通性测试与推送历史查阅。
* 🔄 **进程生命周期与网络架构深度适配**：
  * **随宿主启停**：属于 `daemon`（常驻后台伴生）类型插件，启用后随 `ncmm web` 一键拉起或停止，退出宿主即自动退出，**不写开机自启或外部系统服务**；
  * **安全内网互通，零端口映射**：与宿主通信走回环 `127.0.0.1:5606`，与腾讯通信走主动出站 WebSocket 连接。无论在 Windows 还是在 Docker 容器中运行，**均无需对外映射开放端口**。
* 💬 **丰富便捷的 QQ 远程运维指令**：
  * 支持 `/status`（查看账号状态）、`/run`（立即触发打卡任务）、`/myid`（查询当前 OpenID）、`/bind`（一键绑定管理员）等。

---

## 📦 安装方式

### 方式一：在 `ncmm web` 控制台在线安装（推荐）

1. 打开 `ncmm web` 管理面板，进入 **插件中心**；
2. 点击右上角 **【在线安装插件】**；
3. 在仓库地址一栏直接输入：
   ```text
   crossgg/ncmm-qqbot
   ```
4. 点击 **开始安装**，系统会自动拉取与您系统架构（Windows / Linux x86_64 / aarch64 等）匹配的最新 Release 并完成解压。

### 方式二：命令行在线安装

如果您使用的是纯命令行环境，在宿主运行目录下执行：

```bash
ncmm plugin install crossgg/ncmm-qqbot
```

### 方式三：Release 安装包离线/上传安装

1. 前往 [Releases 页面](https://github.com/crossgg/ncmm-qqbot/releases) 下载适合您系统架构的压缩包；
2. 在 `ncmm web` 插件中心点击 **【上传插件包】**，或直接解压至 `plugins/ncmm-qqbot` 目录即可。

---

## 🚀 配置与使用指南

### 第一步：在腾讯 QQ 开放平台创建机器人

1. 登录 [腾讯 QQ 开放平台](https://bot.q.qq.com/)；
2. 进入管理端创建机器人应用（类型选择 **QQ机器人**）；
3. 在机器人管理面板的 **开发 ➔ 开发设置** 中获取：
   * **AppID**（机器人唯一标识）
   * **AppSecret (ClientSecret)**（机器人通信密钥）
4. 在平台 **沙箱配置** 中将自己的 QQ 号添加为测试成员（测试阶段无需发布上线即可直接私聊）。

### 第二步：在线填写插件配置

1. 打开 `ncmm web` 管理面板 ➔ **插件中心**；
2. 找到 **QQ机器人助手** 卡片，点击 **【⚙️ 在线配置】**；
3. 填入上一步获取的 `AppID` 和 `AppSecret`；
4. 点击 **【保存配置】** 并点击 **【启动服务】**。

### 第三步：获取 OpenID 并绑定管理员

1. 打开手机 QQ，向您创建的机器人发送私聊消息：
   ```text
   /myid
   ```
2. 机器人将回复您的专属 `OpenID`；
3. 直接回复机器人：
   ```text
   /bind
   ```
   即可快速绑定您为系统管理员（也可以将收到的 OpenID 填入第二步的配置界面中保存）。

### 第四步：配置 ncmm 推送通知

打开 `ncmm web` ➔ **系统设置 ➔ 推送通知**，添加 Webhook 配置（或在 `config/notify.yaml` 中配置）：

```yaml
webhook:
  enabled: true
  url: "http://127.0.0.1:5606/notify"
  method: POST
```

并在 `config/config.yaml` 中确保启用了通知功能。配置后，日常打卡执行完毕就会直接私聊发送卡片给您的 QQ。

---

## 📱 QQ 私聊指令手册

向机器人发送以下私聊指令即可进行交互：

| 指令 | 说明 | 示例 |
| :--- | :--- | :--- |
| **发送文件（强烈推荐）** | 直接向机器人发送 `.txt` 或 `.json` 文件，自动提取 `MUSIC_U` 并识别账号身份更新 | 电脑拖入或手机发送 `fan_xxx.txt` / `cookie.txt` |
| **发送 Cookie 文本** | 自动提取 `MUSIC_U` 并识别账号身份，智能精准覆盖更新 | `/cookie MUSIC_U=xxxx` 或粘贴文本 |
| `/login` 或 `/qrcode` | 触发一键授权登录，私聊发送网易云官方授权链接，手机点击确认后自动保存凭据 | `/login` |
| `/status` 或 `/info` | 查看主账号与各辅助账号的 Cookie 状态、UID、昵称及文件存在性 | `/status` |
| `/run` | 立即在后台触发执行每日打卡与定时任务 | `/run` |
| `/myid` | 查看当前聊天发送者的专属 QQ OpenID | `/myid` |
| `/bind` | 当未配置管理员时，将当前发送者一键绑定为管理员 | `/bind` |
| `/help` | 查看完整的指令列表与使用提示 | `/help` |

---

## 💡 常见问题与说明 (FAQ)

### Q1: 在 Docker 容器中部署需要映射 5606 端口吗？
**完全不需要！**  
* `ncmm` 宿主向 `ncmm-qqbot` 发送通知中继通过内部回环地址（`127.0.0.1:5606`）完成，处于同一容器内，网络完全连通。
* `ncmm-qqbot` 与腾讯云端网关的通信是通过主动向外发起的 WebSocket 连接（WSS 长连接），不需要任何外部端口入站映射与防火墙放行。
* 配置修改已完全集成进 `ncmm web` 前端弹窗界面，因此无需暴露 5606 端口到宿主机。

### Q2: 为什么我的 Cookie 更新时提示“未匹配到已有账号”？
插件会解析 Cookie 中的 `MUSIC_U` 并联网请求网易云官方接口获取其 UID：
* 如果该 UID 与您当前系统中的**主账号**匹配，将自动更新主账号凭据；
* 如果与乐迷团**辅助账号 (fans)** 中的某个小号匹配，将自动更新对应的小号凭据；
* 如果系统配置中完全不存在该 UID，为了安全不会胡乱覆盖已有文件，会提示该账号未在系统中导入。

### Q3: 为什么更推荐直接向机器人发送 `.txt` 文本文件来更新 Cookie？
部分账号的网易云 `MUSIC_U` 参数是由 800+ 位密集十六进制字符组成的超长字符串。在 QQ 聊天输入框直接发送此类超长密集密文时，极易触发腾讯开放平台的内容安全反欺诈拦截机制（腾讯服务器端会直接静默丢弃，导致机器人根本收不到消息）。  
而以文件形式发送走的是腾讯对象存储文件通道，**不触发文本风控审查**，机器人能 100% 稳定接收并完成自动解析与绑定！

### Q4: 为什么不需要系统服务或开机自启？
本插件被设计为与 `ncmm web` 强绑定的伴生进程：
* 随着 `ncmm web` 进程的启动而自动拉起；
* 随着 `ncmm web` 进程的退出而优雅关闭；
* 避免了残留后台僵尸进程或由于服务自启引发的端口冲突。

---

## 🛠️ 参与开发与多平台自动构建

### 1. 本地编译

```bash
# 进入插件源码目录
cd plugins/ncmm-qqbot

# 依赖拉取
go mod tidy

# 编译 Windows 版本
go build -o ncmm-qqbot.exe .

# 编译 Linux 版本 (跨平台交叉编译)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ncmm-qqbot .
```

### 2. 发布新版本 (GitHub Actions + GoReleaser)

本仓库已配置自动化 CI/CD 工作流：
1. 提交代码更改并推送到 GitHub；
2. 打上语义化版本 Tag 并推送：
   ```bash
   git tag v1.0.0
   git push origin v1.0.0
   ```
3. GitHub Actions 将自动调用 GoReleaser 构建生成 Windows（amd64/arm64）、Linux（amd64/arm64）与 macOS（amd64/arm64）发布包，并自动发布 Release 资产，支持全平台 `ncmm plugin install` 一键下载安装。

---

## 📄 开源许可证

本项目基于 [MIT 许可证](LICENSE) 开源。
