#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "lumine.h"
#include <wlr/types/wlr_compositor.h>
#include <wlr/types/wlr_screencopy_v1.h>
#include <wlr/types/wlr_subcompositor.h>
#include <wlr/types/wlr_viewporter.h>
#include <wlr/types/wlr_virtual_keyboard_v1.h>
#include <wlr/types/wlr_virtual_pointer_v1.h>
#include <wlr/types/wlr_xdg_activation_v1.h>
#include <wlr/types/wlr_color_management_v1.h>
#include <wlr/types/wlr_color_representation_v1.h>
#include <wlr/types/wlr_xdg_output_v1.h>
#include <wlr/util/log.h>
void lumine_spawn(const char *cmd) {
	pid_t pid = fork();
	if (pid < 0) {
		wlr_log(WLR_ERROR, "fork() failed");
		return;
	}
	if (pid == 0) {
		setsid();
		execl("/bin/sh", "/bin/sh", "-c", cmd, (void *)NULL);
		_exit(EXIT_FAILURE);
	}
}

struct lumine_output *lumine_focused_output(struct lumine_server *server) {
	struct wlr_output *wlr_output = wlr_output_layout_output_at(
		server->output_layout, server->cursor->x, server->cursor->y);
	struct lumine_output *output;
	wl_list_for_each(output, &server->outputs, link) {
		if (output->wlr_output == wlr_output) {
			return output;
		}
	}
	if (!wl_list_empty(&server->outputs)) {
		return wl_container_of(server->outputs.next, output, link);
	}
	return NULL;
}

/*
 * Minimal test driver: LUMINE_TEST="<delay> <cmd>; <delay> <cmd>; ..."
 * runs commands on the event loop after delays (seconds). Used to exercise
 * keybindings without an input device. Commands: mode, focus {next,prev},
 * move {next,prev}, hdr, spawn <cmd>, quit.
 */
struct lumine_test {
	struct lumine_server *server;
	char **cmds;
	size_t count;
	size_t index;
};

static int test_timer(void *data);

static void test_schedule_next(struct lumine_test *test) {
	if (test->index >= test->count) {
		return;
	}
	char *step = test->cmds[test->index];
	double delay = strtod(step, &step);
	while (*step == ' ') {
		step++;
	}
	struct wl_event_loop *loop =
		wl_display_get_event_loop(test->server->display);
	struct wl_event_source *source =
		wl_event_loop_add_timer(loop, test_timer, test);
	wl_event_source_timer_update(source, (int)(delay * 1000.0));
}

static int test_timer(void *data) {
	struct lumine_test *test = data;
	struct lumine_server *server = test->server;
	char *step = test->cmds[test->index];

	/* Step format: "<delay> <command...>" */
	char *cmd = step;
	strtod(cmd, &cmd);
	while (*cmd == ' ') {
		cmd++;
	}
	test->index++;

	wlr_log(WLR_INFO, "test driver: %s", cmd);
	if (strncmp(cmd, "mode", 4) == 0) {
		lumine_toggle_mode(server);
	} else if (strncmp(cmd, "focus next", 10) == 0) {
		lumine_focus_next(server, 1);
	} else if (strncmp(cmd, "focus prev", 10) == 0) {
		lumine_focus_next(server, -1);
	} else if (strncmp(cmd, "move next", 9) == 0) {
		lumine_move_focused(server, 1);
	} else if (strncmp(cmd, "move prev", 9) == 0) {
		lumine_move_focused(server, -1);
	} else if (strncmp(cmd, "hdr", 3) == 0) {
		struct lumine_output *output = lumine_focused_output(server);
		if (output != NULL) {
			lumine_output_toggle_hdr(output);
		}
	} else if (strncmp(cmd, "spawn ", 6) == 0) {
		lumine_spawn(cmd + 6);
	} else if (strncmp(cmd, "quit", 4) == 0) {
		wl_display_terminate(server->display);
	}

	test_schedule_next(test);
	return 0;
}

