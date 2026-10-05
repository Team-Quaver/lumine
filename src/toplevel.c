#include <stdlib.h>
#include <string.h>

#include "lumine.h"
#include <wlr/types/wlr_xdg_activation_v1.h>
#include <wlr/util/log.h>

void lumine_toplevel_close(struct lumine_toplevel *toplevel) {
	wlr_xdg_toplevel_send_close(toplevel->xdg_toplevel);
}

void lumine_toplevel_float(struct lumine_toplevel *toplevel) {
	if (toplevel->floating) {
		return;
	}
	toplevel->floating = true;

	int lx = toplevel->x, ly = toplevel->y;
	wlr_scene_node_reparent(&toplevel->scene_tree->node,
		toplevel->server->tree_floating);
	wlr_scene_node_set_position(&toplevel->scene_tree->node, lx, ly);

	struct wlr_box geom = toplevel->xdg_toplevel->base->geometry;
	toplevel->width = geom.width > 0 ? geom.width : 1000;
	toplevel->height = geom.height > 0 ? geom.height : 700;
}

static void foreign_handle_sync(struct lumine_toplevel *toplevel) {
	if (toplevel->foreign_handle == NULL) {
		return;
	}
	struct wlr_xdg_toplevel *t = toplevel->xdg_toplevel;
	if (t->title != NULL) {
		wlr_foreign_toplevel_handle_v1_set_title(toplevel->foreign_handle,
			t->title);
		free(toplevel->last_foreign_title);
		toplevel->last_foreign_title = strdup(t->title);
	}
	if (t->app_id != NULL) {
		wlr_foreign_toplevel_handle_v1_set_app_id(toplevel->foreign_handle,
			t->app_id);
	}
}

static void toplevel_handle_map(struct wl_listener *listener, void *data) {
	struct lumine_toplevel *toplevel =
		wl_container_of(listener, toplevel, map);
	struct lumine_server *server = toplevel->server;

	toplevel->output = lumine_focused_output(server);
	wl_list_insert(&server->toplevels, &toplevel->link);
	wlr_log(WLR_INFO, "toplevel mapped: %s on %s",
		toplevel->xdg_toplevel->app_id ? toplevel->xdg_toplevel->app_id : "?",
		toplevel->output != NULL ? toplevel->output->wlr_output->name : "none");

	if (server->foreign_toplevel != NULL) {
		toplevel->foreign_handle = wlr_foreign_toplevel_handle_v1_create(
			server->foreign_toplevel);
		foreign_handle_sync(toplevel);
	}

	if (server->mode == LUMINE_LAYOUT_STACK) {
		wlr_scene_node_reparent(&toplevel->scene_tree->node,
			server->tree_floating);
		lumine_arrange_output(toplevel->output);
		toplevel->floating = true;
	}

	lumine_arrange_output(toplevel->output);
	lumine_focus_toplevel(toplevel);
	lumine_anim_fade_in(server, toplevel);
}

static void toplevel_handle_unmap(struct wl_listener *listener, void *data) {
	struct lumine_toplevel *toplevel =
		wl_container_of(listener, toplevel, unmap);
	struct lumine_server *server = toplevel->server;

	lumine_anims_cancel(server, toplevel);

	/* Close animation: snapshot the last frame and fade it out in place;
	 * the surface content is gone after unmap, only the snapshot remains
	 * to be animated. */
	struct wlr_surface *surface = toplevel->xdg_toplevel->base->surface;
	if (toplevel->last_buffer != NULL && server->tree_animation != NULL) {
		struct wlr_scene_buffer *snap = wlr_scene_buffer_create(
			server->tree_animation, toplevel->last_buffer);
		if (snap != NULL) {
			wlr_scene_buffer_set_dest_size(snap,
				surface->current.width, surface->current.height);
			wlr_scene_node_set_position(&snap->node,
				toplevel->x + toplevel->xdg_toplevel->base->geometry.x,
				toplevel->y + toplevel->xdg_toplevel->base->geometry.y);
			lumine_anim_fade_out(server, &snap->node);
		}
	}
	if (toplevel->last_buffer != NULL) {
		wlr_buffer_unlock(toplevel->last_buffer);
		toplevel->last_buffer = NULL;
	}

	if (server->grabbed_toplevel == toplevel) {
		/* The dragged window went away mid-drag: also restore the
		 * xcursor and grab node, not just the mode fields. */
		lumine_cursor_reset(server);
	}
	if (server->focused_toplevel == toplevel) {
		server->focused_toplevel = NULL;
	}
	if (toplevel->foreign_handle != NULL) {
		wlr_foreign_toplevel_handle_v1_destroy(toplevel->foreign_handle);
		toplevel->foreign_handle = NULL;
	}

	/* Re-focus the next window on the same output. */
	struct lumine_output *output = toplevel->output;
	wl_list_remove(&toplevel->link);
	if (output != NULL) {
		struct lumine_toplevel *next = NULL, *tl;
		wl_list_for_each(tl, &server->toplevels, link) {
			if (tl->output == output) {
				next = tl;
				break;
			}
		}
		lumine_arrange_output(output);
		if (next != NULL && server->focused_layer == NULL) {
			lumine_focus_toplevel(next);
		}
	}
}

