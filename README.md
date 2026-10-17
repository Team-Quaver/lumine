# Lumine

纯 Go 实现的 Wayland 合成器 MVP。**不使用 CGO**：Wayland 协议、Vulkan 绑定、
共享内存、配置解析全部使用 Go 标准库与纯 Go 第三方库实现。需要原生库时
（Vulkan loader）通过 [purego](https://github.com/ebitengine/purego) 在运行时
`dlopen`，不链接任何 C 代码。

## 特性

- **Vulkan 渲染器**：通过 purego 直接调用 Vulkan 1.0（单个纹理四边形管线，
  离屏渲染后回读到 CPU 可见帧缓冲）。设备不可用时自动回退到软件渲染器，
  两条路径共用同一套合成逻辑，便于对照验证。
- **双布局模式**：
  - `tile`：类似 Niri 的无限横向滚动平铺。窗口按竖直列排列，视口沿列条带
    滚动，新窗口在焦点列右侧开新列。
  - `stack`：自由浮动窗口，按 z 序叠放。
- **色彩管理**：输出侧对合成结果做 sRGB → 目标色彩空间转换（PQ / HLG /
  linear），并通过 `wp_color_manager_v1` 暴露协议，客户端可创建参数化 /
  scRGB / BT.2100 image description、赋给 surface 并查询输出首选描述。
- **CSD 与 SSD 共存**：实现 `zxdg_decoration_manager_v1`。合成器默认绘制
  服务端装饰（标题栏 + 边框），客户端请求 CSD 时则完全不画，两种窗口可同时
  存在于同一输出。`[decor].mode` 控制服务端偏好，客户端最终决定。
- **配置热重载**：TOML 配置，inotify 监听文件变化，编辑保存即生效（布局
  模式 / 间距 / 列宽 / 色彩参数），无需重启，客户端不受影响。
- **luminectl**：通过 Unix 控制套接字操作运行中的合成器（焦点、滚动、列
  移动、模式切换、关闭窗口、生成进程、强制重载、状态查询）。
- **无 CGO**：`CGO_ENABLED=0 go build ./...` 可直接构建，产物是静态友好的
  纯 Go 二进制。

## 目录结构

```
Lumine/
├── cmd/
│   ├── lumine/           # 合成器入口：后端、渲染器、60Hz 帧循环
│   └── luminectl/        # 控制工具
├── internal/
│   ├── wl/               # 手写 Wayland 线协议 + 代码生成绑定（gen_*.go 勿手改）
│   ├── compositor/       # 全局对象、surface 状态机、seat、布局粘合、IPC、色彩管理
│   ├── layout/           # tile（列 + 滚动）与 stack（浮动 z 序）算法
│   ├── render/           # Framebuffer、CPU 渲染器、Vulkan 渲染器、色彩变换
│   ├── backend/          # 嵌套 Wayland 后端（纯 Go Wayland 客户端）
│   ├── config/           # TOML 配置 + inotify 热重载
│   ├── ipc/              # Unix socket 上的行式 JSON 控制协议
│   └── shm/              # memfd / mmap / 像素解码
├── protocol/             # Wayland 协议 XML（生成 internal/wl 绑定的输入）
└── config.example.toml   # 带注释的示例配置
```

## 构建

需要 Go（`go.mod` 指定 `go 1.26`）。无需 meson、无需 wlroots、无需 libwayland。

```sh
go build ./cmd/lumine ./cmd/luminectl

# 验证纯 Go（无 CGO）构建：
CGO_ENABLED=0 go build ./...
```

> 仓库根目录没有 `.go` 文件，因此不要执行 `go build .` 或 `go run .`。
> 使用 `make build` 构建到 `./bin`，使用 `make run ARGS="..."` 启动，或者
> 明确指定包路径，例如 `go run ./cmd/lumine ...`。

> 说明：`go list -deps` 会列出 `purego/internal/cgo` 包，这是 purego 的源码
> 布局，编译时在 `CGO_ENABLED=0` 下走纯 Go 的 `dlopen` 分支，不会产生 CGO
> 依赖。用 `CGO_ENABLED=0` 构建即可确认。

## 运行

Lumine 目前提供两个后端：

- `nested`（默认）：作为一个 Wayland 客户端运行在现有合成器里，把帧提交为
  `wl_shm` 缓冲并转发父合成器的输入。需要父合成器的 `WAYLAND_DISPLAY`。
- `headless`：不连任何父合成器，仅在内存中渲染，适合测试。

```sh
# 嵌套在现有会话中运行（父 WAYLAND_DISPLAY 指向当前桌面）：
WAYLAND_DISPLAY=wayland-1 \
  go run ./cmd/lumine -config ~/.config/lumine/config.toml

# 无头运行：
go run ./cmd/lumine -backend headless -renderer cpu

# 指定渲染器与窗口尺寸：
go run ./cmd/lumine -renderer vulkan -size 1920x1080
```

命令行参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-backend` | `nested` | `nested` 或 `headless` |
| `-renderer` | `auto` | `auto` / `vulkan` / `cpu`；Vulkan 不可用时回退 CPU |
| `-socket` | `lumine-wayland` | Wayland socket 名或绝对路径 |
| `-config` | `~/.config/lumine/config.toml` | TOML 配置路径 |
| `-terminal` | `$LUMINE_TERMINAL` 或 `kitty` | `spawn-terminal` 动作执行的命令 |
| `-size` | `1280x720` | 嵌套窗口初始尺寸 |
| `-verbose` | `false` | 打印渲染器与后端选择 |

### 启动子客户端

Lumine 通过 `-socket`（默认 `lumine-wayland`）在 `$XDG_RUNTIME_DIR` 下监听。
`spawn-terminal` 动作（以及 `luminectl spawn`）启动子进程时会**剥离父进程的
`WAYLAND_DISPLAY`**，改为指向 Lumine 自己的 socket，因此子客户端会连到
Lumine 而不是宿主桌面：

```sh
# 让一个终端连到已运行的 Lumine：
WAYLAND_DISPLAY=lumine-wayland kitty

# 或让 Lumine 直接拉起命令：
WAYLAND_DISPLAY=wayland-1 go run ./cmd/lumine kitty
```

## 配置与热重载

默认路径 `$XDG_CONFIG_HOME/lumine/config.toml`（即 `~/.config/lumine/config.toml`）。
文件由 inotify 监听，保存后立即重新加载并应用到运行中的窗口。完整示例见
[`config.example.toml`](./config.example.toml)：

```toml
[layout]
mode = "tile"       # "tile" | "stack"
gap = 8
column_ratio = 0.5

[color]
enabled = true
transfer = "srgb"   # "srgb" | "pq" | "hlg"
primaries = "srgb"  # "srgb" | "bt2020" | "dci-p3"
max_luma = 400

[decor]
mode = "auto"       # "auto" | "ssd" | "csd"

[bindings]
"super+t" = "mode-toggle"
```

- 缺失字段使用内置默认值；缺失文件不报错（等价于全默认配置）。
- `[bindings]` 与默认绑定**合并**，只写想改的按键即可。
- 热重载会应用布局模式、间距、列宽与色彩参数；绑定表变化在下一次按键时生效。
- 也可用 `luminectl reload` 强制重载。

## 快捷键（Super = Logo）

| 按键 | 动作 | 说明 |
| --- | --- | --- |
| `Super+Return` | `spawn-terminal` | 打开终端 |
| `Super+T` | `mode-toggle` | 切换 tile / stack |
| `Super+Left/Right` | `focus-left/right` | 跨列移动焦点 |
| `Super+Up/Down` | `focus-up/down` | 列内移动焦点（stack 模式为 z 序循环） |
| `Super+Shift+J/K` | `move-left/right` | 在列条带中左右移动当前列 |
| `Super+Shift+Left/Right` | `scroll-left/right` | 横向滚动词口 |
| `Super+Q` | `close` | 关闭焦点窗口 |

动作名即 `[bindings]` 里可填的值，与 `luminectl` 的语义一致。

## luminectl

控制套接字位于 `$XDG_RUNTIME_DIR/lumine-ctl.sock`。请求为行式 JSON，响应为
JSON。

```sh
luminectl status                 # 当前布局状态
luminectl outputs                # 输出列表
luminectl focus left             # 移动焦点（left/right/up/down）
luminectl move right             # 当前列右移
luminectl scroll 240             # 视口右移 240px
luminectl mode stack             # 切换布局模式（tile/stack/toggle）
luminectl close                  # 关闭焦点窗口
luminectl spawn kitty            # 生成进程（连到 Lumine）
luminectl reload                 # 强制重载配置
```

## 色彩管理

- **输出侧**：渲染器在合成完成后按 `[color]` 的 transfer / primaries /
  max_luma 对整帧做 sRGB → 目标空间的转换。`transfer = "srgb"` 时为零开销
  直通；`pq` / `hlg` / `linear` 会做逐像素重编码。
- **协议侧**（`wp_color_manager_v1`）：对外声明 parametric、set_luminances、
  windows_scrgb、windows_bt2100；客户端可创建 image description（命名
  transfer function / primaries + 亮度范围），描述创建后立即置为 ready，
  `get_information` 可回读参数。`wp_color_management_surface_v1` 上的
  image description 会被记录在对应 surface 上，surface feedback 的
  `get_preferred` 返回输出首选描述。
- **范围说明**：per-surface 的**像素级**转换（ICC profile、3D LUT、逐窗口
  色彩空间映射）尚未实现——描述目前作为元数据跟踪与回读，真正的像素变换在
  输出侧统一完成。不支持的参数（如 `set_tf_power`、显式色度坐标）会按协议
  返回 `failed(unsupported)`，而不是静默接受。

## 测试

```sh
go test ./...
go vet ./...

# 纯 Go 路径：
CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go test ./...
```

- `internal/wl`：线协议编解码、FD 传递、对象表。
- `internal/compositor`：完整客户端会话（registry → shm → xdg_toplevel →
  buffer release → frame callback → keyboard keymap → SSD 几何）与
  `wp_color_manager_v1` 参数化描述的就绪路径。
- `internal/layout`：列生成/聚焦/滚动/重排、模式往返、浮动窗口夹取。
- `internal/config`：默认值、绑定合并、非法模式回退。
- `internal/render`：CPU 与 Vulkan 合成结果一致性。

## 已知限制（MVP 范围）

- 仅有 `nested` 与 `headless` 后端，**尚未实现裸机 DRM/KMS**（因此 HDR 输出
  元数据在真实面板上的路径也未落地）。
- layer-shell / foreign-toplevel / screencopy / xdg_popup 的协议绑定已在
  `internal/wl` 生成，但尚未注册为全局对象，相关客户端暂不可用。
- 指针拖拽/缩放未实现：`xdg_toplevel.move/resize` 为空操作，stack 模式窗口
  目前不能拖动；也没有 `wl_pointer.set_cursor` 光标面渲染。
- per-surface 色彩像素转换（ICC/LUT）未实现（见上）。
- 单输出为主，`wl_output` 热插拔未实现。
- `wl_surface` 的 buffer scale / transform / damage 被忽略，每帧整屏重绘。

## 许可证

见仓库根目录。仓库历史中的旧 C / wlroots 实现已由当前 Go 实现取代。
