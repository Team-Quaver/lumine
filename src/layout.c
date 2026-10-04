#include <math.h>
#include <stdlib.h>

#include "lumine.h"
#include <wlr/types/wlr_scene.h>
#include <wlr/util/log.h>

#define LUMINE_STACK_STEP 40
#define LUMINE_STACK_MARGIN 60
#define LUMINE_DEFAULT_WIDTH 1000
#define LUMINE_DEFAULT_HEIGHT 700

static void toplevel_apply_box(struct lumine_toplevel *toplevel, int x, int y,
		int width, int height) {
	toplevel->x = x;
	toplevel->y = y;

	/* First placement is instant; later layout changes glide. */
	if (toplevel->placed && !toplevel->floating) {
		lumine_anim_move_to(toplevel->server, toplevel, x, y);
	} else {
		lumine_anims_cancel(toplevel->server, toplevel);
		wlr_scene_node_set_position(&toplevel->scene_tree->node, x, y);
	}
	toplevel->placed = true;

	/* Only re-configure the client when the size actually changes. */
	if (width > 0 && height > 0 &&
			(width != toplevel->width || height != toplevel->height)) {
		toplevel->width = width;
		toplevel->height = height;
		wlr_xdg_toplevel_set_size(toplevel->xdg_toplevel, width, height);
	}
}

static void toplevel_get_size(struct lumine_toplevel *toplevel, int *width,
		int *height) {
	if (toplevel->width > 0 && toplevel->height > 0) {
		*width = toplevel->width;
		*height = toplevel->height;
		return;
	}
	struct wlr_box geom = toplevel->xdg_toplevel->base->geometry;
	*width = geom.width > 0 ? geom.width : LUMINE_DEFAULT_WIDTH;
	*height = geom.height > 0 ? geom.height : LUMINE_DEFAULT_HEIGHT;
}

/* Master-stack tiling: the first window fills the left column, the rest
 * share the right column. */
static void arrange_tiling(struct lumine_output *output) {
	struct lumine_server *server = output->server;
	struct wlr_box *area = &output->usable;

	struct lumine_toplevel *toplevels[256];
	size_t n = 0;
	struct lumine_toplevel *toplevel;
	wl_list_for_each(toplevel, &server->toplevels, link) {
		if (toplevel->output == output && !toplevel->floating &&
				n < sizeof(toplevels) / sizeof(toplevels[0])) {
			toplevels[n++] = toplevel;
		}
	}

	if (n == 0) {
		return;
	}
	wlr_log(WLR_DEBUG, "tiling %zu window(s) on %s", n, output->wlr_output->name);
	if (n == 1) {
		toplevel_apply_box(toplevels[0], area->x, area->y,
			area->width, area->height);
		return;
	}

	int master_width = round(area->width * server->master_ratio);
	int stack_width = area->width - master_width;
	int stack_height = area->height / (n - 1);

	toplevel_apply_box(toplevels[0], area->x, area->y,
		master_width, area->height);
	for (size_t i = 1; i < n; i++) {
		toplevel_apply_box(toplevels[i], area->x + master_width,
			area->y + (int)(i - 1) * stack_height,
			stack_width, stack_height);
	}
}

/* Cascade stacking: overlapping windows, newest on top. */
static void arrange_stack(struct lumine_output *output) {
	struct lumine_server *server = output->server;
	struct wlr_box *area = &output->usable;

	int i = 0;
	struct lumine_toplevel *toplevel;
	wl_list_for_each(toplevel, &server->toplevels, link) {
		if (toplevel->output != output) {
			continue;
		}
		if (toplevel->floating) {
			wlr_scene_node_set_position(&toplevel->scene_tree->node,
				toplevel->x, toplevel->y);
			continue;
		}

		int width, height;
		toplevel_get_size(toplevel, &width, &height);
		if (width > area->width - LUMINE_STACK_MARGIN) {
			width = area->width - LUMINE_STACK_MARGIN;
		}
		if (height > area->height - LUMINE_STACK_MARGIN) {
			height = area->height - LUMINE_STACK_MARGIN;
		}
		int x = area->x + LUMINE_STACK_MARGIN / 2 +
			(i % 8) * LUMINE_STACK_STEP;
		int y = area->y + LUMINE_STACK_MARGIN / 2 +
			(i % 8) * (LUMINE_STACK_STEP / 2);

		toplevel->x = x;
		toplevel->y = y;
		if (toplevel->placed) {
			lumine_anim_move_to(toplevel->server, toplevel, x, y);
		} else {
			wlr_scene_node_set_position(&toplevel->scene_tree->node, x, y);
		}
		toplevel->placed = true;
		if (width != toplevel->width || height != toplevel->height) {
			toplevel->width = width;
			toplevel->height = height;
			wlr_xdg_toplevel_set_size(toplevel->xdg_toplevel, width, height);
		}
		i++;
	}
}

void lumine_arrange_output(struct lumine_output *output) {
	if (output == NULL) {
		return;
	}
	if (output->server->mode == LUMINE_LAYOUT_TILING) {
		arrange_tiling(output);
	} else {
		arrange_stack(output);
	}
}

void lumine_arrange_all(struct lumine_server *server) {
	struct lumine_output *output;
	wl_list_for_each(output, &server->outputs, link) {
		lumine_output_arrange(output);
	}
}