static void toplevel_handle_commit(struct wl_listener *listener, void *data) {
	struct lumine_toplevel *toplevel =
		wl_container_of(listener, toplevel, commit);
	struct wlr_xdg_surface *base = toplevel->xdg_toplevel->base;

	if (base->initial_commit) {
		/* Let the client pick its natural size; the layout decides. */
		wlr_xdg_toplevel_set_size(toplevel->xdg_toplevel, 0, 0);
		return;
	}

	/* Keep the newest frame around for the close animation: the surface
	 * drops current.buffer right after the commit signal, so lock ours
	 * here while it is still valid. */
	struct wlr_buffer *buffer = base->surface->current.buffer;
	if (buffer != NULL) {
		wlr_buffer_lock(buffer);
		if (toplevel->last_buffer != NULL) {
			wlr_buffer_unlock(toplevel->last_buffer);
		}
		toplevel->last_buffer = buffer;
	}

	if (toplevel->foreign_handle != NULL &&
			toplevel->xdg_toplevel->title != NULL &&
			(toplevel->last_foreign_title == NULL ||
				strcmp(toplevel->last_foreign_title,
					toplevel->xdg_toplevel->title) != 0)) {
		wlr_foreign_toplevel_handle_v1_set_title(toplevel->foreign_handle,
			toplevel->xdg_toplevel->title);
		free(toplevel->last_foreign_title);
		toplevel->last_foreign_title =
			strdup(toplevel->xdg_toplevel->title);
	}
}

static void toplevel_handle_destroy(struct wl_listener *listener, void *data) {
	struct lumine_toplevel *toplevel =
		wl_container_of(listener, toplevel, destroy);

	lumine_anims_cancel(toplevel->server, toplevel);
	if (toplevel->last_buffer != NULL) {
		wlr_buffer_unlock(toplevel->last_buffer);
		toplevel->last_buffer = NULL;
	}
	free(toplevel->last_foreign_title);
	wl_list_remove(&toplevel->map.link);
	wl_list_remove(&toplevel->unmap.link);
	wl_list_remove(&toplevel->commit.link);
	wl_list_remove(&toplevel->destroy.link);
	wl_list_remove(&toplevel->request_move.link);
	wl_list_remove(&toplevel->request_resize.link);
	wl_list_remove(&toplevel->request_maximize.link);
	wl_list_remove(&toplevel->request_fullscreen.link);
	free(toplevel);
}

static void toplevel_handle_request_move(struct wl_listener *listener,
		void *data) {
	struct lumine_toplevel *toplevel =
		wl_container_of(listener, toplevel, request_move);
	if (toplevel->server->pointer_buttons == 0) {
		wlr_log(WLR_DEBUG, "ignoring request_move: no button pressed");
		return;
	}
	lumine_begin_move(toplevel, 0);
}

static void toplevel_handle_request_resize(struct wl_listener *listener,
		void *data) {
	struct lumine_toplevel *toplevel =
		wl_container_of(listener, toplevel, request_resize);
	struct wlr_xdg_toplevel_resize_event *event = data;
	lumine_begin_resize(toplevel, 0, event->edges);
}

static void toplevel_handle_request_maximize(struct wl_listener *listener,
		void *data) {
	struct lumine_toplevel *toplevel =
		wl_container_of(listener, toplevel, request_maximize);
	if (toplevel->xdg_toplevel->base->initialized) {
		wlr_xdg_surface_schedule_configure(toplevel->xdg_toplevel->base);
	}
}

static void toplevel_handle_request_fullscreen(struct wl_listener *listener,
		void *data) {
	struct lumine_toplevel *toplevel =
		wl_container_of(listener, toplevel, request_fullscreen);
	if (toplevel->xdg_toplevel->base->initialized) {
		wlr_xdg_surface_schedule_configure(toplevel->xdg_toplevel->base);
	}
}

