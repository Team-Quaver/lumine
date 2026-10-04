#include <stdlib.h>

#include "lumine.h"
#include <wlr/util/log.h>

static void layer_handle_map(struct wl_listener *listener, void *data) {
	struct lumine_layer *layer = wl_container_of(listener, layer, map);
	lumine_output_arrange(layer->output);

	/* Exclusive keyboard grabs (launchers, locks) take keyboard focus. */
	if (layer->layer_surface->current.keyboard_interactive ==
			ZWLR_LAYER_SURFACE_V1_KEYBOARD_INTERACTIVITY_EXCLUSIVE) {
		struct lumine_server *server = layer->server;
		server->focused_layer = layer;
		struct lumine_keyboard *keyboard;
		wl_list_for_each(keyboard, &server->keyboards, link) {
			wlr_seat_keyboard_notify_enter(server->seat,
				layer->layer_surface->surface,
				keyboard->wlr_keyboard->keycodes,
				keyboard->wlr_keyboard->num_keycodes,
				&keyboard->wlr_keyboard->modifiers);
			break;
		}
	}
}

static void layer_handle_unmap(struct wl_listener *listener, void *data) {
	struct lumine_layer *layer = wl_container_of(listener, layer, unmap);
	struct lumine_server *server = layer->server;

	if (server->focused_layer == layer) {
		server->focused_layer = NULL;
		lumine_focus_next(server, 1);
	}
	lumine_output_arrange(layer->output);
}

/* Size-relevant layer state: only re-arrange when one of these changes,
 * otherwise every shell repaint would spam configure events. */
static bool layer_state_changed(struct lumine_layer *layer) {
	if (!layer->has_arranged) {
		return true;
	}
	const struct wlr_layer_surface_v1_state *a = &layer->arranged;
	const struct wlr_layer_surface_v1_state *c =
		&layer->layer_surface->current;
	return a->desired_width != c->desired_width ||
		a->desired_height != c->desired_height ||
		a->anchor != c->anchor ||
		a->exclusive_zone != c->exclusive_zone ||
		a->exclusive_edge != c->exclusive_edge ||
		a->margin.top != c->margin.top ||
		a->margin.right != c->margin.right ||
		a->margin.bottom != c->margin.bottom ||
		a->margin.left != c->margin.left ||
		a->layer != c->layer;
}

/* Send a configure to a single layer surface, computing its geometry the
 * same way the full arrange pass does. Used for the initial configure,
 * which must be sent before the surface is mapped. */
static void layer_send_configure(struct lumine_layer *layer) {
	struct wlr_box full = {0};
	wlr_output_layout_get_box(layer->server->output_layout,
		layer->output->wlr_output, &full);
	if (wlr_box_empty(&full)) {
		return;
	}
	struct wlr_box usable = full;
	wlr_scene_layer_surface_v1_configure(layer->scene_layer, &full, &usable);
	layer->arranged = layer->layer_surface->current;
	layer->has_arranged = true;
}

static void layer_handle_commit(struct wl_listener *listener, void *data) {
	struct lumine_layer *layer = wl_container_of(listener, layer, commit);
	struct wlr_layer_surface_v1 *surface = layer->layer_surface;

	if (surface->initial_commit) {
		/* Role processing has set `initialized` by now (role->commit runs
		 * before the commit signal); the client is waiting for the first
		 * configure to attach its buffer. */
		layer_send_configure(layer);
		return;
	}

	/* Keyboard interactivity can change between commits. */
	if (surface->current.keyboard_interactive ==
			ZWLR_LAYER_SURFACE_V1_KEYBOARD_INTERACTIVITY_EXCLUSIVE) {
		if (layer->server->focused_layer != layer) {
			layer_handle_map(&layer->map, NULL);
		}
	}

	if (layer_state_changed(layer)) {
		lumine_output_arrange(layer->output);
	}
}

static void layer_handle_destroy(struct wl_listener *listener, void *data) {
	struct lumine_layer *layer = wl_container_of(listener, layer, destroy);
	struct lumine_server *server = layer->server;

	if (server->focused_layer == layer) {
		server->focused_layer = NULL;
	}
	wl_list_remove(&layer->link);
	wl_list_remove(&layer->map.link);
	wl_list_remove(&layer->unmap.link);
	wl_list_remove(&layer->commit.link);
	wl_list_remove(&layer->destroy.link);
	free(layer);
}

void lumine_handle_new_layer_surface(struct wl_listener *listener, void *data) {
	struct lumine_server *server =
		wl_container_of(listener, server, new_layer_surface);
	struct wlr_layer_surface_v1 *layer_surface = data;

	if (layer_surface->output == NULL) {
		/* Client did not pick an output; use the focused one. */
		struct lumine_output *output = lumine_focused_output(server);
		if (output != NULL) {
			layer_surface->output = output->wlr_output;
		}
	}

	struct lumine_output *output = NULL;
	struct lumine_output *candidate;
	wl_list_for_each(candidate, &server->outputs, link) {
		if (candidate->wlr_output == layer_surface->output) {
			output = candidate;
			break;
		}
	}
	if (output == NULL) {
		wlr_log(WLR_ERROR, "layer surface for unknown output, rejecting");
		wlr_layer_surface_v1_destroy(layer_surface);
		return;
	}

	struct lumine_layer *layer = calloc(1, sizeof(*layer));
	if (layer == NULL) {
		return;
	}
	layer->server = server;
	layer->layer_surface = layer_surface;
	layer->output = output;

	struct wlr_scene_tree *parent;
	switch (layer_surface->current.layer) {
	case ZWLR_LAYER_SHELL_V1_LAYER_BACKGROUND:
		parent = server->tree_background;
		break;
	case ZWLR_LAYER_SHELL_V1_LAYER_BOTTOM:
		parent = server->tree_bottom;
		break;
	case ZWLR_LAYER_SHELL_V1_LAYER_TOP:
		parent = server->tree_top;
		break;
	case ZWLR_LAYER_SHELL_V1_LAYER_OVERLAY:
	default:
		parent = server->tree_overlay;
		break;
	}
	layer->scene_layer = wlr_scene_layer_surface_v1_create(parent,
		layer_surface);

	layer->map.notify = layer_handle_map;
	wl_signal_add(&layer_surface->surface->events.map, &layer->map);
	layer->unmap.notify = layer_handle_unmap;
	wl_signal_add(&layer_surface->surface->events.unmap, &layer->unmap);
	layer->commit.notify = layer_handle_commit;
	wl_signal_add(&layer_surface->surface->events.commit, &layer->commit);
	layer->destroy.notify = layer_handle_destroy;
	wl_signal_add(&layer_surface->events.destroy, &layer->destroy);

	wl_list_insert(&server->layers, &layer->link);
	lumine_output_arrange(output);
}
