# Lumine

基于 wlroots 0.21 的轻量 Wayland 合成器 MVP，参考 `/home/ne0w0r1d/Desktop/wlroots` 的 wlroots 代码实现。

核心特性：

- **Tiling / Stack 双布局模式**：Tiling 为 master-stack 自动平铺；Stack 为层叠浮动窗口（可拖动/缩放），`Super+M` 随时切换。
- **窗口动画**：打开时淡入 + 上浮 14px（220ms）；布局变化时窗口平滑滑动（170ms，ease-out cubic）；**关闭时对最后一帧快照做原位淡出**（200ms）。全部由事件循环定时器驱动，可与 Noctalia 共存。
- **HDR / 色彩管理**：实现 `wp_color_management_v1`（v2）与 `wp_color_representation_v1` 协议；输出支持时自动启用 HDR（BT.2020 + ST.2084 PQ image description，走 DRM `HDR_OUTPUT_METADATA`），`Super+X` 手动开关。
- **完整 Shell 支持**：layer-shell（背景/顶栏/dock/通知/tooltip）、foreign-toplevel（任务栏）、xdg-shell、screencopy、virtual keyboard/pointer 等，已用 **Noctalia** 壳完整验证（壁纸 + 顶栏 + dock + 通知全部正常）。

## 目录结构

```
Lumine/
├── meson.build meson.options    # 构建定义
├── src/
│   ├── main.c                   # 初始化、协议注册、测试驱动
│   ├── output.c                 # 输出生命周期、HDR 启用/切换、layer 排布
│   ├── layout.c                 # Tiling/Stack 布局、焦点、命中测试
│   ├── toplevel.c               # xdg toplevel/popup、foreign-toplevel
│   ├── layer.c                  # layer-shell 表面
│   ├── input.c                  # 键盘/指针、拖拽、快捷键
│   └── lumine.h
├── tools/lumine-shot.c          # 截图工具（见下）
└── .deps/                       # 本地构建的 wlroots 0.21-dev（prefix）
```

## 构建

wlroots 未装入系统，构建脚本会把用户 checkout 的 wlroots 编译安装到 `Lumine/.deps`（一次性，已完成）：

```sh
# 如需重建 wlroots：
export PATH="$HOME/.local/bin:$PATH"   # meson 通过 pip --user 安装
export PKG_CONFIG_PATH=/usr/share/pkgconfig:$PWD/.deps/pc   # .deps/pc/hwdata.pc 为本地补的 pc 文件
meson setup .deps/wlroots-build /home/ne0w0r1d/Desktop/wlroots \
    --wrap-mode=default -Dexamples=false -Dtests=false -Dxwayland=disabled \
    --prefix=$PWD/.deps -Dv4l-utils:werror=false -Dv4l-utils:udevdir=$PWD/.deps/udev
meson compile -C .deps/wlroots-build && meson install -C .deps/wlroots-build

# 构建 Lumine：
export PKG_CONFIG_PATH=$PWD/.deps/lib64/pkgconfig
meson setup build && meson compile -C build
```

## 运行

支持命令行启动命令（类似 weston）：

```sh
export LD_LIBRARY_PATH=$PWD/.deps/lib64
# 嵌套在现有会话里测试（不影响当前桌面）：
WLR_BACKENDS=wayland WLR_LIBINPUT_NO_DEVICES=1 WAYLAND_DISPLAY=wayland-1 ./build/lumine
# 子进程会继承 WAYLAND_DISPLAY（lumine 自己的 socket），直接 kitty / noctalia 即可

# 启动时直接拉起命令：
WLR_BACKENDS=wayland WAYLAND_DISPLAY=wayland-1 ./build/lumine kitty
WLR_BACKENDS=wayland WAYLAND_DISPLAY=wayland-1 ./build/lumine "kitty && noctalia"
```

真机会话（DRM backend，支持 HDR）：从 TTY 直接 `./build/lumine`。

> 注意：plasmashell 这类 KDE 组件依赖 KWin 私有协议（org_kde_plasma_surface、plasma-window-management 等），在 wlroots 系合成器上不能正常工作——shell 角色请用 Noctalia（纯 layer-shell，已验证）。启动时创建/销毁 layer surface 的客户端不会伤到 lumine（有回归测试覆盖）。

## 快捷键（Super = Logo）

| 按键 | 功能 |
| --- | --- |
| `Super+Return` | 打开终端（`LUMINE_TERMINAL` 可覆盖，默认 kitty） |
| `Super+M` | **切换 Tiling / Stack 模式** |
| `Super+J` / `Super+K` | 焦点移到下/上一个窗口（方向键亦可） |
| `Super+Shift+J` / `Super+Shift+K` | 平铺模式下移动窗口顺序 |
| `Super+H` / `Super+L` | 调整 master 栏宽度（20%–80%） |
| `Super+Q` | 关闭焦点窗口 |
| `Super+F` | 切换全屏 |
| `Super+X` | **切换焦点输出 HDR 开/关** |
| `Super+左键拖动` | 移动窗口（平铺窗口拖动即转为浮动） |
| `Super+右键拖动` | 缩放浮动窗口 |
| `Super+Shift+E` | 退出 Lumine |

## HDR / 色彩管理说明