void lumine_handle_new_xdg_toplevel(struct wl_listener *listener, void *data) {
	struct lumine_server *server =
		wl_container_of(listener, server, new_xdg_toplevel);
	struct wlr_xdg_toplevel *xdg_toplevel = data;

	struct lumine_toplevel *toplevel = calloc(1, sizeof(*toplevel));
	if (toplevel == NULL) {
		return;
	}
	toplevel->server = server;
	toplevel->xdg_toplevel = xdg_toplevel;
	toplevel->scene_tree = wlr_scene_xdg_surface_create(
		server->tree_tiling, xdg_toplevel->base);
	toplevel->scene_tree->node.data = toplevel;
	xdg_toplevel->base->data = toplevel->scene_tree;

	toplevel->map.notify = toplevel_handle_map;
	wl_signal_add(&xdg_toplevel->base->surface->events.map, &toplevel->map);
	toplevel->unmap.notify = toplevel_handle_unmap;
	wl_signal_add(&xdg_toplevel->base->surface->events.unmap, &toplevel->unmap);
	toplevel->commit.notify = toplevel_handle_commit;
	wl_signal_add(&xdg_toplevel->base->surface->events.commit, &toplevel->commit);

	toplevel->destroy.notify = toplevel_handle_destroy;
	wl_signal_add(&xdg_toplevel->events.destroy, &toplevel->destroy);
	toplevel->request_move.notify = toplevel_handle_request_move;
	wl_signal_add(&xdg_toplevel->events.request_move, &toplevel->request_move);
	toplevel->request_resize.notify = toplevel_handle_request_resize;
	wl_signal_add(&xdg_toplevel->events.request_resize, &toplevel->request_resize);
	toplevel->request_maximize.notify = toplevel_handle_request_maximize;
	wl_signal_add(&xdg_toplevel->events.request_maximize, &toplevel->request_maximize);
	toplevel->request_fullscreen.notify = toplevel_handle_request_fullscreen;
	wl_signal_add(&xdg_toplevel->events.request_fullscreen,
		&toplevel->request_fullscreen);
}

static void popup_handle_commit(struct wl_listener *listener, void *data) {
	struct lumine_popup *popup = wl_container_of(listener, popup, commit);
	if (popup->xdg_popup->base->initial_commit) {
		wlr_xdg_surface_schedule_configure(popup->xdg_popup->base);
	}
}

static void popup_handle_destroy(struct wl_listener *listener, void *data) {
	struct lumine_popup *popup = wl_container_of(listener, popup, destroy);
	wl_list_remove(&popup->commit.link);
	wl_list_remove(&popup->destroy.link);
	free(popup);
}

/*
 * Popups can be parented to an xdg toplevel, another popup, or a layer
 * surface (menus of shell panels like Noctalia). Find the right scene tree
 * to parent them to.
 */
void lumine_handle_new_xdg_popup(struct wl_listener *listener, void *data) {
	struct lumine_server *server =
		wl_container_of(listener, server, new_xdg_popup);
	struct wlr_xdg_popup *xdg_popup = data;

	if (xdg_popup->parent == NULL) {
		/* Layer-shell popups are created parentless first and get attached
		 * to their layer surface by a subsequent
		 * zwlr_layer_surface_v1.get_popup request, which re-emits
		 * new_popup with the parent set. Keep the popup alive until then. */
		wlr_log(WLR_DEBUG, "popup created parentless (layer-shell popup)");
		return;
	}

	struct wlr_scene_tree *parent_tree = NULL;
	struct wlr_xdg_surface *parent_xdg =
		wlr_xdg_surface_try_from_wlr_surface(xdg_popup->parent);
	if (parent_xdg != NULL) {
		parent_tree = parent_xdg->data;
	} else {
		struct lumine_layer *layer;
		wl_list_for_each(layer, &server->layers, link) {
			if (layer->layer_surface->surface == xdg_popup->parent) {
				parent_tree = layer->scene_layer->tree;
				break;
			}
		}
	}
	if (parent_tree == NULL) {
		/* Keep the popup alive (destroying it would make the client's
		 * follow-up requests reference a dead object); it just won't be
		 * rendered. */
		wlr_log(WLR_ERROR, "popup with unknown parent, not rendering it");
		return;
	}

	struct lumine_popup *popup = calloc(1, sizeof(*popup));
	if (popup == NULL) {
		return;
	}
	popup->xdg_popup = xdg_popup;
	popup->scene_tree = wlr_scene_xdg_surface_create(parent_tree,
		xdg_popup->base);
	xdg_popup->base->data = popup->scene_tree;

	popup->commit.notify = popup_handle_commit;
	wl_signal_add(&xdg_popup->base->surface->events.commit, &popup->commit);
	popup->destroy.notify = popup_handle_destroy;
	wl_signal_add(&xdg_popup->base->events.destroy, &popup->destroy);
}
