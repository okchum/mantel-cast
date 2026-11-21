# Mantel Cast

让 Google Nest Hub (Max) 一直亮着你的 [Mantel](https://github.com/okchum/mantel) 页面（其实任何网页都行）。也支持装了 Fully Kiosk Browser 的 Android 平板（见「其他设备」）。

Cast 协议只在局域网里工作，Google 也没有从云端启动设备上 App 的接口，所以 Mantel 放在外网时，家里得有个小程序盯着设备：这就是 Mantel Cast。它不提供页面、不采集数据，只做一件事：

- 设备回到待机/相册画面 → 重新投上去；
- 正在播 YouTube/Spotify → 不打扰；
- 夜里 → 主动关掉页面，让设备回到自己的暗色时钟，早上再投回来。

单个二进制，只用 Go 标准库和同作者的 [mantel-kit](https://github.com/okchum/mantel-kit)（和服务端共用的配置格式），没有别的依赖；Cast v2 协议和 mDNS 发现都是自己实现的，不需要 Python、catt、Home Assistant。

## 快速开始

```bash
make build                                     # 或 make dist 交叉编译 linux/arm64、amd64、树莓派、Windows……
./mantel-cast discover                         # 列出局域网里的 Cast 设备
cp mantel-cast.example.json mantel-cast.json   # 改 mantel 地址、令牌、设备名
export MANTEL_TOKEN=...                        # "$NAME" 读环境变量
./mantel-cast                                  # 常驻：守着设备
```

调试命令：

```bash
./mantel-cast status -device "Nest Hub Max"          # 设备上在跑什么
./mantel-cast cast   -device "Nest Hub Max" [url]    # 投一次（默认投配置里的页面）
./mantel-cast stop   -device "Nest Hub Max"          # 停掉设备上的 App
```

需要一台常开、和 Nest Hub 在同一网段的机器（NAS、树莓派、小主机都行）。mDNS 不跨 VLAN；跨网段时在 `cast.host` 里直接写设备 IP。

## 配置

```json
{
  "mantel": "https://mantel.example.com/",
  "token": "$MANTEL_TOKEN",
  "timezone": "Pacific/Auckland",
  "cast": {
    "device": "Nest Hub Max",
    "check_interval": "30s",
    "take_over": false,
    "quiet": "auto",
    "quiet_hours": "23:30-07:00"
  }
}
```

- `mantel`：页面地址。Mantel Cast 和 Mantel 在同一台机器上时写 `http://auto:8080/`，`auto` 代表本机的局域网地址，每次投屏前重新算，DHCP 换了地址也能跟上。
- `token`：Mantel 的访问令牌，Mantel Cast 会把它带进页面地址（`?k=`）。想在屏幕上按按钮，在 Mantel 那边给这个令牌 `"scopes": ["act"]`；这只对 Custom Receiver 有用，默认的 DashCast 不能触摸（见下文「触摸与 Custom Receiver」），给了 `act` 也按不了。
- Mantel Cast 访问 Mantel（投屏前探测页面、查询手机睡眠状态）时不走 `HTTP_PROXY`/`HTTPS_PROXY`：Mantel 一般在局域网里，探测结果也要和设备直连时看到的一样。
- 写错字段名会直接报错（`cast` 里的也一样）。
- 命令行参数 `-mantel`、`-token`、`-device`、`-host`、`-app` 覆盖配置。
- 不给 `-config` 时读当前目录的 `mantel-cast.json`。

### `cast` 字段

常用的：

| 字段 | 默认 | 说明 |
|---|---|---|
| `driver` | `cast` | 屏幕类型：`cast`（Nest Hub、Chromecast、带 Cast 的电视）或 `fully`（Fully Kiosk Browser，见「其他设备」）。 |
| `enabled` | `true` | `false` 时 `mantel-cast run` 什么都不做，只等退出信号（不退出，免得 systemd/Docker 反复重启）；`status`/`cast`/`stop` 调试命令照常可用。不写等于 `true`。 |
| `device` | 第一台 | 选设备：先找名字完全相同的（不分大小写），再按 UUID，最后按名字或型号的子串。匹配到多台时用第一台并在日志里警告。 |
| `host` | — | 设备 IP，写了就跳过 mDNS 发现（跨 VLAN 时用）。写了就要在路由器里给设备设 DHCP 固定地址，否则路由器重启后地址一变就连不上；不写时设备地址变了，Mantel Cast 会重新发现。 |
| `port` | `8009` | 设备的 Cast 端口，一般不用改。 |
| `app_id` | DashCast | 自己的 Custom Receiver 的 App ID（见下文）。只用于 `cast`。 |
| `password` | — | `fully` 的 Remote Admin 密码，`"$NAME"` 读环境变量。只用于 `fully`。 |
| `check_interval` | `30s` | 多久看一次设备。 |
| `take_over` | `false` | `true`：别的 App 在用也抢回来。 |
| `quiet` | `auto` | 见「自动安静」：`auto`、`hours`、`off`。 |
| `quiet_hours` | — | 固定安静时段，`"23:30-07:00"`；起止相同（`"00:00-00:00"`）永远不会生效，会直接报错，全天请写 `"00:00-24:00"`（`24:00` 只能做结束时间）。 |
| `reload_seconds` | `0` | DashCast 每隔这么多秒自己刷新页面；`0` 不刷新（Mantel 页面自己会更新）。只用于 `cast`。 |

时间参数一般不用动，时长写成 `"90s"`、`"5m"`、`"2h"`；不写或写 `0` 用默认值，负数报错：

| 字段 | 默认 | 说明 |
|---|---|---|
| `crash_if_up_under` | `2m` | 投上去不到这么久页面就没了，才算一次「被关」（崩溃）；显示了更久之后被关、或者被别的 App 顶掉，都不算。 |
| `relaunch_window` | `15m` | 这段时间内…… |
| `relaunch_limit` | `3` | ……被关这么多次，就暂停重投。 |
| `pause_min` | `10m` | 第一次暂停多久，之后每次翻倍…… |
| `pause_max` | `1h` | ……最长这么久。`pause_min` 不能大于 `pause_max`。 |
| `stable_after` | `30m` | 正常显示这么久后，暂停时长回到 `pause_min`。 |
| `sleep_report_ttl` | `16h` | 手机的「睡了」报告有效多久（手机没电、自动化删了，也不会一直黑屏）。 |
| `phone_known_for` | `36h` | 这么久内有过手机报告，就算「配了手机」，它说醒着会盖过时间表。 |
| `night_mode_refresh` | `5m` | 多久重新读一次设备的夜间模式设置（读到的结果在这段时间内直接复用）。 |
| `night_mode_stale_for` | `1h` | 读不到设备夜间模式时，沿用最后一次读到的多久。 |
| `discover_timeout` | `4s` | mDNS 发现等多久（`mantel-cast discover` 也用它）。 |
| `probe_timeout` | `8s` | 投屏前确认页面能打开，最多等多久。 |
| `mantel_timeout` | `8s` | 从 Mantel 读手机睡眠状态最多等多久。 |
| `command_timeout` | `15s` | 给设备发一条查询/停止命令最多等多久。 |
| `launch_timeout` | `30s` | 启动 App 并加载页面最多等多久。 |

### 直接用 Mantel 的 `config.json`

`mantel-cast -config config.json`：只读 `public_url`、`tokens`（优先名为 `mantel-cast`、其次 `nesthub` 的令牌）、`timezone`、`cast`，其余留给 Mantel；`cast` 段照样严格检查，写错字段名会报错。

`public_url` 留空时 Mantel 的页面就是 `http://<本机局域网 IP><listen>/`，Mantel Cast 按同样规则推出地址：取 `listen` 的端口（不写时 Mantel 默认 `:8080`），主机写成 `auto`。例如 `":8080"` → `http://auto:8080/`，`"0.0.0.0:9000"` → `http://auto:9000/`；配了 `tls_cert` 就用 `https://`（这时证书必须对这个 IP 有效，否则 Mantel Cast 会报 `bad certificate` 不投）。

## 守护规则

- 每 `check_interval`（默认 30s）看一次设备。
- 设备空闲（没有 App、Backdrop、相册画面）→ 投页面；别的 App 在用 → 不打扰（`take_over: true` 则强制抢回）。
- **哪个会话算自己的**：Mantel Cast 记下自己启动的会话 ID。别人（比如手机）用同一个 DashCast 投了别的网页，Mantel Cast 当它是别的 App：不重投、安静时也不关。唯一的例外是 Mantel Cast 刚启动时看到的那个会话：当成重启前自己投的，主动重载一次页面，避免设备卡在旧的错误页（DashCast 会先停掉旧会话再重开，因为 `force` 加载后旧会话收不到新地址）；如果这时正处在安静时段，就直接关掉它。
- **先确认页面能打开再投**：投之前从本机带着令牌请求一下页面（不跟随跳转），2xx/3xx 才投。没有应答（日志 `not casting: … does not answer`）、令牌不对（`Mantel rejected the token (HTTP 401)`）、没配令牌（`the page needs a token`）或其他错误状态都不投，免得设备卡在 DashCast 的「Force loading …」画面或 Mantel 的 401 页上。HTTPS 页面会像设备一样校验证书，证书不受信任时日志是 `not casting: … bad certificate (…)`：设备反正也打不开。
- **反复被关就暂停**：页面投上去不到 2 分钟就没了算一次被关；15 分钟内被关 3 次，就暂停 10 分钟再试；还这样就 20、40 分钟，最长 1 小时。正常显示满 30 分钟后恢复。看 YouTube 之类把页面顶掉不算。这些数字都能在 `cast` 里改（见上表）。
- **断线重连**：先直连上次连上的地址（会先问设备的名字和 UUID，路由器把这个地址分给了另一台设备时不会投错），连不上才重新 mDNS 发现；重连等待从 5 秒起翻倍、最长 30 秒（路由器重启这类几分钟的断网，网络一恢复半分钟内就能投回去），连接稳定超过 1 分钟后回到 5 秒。设备拒绝某条命令（比如 `LAUNCH_ERROR`）只记日志、下次检查再试，不断开连接。
- 固定地址最省心：在路由器里给 Mantel 那台机器做 DHCP 地址保留。

## 自动安静

`cast.quiet` 默认 `"auto"`，按下面顺序判断，前面的说了算。安静时 Mantel Cast 会**主动关掉页面**（别的 App 正在用时不碰），安静结束自动投回来。

1. **设备的勿扰模式**：「Hey Google，打开勿扰模式」或在 Google Home App 里打开。和夜间模式一起读，最多 5 分钟（`night_mode_refresh`）后生效。
2. **手机睡眠状态**：iPhone 进入「睡眠」专注模式时由快捷指令报给 Mantel（`POST /api/v1/quiet?asleep=1`，设置方法见 Mantel 的 README），Mantel Cast 去 Mantel 读。手机报过状态之后，它说「醒着」会盖过下面的时间表，所以熬夜、周末睡懒觉都跟得上。睡眠报告 16 小时后失效，36 小时没有任何报告就当没配手机（`sleep_report_ttl`、`phone_known_for`）。报告时间是 Mantel 那台机器的时钟，有效期用 Mantel Cast 这台机器的时钟算；两者装在不同机器上时，两台都要开启 NTP 自动校时。
3. **设备的夜间模式时间表**：Google Home App → 这台设备 → 设置 → 夜间模式。Mantel Cast 从局域网直接读（不用登录），每 5 分钟读一次（`night_mode_refresh`），读不到时沿用 1 小时内最后一次读到的（`night_mode_stale_for`）。
4. **`quiet_hours`**：配置里的固定时段，比如 `"23:30-07:00"`，兜底。

`"quiet": "hours"` 只看 `quiet_hours`；`"off"` 从不安静。`fully` 没有设备自己的勿扰/夜间模式，只看手机和 `quiet_hours`。

## 触摸与 Custom Receiver（可选，最稳）

默认的 DashCast 开箱即用，但**不能触摸**，会话偶尔被系统收回（Mantel Cast 会补投）。要触摸、不超时：

1. 在 [Google Cast SDK Developer Console](https://cast.google.com/publish) 注册（一次性 5 美元），新建 **Custom Receiver**，URL 填 `https://你的 Mantel/?receiver=1&k=令牌`，并把这台 Hub 的序列号登记为测试设备（设备重启后生效）。
2. 页面必须走 HTTPS 且证书受信任。
3. 把 `cast.app_id` 改成控制台给的 App ID。这时 Mantel Cast 只发「启动这个 App」，网址在 Cast 后台里配。

## 其他设备

设备那一层是可替换的驱动（`internal/display` 定义接口：发现、连接、看屏幕上是什么、显示页面、收起页面），守护规则、安静判断、崩溃暂停对所有驱动都一样。

**Fully Kiosk Browser**（`"driver": "fully"`）：Android 平板 / Fire 平板 / 旧手机上常用的 kiosk 浏览器，适合挂墙。

1. Fully 设置 → Remote Administration 打开（PLUS 授权），设一个密码；Mantel Cast 这台机器要能访问平板的 2323 端口。
2. 配置：

   ```json
   "cast": { "driver": "fully", "host": "192.168.1.40", "password": "$FULLY_PASSWORD", "quiet_hours": "23:30-07:00" }
   ```

   Fully 不在局域网里广播自己，必须写 `host`（`discover` 不适用）。
3. 行为：屏幕关着、或停在 Fully 的起始页 → 亮屏、切到前台、打开页面；前台是别的 App 或别的网页 → 不打扰；安静时段 → 关屏。`status` 显示屏幕开关、前台 App 和当前页面（不带令牌），`stop` 关屏。

**还能接的同类技术**（实现 `display.Driver` 即可）：

| 技术 | 设备 | 怎么控制 | 状态 |
|---|---|---|---|
| Google Cast（DashCast / Custom Receiver） | Nest Hub、Chromecast、Google TV、带 Cast 的电视 | Cast v2 协议 + mDNS | 已支持（`cast`） |
| Fully Kiosk Browser | Android / Fire 平板 | Remote Admin REST | 已支持（`fully`） |
| Chromium kiosk + DevTools | 树莓派、小主机接的显示器 | `--remote-debugging-port` 的 `/json/list`、`/json/new?url` | 可做 |
| Android TV / Fire TV | 电视盒子 | ADB `am start -a android.intent.action.VIEW -d <url>` | 可做（要开 ADB 调试） |
| Home Assistant browser_mod | 任何开着 HA 页面的浏览器 | HA 服务调用 `browser_mod.navigate` | 可做（依赖 HA） |

不适合的：AirPlay、DLNA/UPnP、Miracast 只能投视频/图片或镜像屏幕，不能让设备自己打开网页；Echo Show 没有开放的局域网接口。

## 目录

```
main.go             CLI：run / discover / status / cast / stop；配置读取
internal/display    屏幕驱动接口（Driver / Session / Screen）和设备夜间模式的时间窗
internal/cast       Cast 驱动：Cast v2 协议（手写 protobuf）、mDNS 发现、设备 setup 接口（夜间模式、身份）
internal/fully      Fully Kiosk Browser 驱动（Remote Admin REST）
internal/keeper     守护循环、安静判断、页面地址和令牌（不依赖具体驱动）
internal/config     cast 配置（时长、时间窗的格式来自 mantel-kit）
```
