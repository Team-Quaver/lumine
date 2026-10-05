project('lumine', 'c',
	version: '0.1.0',
	license: 'MIT',
	default_options: [
		'c_std=c11',
		'warning_level=2',
	],
)

add_project_arguments([
	'-D_POSIX_C_SOURCE=200809L',
	'-Wno-unused-parameter',
], language: 'c')

cc = meson.get_compiler('c')

wlroots_dep = dependency('wlroots-0.21')
wayland_server = dependency('wayland-server')
xkbcommon = dependency('xkbcommon')
math = cc.find_library('m')

# The generated wayland-scanner protocol headers ship in the wlroots build
# dir (our local .deps install) rather than the installed header set.
wlr_protocol_inc = meson.project_source_root() / '.deps' / 'wlroots-build' / 'protocol'

lumine_files = [
	'src/main.c',
	'src/output.c',
	'src/toplevel.c',
	'src/layer.c',
	'src/layout.c',
	'src/input.c',
	'src/animation.c',
]

executable('lumine', lumine_files,
	dependencies: [wlroots_dep, wayland_server, xkbcommon, math],
	c_args: ['-I' + wlr_protocol_inc],
)

# Test helper: screencopy client that handles the 24bpp readback format
# NVIDIA GLES2 prefers (grim chokes on it).
wayland_scanner = find_program('wayland-scanner', native: true)
wayland_client = dependency('wayland-client')
zlib_dep = dependency('zlib')
wayland_protocols = dependency('wayland-protocols')
screencopy_xml = get_option('wlr_src_dir') / 'protocol' / 'wlr-screencopy-unstable-v1.xml'
screencopy_client_h = custom_target(
	'screencopy_client_h',
	input: screencopy_xml,
	output: 'wlr-screencopy-unstable-v1-client-protocol.h',
	command: [wayland_scanner, 'client-header', '@INPUT@', '@OUTPUT@'],
)
screencopy_client_c = custom_target(
	'screencopy_client_c',
	input: screencopy_xml,
	output: 'wlr-screencopy-unstable-v1-protocol.c',
	command: [wayland_scanner, 'private-code', '@INPUT@', '@OUTPUT@'],
)
executable('lumine-shot', 'tools/lumine-shot.c',
	screencopy_client_h, screencopy_client_c,
	dependencies: [wayland_client, zlib_dep],
	c_args: ['-I' + meson.current_build_dir()],
)

# Test helper: synthetic pointer events via virtual-pointer protocol.
virtual_pointer_xml = get_option('wlr_src_dir') / 'protocol' / 'wlr-virtual-pointer-unstable-v1.xml'
virtual_pointer_client_h = custom_target(
	'virtual_pointer_client_h',
	input: virtual_pointer_xml,
	output: 'wlr-virtual-pointer-unstable-v1-client-protocol.h',
	command: [wayland_scanner, 'client-header', '@INPUT@', '@OUTPUT@'],
)
virtual_pointer_client_c = custom_target(
	'virtual_pointer_client_c',
	input: virtual_pointer_xml,
	output: 'wlr-virtual-pointer-unstable-v1-protocol.c',
	command: [wayland_scanner, 'private-code', '@INPUT@', '@OUTPUT@'],
)
executable('lumine-input', 'tools/lumine-input.c',
	virtual_pointer_client_h, virtual_pointer_client_c,
	dependencies: [wayland_client],
	c_args: ['-I' + meson.current_build_dir()],
)

# Test helper: map-then-destroy layer surface lifecycle (compositor crash
# regression test).
layer_shell_xml = get_option('wlr_src_dir') / 'protocol' / 'wlr-layer-shell-unstable-v1.xml'
layer_shell_client_h = custom_target(
	'layer_shell_client_h',
	input: layer_shell_xml,
	output: 'wlr-layer-shell-unstable-v1-client-protocol.h',
	command: [wayland_scanner, 'client-header', '@INPUT@', '@OUTPUT@'],
)
layer_shell_client_c = custom_target(
	'layer_shell_client_c',
	input: layer_shell_xml,
	output: 'wlr-layer-shell-unstable-v1-protocol.c',
	command: [wayland_scanner, 'private-code', '@INPUT@', '@OUTPUT@'],
)
xdg_shell_xml = wayland_protocols.get_variable('pkgdatadir') / 'stable' / 'xdg-shell' / 'xdg-shell.xml'
xdg_shell_client_c = custom_target(
	'xdg_shell_client_c',
	input: xdg_shell_xml,
	output: 'xdg-shell-protocol.c',
	command: [wayland_scanner, 'private-code', '@INPUT@', '@OUTPUT@'],
)
executable('lumine-layer-test', 'tools/lumine-layer-test.c',
	layer_shell_client_h, layer_shell_client_c, xdg_shell_client_c,
	dependencies: [wayland_client, wayland_protocols],
	c_args: ['-I' + meson.current_build_dir()],
)

# Test helper: CSD-style client that starts an interactive move on button
# press and requires the matching release afterwards (stuck-grab regression
# test: a compositor that swallows the release freezes pointer input).
xdg_shell_client_h = custom_target(
	'xdg_shell_client_h',
	input: xdg_shell_xml,
	output: 'xdg-shell-client-protocol.h',
	command: [wayland_scanner, 'client-header', '@INPUT@', '@OUTPUT@'],
)
executable('lumine-drag-test', 'tools/lumine-drag-test.c',
	xdg_shell_client_h, xdg_shell_client_c,
	dependencies: [wayland_client, wayland_protocols],
	c_args: ['-I' + meson.current_build_dir()],
)