- 协议侧：`wp_color_manager_v1` v2（parametric image description、mastering display primaries）+ `wp_color_representation_v1`，并通过 `wlr_scene_set_color_manager_v1` 接入场景图，客户端可查询/标记色彩属性。
- 输出侧：输出枚举时检测 `supported_transfer_functions & ST2084_PQ` 与 `supported_primaries & BT2020`，逐个尝试 10bpc/8bpc render format，`wlr_output_test_state` 通过后提交 BT.2020+PQ image description（DRM atomic 后端会转成内核 `HDR_OUTPUT_METADATA` blob）。`Super+X` 随时切换。
- 注意：HDR 依赖 DRM 后端与面板 EDID 支持度。嵌套 wayland / headless 后端没有色彩属性，会如实打印 `HDR not supported by backend/panel`，这是正确行为。
- 截图提示：NVIDIA GLES2 的 readback 格式是 24bpp BG24，`grim`（cairo）不支持，所以自带 `tools/lumine-shot`（screencopy 客户端，支持 BG24/RGB888/32bpp 系列，zlib 手写 PNG 编码）：
  ```sh
  WAYLAND_DISPLAY=wayland-0 ./build/lumine-shot /tmp/shot.png
  ```

## 测试钩子与工具

无输入设备自动化测试用（也是演示钩子）：

- `LUMINE_SPAWN="cmd1;;cmd2"`：启动后依次执行。
- `LUMINE_TEST="<秒> <命令>; <秒> <命令>; ..."`：事件循环定时执行，命令：`mode`、`focus next|prev`、`move next|prev`、`hdr`、`spawn <cmd>`、`quit`。
- `build/lumine-shot <out.png>`：screencopy 截图（支持 NVIDIA 的 24bpp BG24 readback，grim 不支持）。
- `build/lumine-input abs|click|press|release|drag ...`：经 virtual-pointer 注入合成指针事件（拖拽/点击回归测试用）。
- `build/lumine-layer-test`：layer surface「映射→销毁」生命周期回归测试。

例如完整回归：

```sh
WLR_BACKENDS=wayland WAYLAND_DISPLAY=wayland-1 \
LUMINE_TEST="1.5 spawn kitty; 2 spawn kitty; 3 mode; 4 mode; 5 quit" ./build/lumine
```

## 已验证

- [x] Tiling（master-stack 平铺、比例调整、顺序移动）
- [x] Stack（层叠、Super 拖动/缩放、模式往返切换）
- [x] 窗口动画（fade-in+上浮 / 布局 glide / 关闭快照淡出，帧级截图验证）
- [x] CSD 标题栏拖拽（kitty 子表面 + 隐式抓取保持）与拖拽后不黏鼠标
- [x] `wp_color_manager_v1` v2 + `wp_color_representation_v1` 注册（wayland-info 验证）
- [x] HDR 探测/开关代码路径（嵌套后端正确报告不支持；DRM 上随面板能力启用）
- [x] Noctalia 壳：壁纸、顶栏（exclusive zone 正确压缩工作区）、dock、通知、tooltip popup
- [x] xdg popup（含 layer-shell 两段式挂载的 tooltip、layer 弹出菜单）
- [x] layer surface map→destroy 生命周期（lumine-layer-test 回归）
- [x] 干净退出（listener 清理，无 wlroots 断言）

## 实现要点（踩坑记录）

- **隐式抓取**：按钮按住期间不能对每次 motion 重发 `notify_enter`——CSD 标题栏是独立子表面，光标移出会打断客户端的抓取状态机（kitty 拖拽失效）。抓取期间焦点钉住按下表面，坐标用按下时记录的 scene node 换算。
- **request_move 防黏鼠标**：客户端（kitty）可能在按钮释放后再发一次 `xdg_toplevel.move`；合成器需自行跟踪按住的按键数，仅在 `button_count > 0` 时接受交互式移动。
- **virtual pointer/keyboard**：不经过 backend 的 new_input，管理器只发 `new_virtual_pointer/new_virtual_keyboard` 信号，需要合成器自己 attach 到 cursor/seat。
- **layer popup 两段式**：layer-shell 的 tooltip 先 `xdg_surface.get_popup(NULL)` 创建无父 popup，再由 `zwlr_layer_surface_v1.get_popup` 挂载并二次触发 new_popup；中途销毁 popup 会让客户端后续请求引用死对象（wlroots 断言崩溃）。
- **layer surface 生命周期**：unmap 阶段（含客户端销毁 role resource 触发的 unmap）不能对未映射表面发 configure——arrange 需跳过未 `mapped` 的层。
- **listener 生命周期**：`wl_signal_emit` 不会摘除 listener，节点销毁回调里必须先 `wl_list_remove` 自己，否则 wlroots 的 scene 断言崩溃。
- **防 configure 风暴**：`wlr_xdg_toplevel_set_size`/layer configure 都不去重，arrange 必须由状态变化驱动（toplevel 尺寸去重 + layer 状态比对），而不是每次 output commit。
- **关闭动画快照**：wlroots 0.21 在每次 commit 结束即释放 `surface->current.buffer`（置 NULL），所以要在 commit 信号里自己 `wlr_buffer_lock` 留住最后一帧，unmap 时交给 scene buffer 淡出。快照只含主表面——kitty 这类把标题栏画在子表面里的客户端，其标题栏不在快照中（body 淡出可见）；要完整幽灵需场景级截图管线，MVP 未做。

## 已知限制（MVP 范围外）

- 单输出布局为主，多输出可用但窗口按焦点输出分配。
- 无配置文件；快捷键硬编码。
- fullscreen 请求只回 configure，不做真全屏状态跟踪。
- 未实现 xwayland、输出热插拔布局策略、`wlr-output-management`。