void lumine_toggle_mode(struct lumine_server *server) {
	server->mode = server->mode == LUMINE_LAYOUT_TILING ?
		LUMINE_LAYOUT_STACK : LUMINE_LAYOUT_TILING;
	wlr_log(WLR_INFO, "layout mode: %s",
		server->mode == LUMINE_LAYOUT_TILING ? "tiling" : "stack");

	struct lumine_toplevel *toplevel;
	wl_list_for_each(toplevel, &server->toplevels, link) {
		toplevel->floating = false;
		wlr_scene_node_reparent(&toplevel->scene_tree->node,
			server->mode == LUMINE_LAYOUT_TILING ?
				server->tree_tiling : server->tree_floating);
	}
	lumine_arrange_all(server);
}

void lumine_adjust_master_ratio(struct lumine_server *server, double delta) {
	server->master_ratio = fmin(0.8, fmax(0.2, server->master_ratio + delta));
	if (server->mode == LUMINE_LAYOUT_TILING) {
		lumine_arrange_all(server);
	}
}

void lumine_focus_toplevel(struct lumine_toplevel *toplevel) {
	if (toplevel == NULL) {
		return;
	}
	struct lumine_server *server = toplevel->server;
	struct wlr_seat *seat = server->seat;

	struct wlr_surface *prev_surface =
		seat->keyboard_state.focused_surface;
	if (prev_surface == toplevel->xdg_toplevel->base->surface) {
		return;
	}
	if (server->focused_toplevel != NULL &&
			server->focused_toplevel != toplevel) {
		wlr_xdg_toplevel_set_activated(
			server->focused_toplevel->xdg_toplevel, false);
	}
	if (server->focused_layer != NULL) {
		server->focused_layer = NULL;
	}

	server->focused_toplevel = toplevel;

	/* Move the window to the top of its tree. */
	wlr_scene_node_raise_to_top(&toplevel->scene_tree->node);

	wlr_xdg_toplevel_set_activated(toplevel->xdg_toplevel, true);

	if (toplevel->foreign_handle != NULL) {
		struct lumine_toplevel *other;
		wl_list_for_each(other, &server->toplevels, link) {
			if (other->foreign_handle != NULL) {
				wlr_foreign_toplevel_handle_v1_set_activated(
					other->foreign_handle, other == toplevel);
			}
		}
	}

	struct lumine_keyboard *keyboard = NULL;
	if (!wl_list_empty(&server->keyboards)) {
		keyboard = wl_container_of(server->keyboards.next, keyboard, link);
	}
	wlr_seat_keyboard_notify_enter(seat,
		toplevel->xdg_toplevel->base->surface,
		keyboard != NULL ? keyboard->wlr_keyboard->keycodes : NULL,
		keyboard != NULL ? keyboard->wlr_keyboard->num_keycodes : 0,
		keyboard != NULL ? &keyboard->wlr_keyboard->modifiers : NULL);
}

void lumine_focus_next(struct lumine_server *server, int dir) {
	struct lumine_output *output = lumine_focused_output(server);
	if (output == NULL) {
		return;
	}

	/* First pass: count windows on this output; second pass: pick the
	 * neighbor of the currently focused one (wrapping around). */
	size_t count = 0;
	struct lumine_toplevel *toplevel;
	wl_list_for_each(toplevel, &server->toplevels, link) {
		if (toplevel->output == output) {
			count++;
		}
	}
	if (count == 0) {
		return;
	}

	size_t index = 0, focused_index = 0;
	struct lumine_toplevel *windows[256];
	memset(windows, 0, sizeof(windows));
	wl_list_for_each(toplevel, &server->toplevels, link) {
		if (toplevel->output != output) {
			continue;
		}
		if (count <= sizeof(windows) / sizeof(windows[0])) {
			windows[index] = toplevel;
		}
		if (toplevel == server->focused_toplevel) {
			focused_index = index;
		}
		index++;
	}
	if (count > sizeof(windows) / sizeof(windows[0])) {
		return;
	}

	size_t next = ((int)focused_index + dir + (int)count) % count;
	lumine_focus_toplevel(windows[next]);
}

void lumine_move_focused(struct lumine_server *server, int dir) {
	if (server->focused_toplevel == NULL ||
			server->mode != LUMINE_LAYOUT_TILING ||
			server->focused_toplevel->floating) {
		return;
	}
	struct lumine_toplevel *toplevel = server->focused_toplevel;

	struct wl_list *target = dir > 0 ? toplevel->link.next : toplevel->link.prev;
	if (target == &server->toplevels) {
		return;
	}
	wl_list_remove(&toplevel->link);
	wl_list_insert(target, &toplevel->link);
	lumine_arrange_all(server);
}

struct lumine_toplevel *lumine_toplevel_at(struct lumine_server *server,
		double lx, double ly, double *sx, double *sy) {
	struct wlr_scene_node *node =
		wlr_scene_node_at(&server->scene->tree.node, lx, ly, sx, sy);
	if (node == NULL || node->type != WLR_SCENE_NODE_BUFFER) {
		return NULL;
	}
	struct wlr_scene_buffer *buffer = wlr_scene_buffer_from_node(node);
	struct wlr_scene_surface *surface =
		wlr_scene_surface_try_from_buffer(buffer);
	if (surface == NULL) {
		return NULL;
	}

	struct wlr_scene_tree *tree = node->parent;
	while (tree != NULL && tree->node.data == NULL) {
		tree = tree->node.parent;
	}
	if (tree == NULL) {
		return NULL;
	}
	return tree->node.data;
}