static void test_driver_start(struct lumine_server *server) {
	const char *script = getenv("LUMINE_TEST");
	if (script == NULL || script[0] == '\0') {
		return;
	}
	char *buf = strdup(script);

	size_t count = 0;
	for (const char *p = buf; *p != '\0'; p++) {
		if (*p == ';') {
			count++;
		}
	}
	char **cmds = calloc(count + 1, sizeof(char *));
	size_t n = 0;
	char *saveptr = NULL;
	for (char *step = strtok_r(buf, ";", &saveptr); step != NULL;
			step = strtok_r(NULL, ";", &saveptr)) {
		cmds[n++] = step;
	}

	struct lumine_test *test = calloc(1, sizeof(*test));
	test->server = server;
	test->cmds = cmds;
	test->count = n;
	test_schedule_next(test);
	wlr_log(WLR_INFO, "test driver started with %zu steps", n);
}

int main(int argc, char *argv[]) {
	wlr_log_init(WLR_INFO, NULL);

	struct lumine_server server = {0};
	server.mode = LUMINE_LAYOUT_TILING;
	server.master_ratio = 0.55;
	wl_list_init(&server.outputs);
	wl_list_init(&server.toplevels);
	wl_list_init(&server.layers);
	wl_list_init(&server.keyboards);

	server.display = wl_display_create();
	lumine_anims_init(&server);
	server.backend = wlr_backend_autocreate(
		wl_display_get_event_loop(server.display), NULL);
	if (server.backend == NULL) {
		wlr_log(WLR_ERROR, "failed to create backend");
		return 1;
	}

	server.renderer = wlr_renderer_autocreate(server.backend);
	if (server.renderer == NULL) {
		wlr_log(WLR_ERROR, "failed to create renderer");
		return 1;
	}
	wlr_renderer_init_wl_display(server.renderer, server.display);

	server.allocator = wlr_allocator_autocreate(server.backend,
		server.renderer);
	if (server.allocator == NULL) {
		wlr_log(WLR_ERROR, "failed to create allocator");
		return 1;
	}

	/* Core protocols. */
	wlr_compositor_create(server.display, 5, server.renderer);
	wlr_subcompositor_create(server.display);
	wlr_data_device_manager_create(server.display);
	wlr_viewporter_create(server.display);
	wlr_screencopy_manager_v1_create(server.display);
	wlr_xdg_activation_v1_create(server.display);
	server.virtual_keyboard_manager =
		wlr_virtual_keyboard_manager_v1_create(server.display);
	server.virtual_pointer_manager =
		wlr_virtual_pointer_manager_v1_create(server.display);

	/* Color management: wp_color_management_v1 + wp_color_representation_v1. */
	static const enum wp_color_manager_v1_render_intent render_intents[] = {
		WP_COLOR_MANAGER_V1_RENDER_INTENT_PERCEPTUAL,
		WP_COLOR_MANAGER_V1_RENDER_INTENT_RELATIVE,
		WP_COLOR_MANAGER_V1_RENDER_INTENT_SATURATION,
		WP_COLOR_MANAGER_V1_RENDER_INTENT_ABSOLUTE,
	};
	static const enum wp_color_manager_v1_transfer_function transfer_functions[] = {
		WP_COLOR_MANAGER_V1_TRANSFER_FUNCTION_BT1886,
		WP_COLOR_MANAGER_V1_TRANSFER_FUNCTION_GAMMA22,
		WP_COLOR_MANAGER_V1_TRANSFER_FUNCTION_GAMMA28,
		WP_COLOR_MANAGER_V1_TRANSFER_FUNCTION_ST240,
		WP_COLOR_MANAGER_V1_TRANSFER_FUNCTION_EXT_LINEAR,
		WP_COLOR_MANAGER_V1_TRANSFER_FUNCTION_LOG_100,
		WP_COLOR_MANAGER_V1_TRANSFER_FUNCTION_LOG_316,
		WP_COLOR_MANAGER_V1_TRANSFER_FUNCTION_XVYCC,
		WP_COLOR_MANAGER_V1_TRANSFER_FUNCTION_EXT_SRGB,
		WP_COLOR_MANAGER_V1_TRANSFER_FUNCTION_ST2084_PQ,
		WP_COLOR_MANAGER_V1_TRANSFER_FUNCTION_ST428,
		WP_COLOR_MANAGER_V1_TRANSFER_FUNCTION_HLG,
		WP_COLOR_MANAGER_V1_TRANSFER_FUNCTION_COMPOUND_POWER_2_4,
	};
	static const enum wp_color_manager_v1_primaries primaries[] = {
		WP_COLOR_MANAGER_V1_PRIMARIES_SRGB,
		WP_COLOR_MANAGER_V1_PRIMARIES_PAL_M,
		WP_COLOR_MANAGER_V1_PRIMARIES_PAL,
		WP_COLOR_MANAGER_V1_PRIMARIES_NTSC,
		WP_COLOR_MANAGER_V1_PRIMARIES_GENERIC_FILM,
		WP_COLOR_MANAGER_V1_PRIMARIES_BT2020,
		WP_COLOR_MANAGER_V1_PRIMARIES_CIE1931_XYZ,
		WP_COLOR_MANAGER_V1_PRIMARIES_DCI_P3,
		WP_COLOR_MANAGER_V1_PRIMARIES_DISPLAY_P3,
		WP_COLOR_MANAGER_V1_PRIMARIES_ADOBE_RGB,
	};
	struct wlr_color_manager_v1_options cm_options = {
		.features = {
			.parametric = true,
			.set_mastering_display_primaries = true,
		},
		.render_intents = render_intents,
		.render_intents_len = sizeof(render_intents) / sizeof(render_intents[0]),
		.transfer_functions = transfer_functions,
		.transfer_functions_len = sizeof(transfer_functions) / sizeof(transfer_functions[0]),
		.primaries = primaries,
		.primaries_len = sizeof(primaries) / sizeof(primaries[0]),
	};
	struct wlr_color_manager_v1 *color_manager =
		wlr_color_manager_v1_create(server.display, 2, &cm_options);
	if (color_manager == NULL) {
		wlr_log(WLR_ERROR, "failed to create color manager");
		return 1;
	}
	wlr_color_representation_manager_v1_create_with_renderer(
		server.display, 1, server.renderer);

	/* Scene graph. */
	server.output_layout = wlr_output_layout_create(server.display);
	server.scene = wlr_scene_create();
	server.scene_layout =
		wlr_scene_attach_output_layout(server.scene, server.output_layout);
	wlr_xdg_output_manager_v1_create(server.display, server.output_layout);
	server.tree_background = wlr_scene_tree_create(&server.scene->tree);
	server.tree_bottom = wlr_scene_tree_create(&server.scene->tree);
	server.tree_tiling = wlr_scene_tree_create(&server.scene->tree);
	server.tree_floating = wlr_scene_tree_create(&server.scene->tree);
	server.tree_animation = wlr_scene_tree_create(&server.scene->tree);
	server.tree_top = wlr_scene_tree_create(&server.scene->tree);
	server.tree_overlay = wlr_scene_tree_create(&server.scene->tree);
	wlr_scene_set_color_manager_v1(server.scene, color_manager);

	/* Shell protocols. */
	server.xdg_shell = wlr_xdg_shell_create(server.display, 3);
	server.new_xdg_toplevel.notify = lumine_handle_new_xdg_toplevel;
	wl_signal_add(&server.xdg_shell->events.new_toplevel, &server.new_xdg_toplevel);
	server.new_xdg_popup.notify = lumine_handle_new_xdg_popup;
	wl_signal_add(&server.xdg_shell->events.new_popup, &server.new_xdg_popup);

	server.layer_shell = wlr_layer_shell_v1_create(server.display, 5);
	server.new_layer_surface.notify = lumine_handle_new_layer_surface;
	wl_signal_add(&server.layer_shell->events.new_surface,
		&server.new_layer_surface);

	server.foreign_toplevel =
		wlr_foreign_toplevel_manager_v1_create(server.display);

	/* Input. */
	server.cursor = wlr_cursor_create();
	wlr_cursor_attach_output_layout(server.cursor, server.output_layout);
	server.cursor_mgr = wlr_xcursor_manager_create(NULL, 24);
	lumine_seat_init(&server);

	server.new_output.notify = lumine_handle_new_output;
	wl_signal_add(&server.backend->events.new_output, &server.new_output);

	const char *socket = wl_display_add_socket_auto(server.display);
	if (socket == NULL) {
		wlr_log(WLR_ERROR, "failed to add Wayland socket");
		return 1;
	}
	setenv("WAYLAND_DISPLAY", socket, true);

	if (!wlr_backend_start(server.backend)) {
		wlr_log(WLR_ERROR, "failed to start backend");
		return 1;
	}

	wlr_log(WLR_INFO, "running Wayland compositor on WAYLAND_DISPLAY=%s", socket);

	/* Test/demo hook: LUMINE_SPAWN="cmd one;;cmd two" runs after startup. */
	const char *spawn_list = getenv("LUMINE_SPAWN");
	if (spawn_list != NULL && spawn_list[0] != '\0') {
		char *buf = strdup(spawn_list);
		char *saveptr = NULL;
		for (char *cmd = strtok_r(buf, ";;", &saveptr); cmd != NULL;
				cmd = strtok_r(NULL, ";;", &saveptr)) {
			if (cmd[0] != '\0') {
				lumine_spawn(cmd);
			}
		}
		free(buf);
	}

	test_driver_start(&server);

	/* `lumine [command...]` runs a command after startup (like weston). */
	if (argc > 1) {
		size_t cmd_len = 1;
		for (int i = 1; i < argc; i++) {
			cmd_len += strlen(argv[i]) + 1;
		}
		char *cmd = malloc(cmd_len);
		cmd[0] = '\0';
		for (int i = 1; i < argc; i++) {
			strcat(cmd, argv[i]);
			if (i + 1 < argc) {
				strcat(cmd, " ");
			}
		}
		lumine_spawn(cmd);
		free(cmd);
	}

	wl_display_run(server.display);

	/* Tear down in reverse order; wlroots asserts that backend listeners
	 * are gone before the backend is destroyed. */
	wl_list_remove(&server.new_output.link);
	wl_list_remove(&server.new_xdg_toplevel.link);
	wl_list_remove(&server.new_xdg_popup.link);
	wl_list_remove(&server.new_layer_surface.link);
	wl_list_remove(&server.new_input.link);
	wl_list_remove(&server.new_virtual_pointer.link);
	wl_list_remove(&server.new_virtual_keyboard.link);
	wl_list_remove(&server.request_cursor.link);
	wl_list_remove(&server.request_set_selection.link);
	wl_list_remove(&server.pointer_focus_change.link);
	wl_list_remove(&server.cursor_motion.link);
	wl_list_remove(&server.cursor_motion_absolute.link);
	wl_list_remove(&server.cursor_button.link);
	wl_list_remove(&server.cursor_axis.link);
	wl_list_remove(&server.cursor_frame.link);

	wl_display_destroy_clients(server.display);
	wlr_scene_node_destroy(&server.scene->tree.node);
	wlr_allocator_destroy(server.allocator);
	wlr_renderer_destroy(server.renderer);
	wlr_backend_destroy(server.backend);
	wl_display_destroy(server.display);
	return 0;
}
