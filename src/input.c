#include <linux/input-event-codes.h>
#include <stdlib.h>
#include <string.h>

#include "lumine.h"
#include <wlr/types/wlr_virtual_keyboard_v1.h>
#include <wlr/types/wlr_virtual_pointer_v1.h>
#include <wlr/util/log.h>
#include <xkbcommon/xkbcommon-keysyms.h>

static void keyboard_handle_modifiers(struct wl_listener *listener, void *data) {
	struct lumine_keyboard *keyboard =
		wl_container_of(listener, keyboard, modifiers);
	wlr_seat_set_keyboard(keyboard->server->seat, keyboard->wlr_keyboard);
	wlr_seat_keyboard_notify_modifiers(keyboard->server->seat,
		&keyboard->wlr_keyboard->modifiers);
}

/* Returns whether the keysym was bound; unbound combos must fall through
 * to the client, or holding Super makes every keystroke vanish. */
static bool handle_binding(struct lumine_server *server, uint32_t mods,
		xkb_keysym_t sym) {
	if (sym == XKB_KEY_Return) {
		const char *terminal = getenv("LUMINE_TERMINAL");
		lumine_spawn(terminal != NULL ? terminal : "kitty");
		return true;
	}
	if (sym == XKB_KEY_E && (mods & WLR_MODIFIER_SHIFT)) {
		wlr_log(WLR_INFO, "quitting on user request");
		wl_display_terminate(server->display);
		return true;
	}
	if (sym == XKB_KEY_q) {
		if (server->focused_toplevel != NULL) {
			lumine_toplevel_close(server->focused_toplevel);
		}
		return true;
	}
	if (sym == XKB_KEY_m) {
		lumine_toggle_mode(server);
		return true;
	}
	if (sym == XKB_KEY_j || sym == XKB_KEY_Down) {
		lumine_focus_next(server, 1);
		return true;
	}
	if (sym == XKB_KEY_k || sym == XKB_KEY_Up) {
		lumine_focus_next(server, -1);
		return true;
	}
	if (sym == XKB_KEY_J) {
		lumine_move_focused(server, 1);
		return true;
	}
	if (sym == XKB_KEY_K) {
		lumine_move_focused(server, -1);
		return true;
	}
	if (sym == XKB_KEY_h) {
		lumine_adjust_master_ratio(server, -0.05);
		return true;
	}
	if (sym == XKB_KEY_l) {
		lumine_adjust_master_ratio(server, 0.05);
		return true;
	}
	if (sym == XKB_KEY_x) {
		struct lumine_output *output = lumine_focused_output(server);
		if (output != NULL) {
			lumine_output_toggle_hdr(output);
		}
		return true;
	}
	if (sym == XKB_KEY_f) {
		if (server->focused_toplevel != NULL) {
			struct wlr_xdg_toplevel *t =
				server->focused_toplevel->xdg_toplevel;
			wlr_xdg_toplevel_set_fullscreen(t, !t->requested.fullscreen);
		}
		return true;
	}
	return false;
}

static void keyboard_handle_key(struct wl_listener *listener, void *data) {
	struct lumine_keyboard *keyboard =
		wl_container_of(listener, keyboard, key);
	struct lumine_server *server = keyboard->server;
	struct wlr_keyboard_key_event *event = data;
	struct wlr_seat *seat = server->seat;

	uint32_t keycode = event->keycode + 8;
	const xkb_keysym_t *syms;
	int nsyms = xkb_state_key_get_syms(keyboard->wlr_keyboard->xkb_state,
		keycode, &syms);

	uint32_t mods = wlr_keyboard_get_modifiers(keyboard->wlr_keyboard);
	bool handled = false;
	if ((mods & WLR_MODIFIER_LOGO) &&
			event->state == WL_KEYBOARD_KEY_STATE_PRESSED) {
		for (int i = 0; i < nsyms && !handled; i++) {
			handled = handle_binding(server, mods, syms[i]);
		}
	}

	if (!handled) {
		wlr_seat_keyboard_notify_key(seat, event->time_msec,
			event->keycode, event->state);
	}
}

static void keyboard_handle_destroy(struct wl_listener *listener, void *data) {
	struct lumine_keyboard *keyboard =
		wl_container_of(listener, keyboard, destroy);
	wl_list_remove(&keyboard->link);
	wl_list_remove(&keyboard->modifiers.link);
	wl_list_remove(&keyboard->key.link);
	wl_list_remove(&keyboard->destroy.link);
	free(keyboard);
}

static void keyboard_create(struct lumine_server *server,
		struct wlr_input_device *device) {
	struct wlr_keyboard *wlr_keyboard = wlr_keyboard_from_input_device(device);

	struct lumine_keyboard *keyboard = calloc(1, sizeof(*keyboard));
	if (keyboard == NULL) {
		return;
	}
	keyboard->server = server;
	keyboard->wlr_keyboard = wlr_keyboard;

	struct xkb_rule_names rules = {0};
	struct xkb_context *context = xkb_context_new(XKB_CONTEXT_NO_FLAGS);
	struct xkb_keymap *keymap = xkb_keymap_new_from_names(context, &rules,
		XKB_KEYMAP_COMPILE_NO_FLAGS);
	if (keymap != NULL) {
		wlr_keyboard_set_keymap(wlr_keyboard, keymap);
		xkb_keymap_unref(keymap);
	}
	xkb_context_unref(context);
	wlr_keyboard_set_repeat_info(wlr_keyboard, 25, 600);

	keyboard->modifiers.notify = keyboard_handle_modifiers;
	wl_signal_add(&wlr_keyboard->events.modifiers, &keyboard->modifiers);
	keyboard->key.notify = keyboard_handle_key;
	wl_signal_add(&wlr_keyboard->events.key, &keyboard->key);
	keyboard->destroy.notify = keyboard_handle_destroy;
	wl_signal_add(&device->events.destroy, &keyboard->destroy);

	wl_list_insert(&server->keyboards, &keyboard->link);
	wlr_seat_set_keyboard(server->seat, wlr_keyboard);
}

/*
 * Virtual input devices (wtype-style clients) do not surface through the
 * backend's new_input; the managers expose dedicated signals and the
 * compositor attaches the devices to the cursor/seat itself.
 */
static void update_seat_caps(struct lumine_server *server) {
	uint32_t caps = 0;
	if (!wl_list_empty(&server->keyboards)) {
		caps |= WL_SEAT_CAPABILITY_KEYBOARD;
	}
	if (server->pointer_count > 0) {
		caps |= WL_SEAT_CAPABILITY_POINTER;
	}
	wlr_seat_set_capabilities(server->seat, caps);
}

static void seat_handle_new_virtual_pointer(struct wl_listener *listener,
		void *data) {
	struct lumine_server *server =
		wl_container_of(listener, server, new_virtual_pointer);
	struct wlr_virtual_pointer_v1_new_pointer_event *event = data;
	wlr_log(WLR_INFO, "virtual pointer attached");
	wlr_cursor_attach_input_device(server->cursor,
		&event->new_pointer->pointer.base);
	server->pointer_count++;
	update_seat_caps(server);
}

static void seat_handle_new_virtual_keyboard(struct wl_listener *listener,
		void *data) {
	struct lumine_server *server =
		wl_container_of(listener, server, new_virtual_keyboard);
	struct wlr_virtual_keyboard_v1 *virtual_keyboard = data;
	keyboard_create(server, &virtual_keyboard->keyboard.base);
}

static void seat_handle_new_input(struct wl_listener *listener, void *data) {
	struct lumine_server *server =
		wl_container_of(listener, server, new_input);
	struct wlr_input_device *device = data;

	switch (device->type) {
	case WLR_INPUT_DEVICE_KEYBOARD:
		keyboard_create(server, device);
		break;
	case WLR_INPUT_DEVICE_POINTER:
		wlr_cursor_attach_input_device(server->cursor, device);
		server->pointer_count++;
		break;
	default:
		break;
	}

	update_seat_caps(server);
}

static void seat_handle_request_cursor(struct wl_listener *listener, void *data) {
	struct lumine_server *server =
		wl_container_of(listener, server, request_cursor);
	struct wlr_seat_pointer_request_set_cursor_event *event = data;
	struct wlr_seat_client *focused_client =
		server->seat->pointer_state.focused_client;

	if (focused_client == event->seat_client &&
			event->surface != NULL) {
		wlr_cursor_set_surface(server->cursor, event->surface,
			event->hotspot_x, event->hotspot_y);
	}
}

static void seat_handle_request_set_selection(struct wl_listener *listener,
		void *data) {
	struct lumine_server *server =
		wl_container_of(listener, server, request_set_selection);
	struct wlr_seat_request_set_selection_event *event = data;
	wlr_seat_set_selection(server->seat, event->source,
		event->serial);
}

static void handle_grab_node_destroy(struct wl_listener *listener, void *data) {
	struct lumine_server *server =
		wl_container_of(listener, server, grab_node_destroy);
	/* Signals do not detach their listeners; remove ourselves since the
	 * node is going away. */
	wl_list_remove(&listener->link);
	server->grab_node = NULL;
}

static void grab_node_clear(struct lumine_server *server) {
	if (server->grab_node != NULL) {
		wl_list_remove(&server->grab_node_destroy.link);
		server->grab_node = NULL;
	}
}

static void grab_node_set(struct lumine_server *server,
		struct wlr_scene_node *node) {
	grab_node_clear(server);
	if (node != NULL) {
		server->grab_node = node;
		server->grab_node_destroy.notify = handle_grab_node_destroy;
		wl_signal_add(&node->events.destroy, &server->grab_node_destroy);
	}
}

static void reset_cursor_mode(struct lumine_server *server) {
	server->cursor_mode = LUMINE_CURSOR_PASSTHROUGH;
	server->grabbed_toplevel = NULL;
	grab_node_clear(server);
	wlr_cursor_set_xcursor(server->cursor, server->cursor_mgr, "default");
}

void lumine_cursor_reset(struct lumine_server *server) {
	reset_cursor_mode(server);
}

void lumine_begin_move(struct lumine_toplevel *toplevel, uint32_t time) {
	struct lumine_server *server = toplevel->server;
	if (server->cursor_mode != LUMINE_CURSOR_PASSTHROUGH ||
			server->pointer_buttons == 0) {
		return;
	}

	lumine_toplevel_float(toplevel);
	lumine_anims_cancel(server, toplevel);
	server->cursor_mode = LUMINE_CURSOR_MOVE;
	server->grabbed_toplevel = toplevel;
	server->grab_x = server->cursor->x - toplevel->x;
	server->grab_y = server->cursor->y - toplevel->y;
	wlr_cursor_set_xcursor(server->cursor, server->cursor_mgr, "move");
}

void lumine_begin_resize(struct lumine_toplevel *toplevel, uint32_t time,
		uint32_t edges) {
	struct lumine_server *server = toplevel->server;
	if (server->cursor_mode != LUMINE_CURSOR_PASSTHROUGH ||
			!toplevel->floating || server->pointer_buttons == 0) {
		return;
	}

	server->cursor_mode = LUMINE_CURSOR_RESIZE;
	lumine_anims_cancel(server, toplevel);
	server->grabbed_toplevel = toplevel;
	server->grab_geobox = (struct wlr_box){
		.x = toplevel->x,
		.y = toplevel->y,
		.width = toplevel->width,
		.height = toplevel->height,
	};
	server->resize_edges = edges == 0 ?
		(server->cursor->x > toplevel->x + toplevel->width / 2 ?
			WLR_EDGE_RIGHT : WLR_EDGE_LEFT) |
		(server->cursor->y > toplevel->y + toplevel->height / 2 ?
			WLR_EDGE_BOTTOM : WLR_EDGE_TOP) :
		edges;
	wlr_cursor_set_xcursor(server->cursor, server->cursor_mgr,
		"nwse-resize");
}

static void process_cursor_move(struct lumine_server *server) {
	struct lumine_toplevel *toplevel = server->grabbed_toplevel;
	toplevel->x = server->cursor->x - server->grab_x;
	toplevel->y = server->cursor->y - server->grab_y;
	wlr_scene_node_set_position(&toplevel->scene_tree->node,
		toplevel->x, toplevel->y);
}

static void process_cursor_resize(struct lumine_server *server) {
	struct lumine_toplevel *toplevel = server->grabbed_toplevel;
	uint32_t edges = server->resize_edges;
	struct wlr_box *geobox = &server->grab_geobox;

	double border_x = server->cursor->x - geobox->x;
	double border_y = server->cursor->y - geobox->y;
	int width = geobox->width, height = geobox->height;
	int x = geobox->x, y = geobox->y;
	if (edges & WLR_EDGE_LEFT) {
		x = geobox->x + (int)border_x;
		width = geobox->width - (int)border_x;
	} else if (edges & WLR_EDGE_RIGHT) {
		width = geobox->width + (int)border_x;
	}
	if (edges & WLR_EDGE_TOP) {
		y = geobox->y + (int)border_y;
		height = geobox->height - (int)border_y;
	} else if (edges & WLR_EDGE_BOTTOM) {
		height = geobox->height + (int)border_y;
	}
	if (width < 50 || height < 50) {
		return;
	}

	toplevel->x = x;
	toplevel->y = y;
	wlr_scene_node_set_position(&toplevel->scene_tree->node, x, y);
	wlr_xdg_toplevel_set_size(toplevel->xdg_toplevel, width, height);
}

static void process_cursor_motion(struct lumine_server *server,
		uint32_t time) {
	if (server->cursor_mode == LUMINE_CURSOR_MOVE) {
		process_cursor_move(server);
		return;
	}
	if (server->cursor_mode == LUMINE_CURSOR_RESIZE) {
		process_cursor_resize(server);
		return;
	}

	struct wlr_seat *seat = server->seat;
	bool grabbing = seat->pointer_state.button_count > 0;
	struct wlr_surface *grabbed = seat->pointer_state.focused_surface;

	double sx, sy;
	struct wlr_scene_node *node = wlr_scene_node_at(
		&server->scene->tree.node, server->cursor->x, server->cursor->y,
		&sx, &sy);
	struct wlr_scene_buffer *buffer = node != NULL &&
		node->type == WLR_SCENE_NODE_BUFFER ?
		wlr_scene_buffer_from_node(node) : NULL;
	struct wlr_scene_surface *scene_surface = buffer != NULL ?
		wlr_scene_surface_try_from_buffer(buffer) : NULL;
	struct wlr_surface *surface =
		scene_surface != NULL ? scene_surface->surface : NULL;

	if (grabbing) {
		/* Implicit grab: focus stays pinned to the pressed surface even
		 * when the cursor wanders onto other surfaces (CSD title bars are
		 * separate sub-surfaces; re-entering would break client grabs). */
		int nx = 0, ny = 0;
		if (server->grab_node != NULL &&
				wlr_scene_node_coords(server->grab_node, &nx, &ny)) {
			wlr_seat_pointer_notify_motion(seat, time,
				server->cursor->x - nx, server->cursor->y - ny);
			return;
		}
		/* The pressed surface is gone (window closed under the held
		 * button): fall through to hover handling so the pointer keeps
		 * working; entering another surface resets the seat's
		 * pressed-button state. */
	}

	if (surface == NULL) {
		wlr_cursor_set_xcursor(server->cursor, server->cursor_mgr, "default");
		wlr_seat_pointer_clear_focus(server->seat);
		return;
	}

	wlr_cursor_set_xcursor(server->cursor, server->cursor_mgr, "default");
	if (grabbed != surface) {
		wlr_seat_pointer_notify_enter(server->seat, surface, sx, sy);
	}
	wlr_seat_pointer_notify_motion(server->seat, time, sx, sy);
}

static void cursor_handle_motion(struct wl_listener *listener, void *data) {
	struct lumine_server *server =
		wl_container_of(listener, server, cursor_motion);
	struct wlr_pointer_motion_event *event = data;
	wlr_cursor_move(server->cursor, &event->pointer->base,
		event->delta_x, event->delta_y);
	process_cursor_motion(server, event->time_msec);
}

static void cursor_handle_motion_absolute(struct wl_listener *listener,
		void *data) {
	struct lumine_server *server =
		wl_container_of(listener, server, cursor_motion_absolute);
	struct wlr_pointer_motion_absolute_event *event = data;
	wlr_cursor_warp_absolute(server->cursor, &event->pointer->base,
		event->x, event->y);
	process_cursor_motion(server, event->time_msec);
}

static void cursor_handle_button(struct wl_listener *listener, void *data) {
	struct lumine_server *server =
		wl_container_of(listener, server, cursor_button);
	struct wlr_pointer_button_event *event = data;

	if (event->state == WL_POINTER_BUTTON_STATE_PRESSED) {
		server->pointer_buttons++;
	} else if (server->pointer_buttons > 0) {
		server->pointer_buttons--;
		if (server->pointer_buttons == 0) {
			grab_node_clear(server);
		}
	}

	if (event->state == WL_POINTER_BUTTON_STATE_RELEASED) {
		if (server->cursor_mode != LUMINE_CURSOR_PASSTHROUGH) {
			reset_cursor_mode(server);
		}
		/* Always deliver the release. A compositor-initiated drag (Logo+
		 * button) swallowed the press, so wlroots drops this unmatched
		 * release — but a client-initiated drag (CSD title bar) did get
		 * the press, and skipping its release leaves the button pressed
		 * in seat state forever: the implicit grab never ends and the
		 * pointer freezes for every client. */
		wlr_seat_pointer_notify_button(server->seat, event->time_msec,
			event->button, event->state);
		return;
	}

	if (server->cursor_mode != LUMINE_CURSOR_PASSTHROUGH) {
		/* An interactive move/resize owns the pointer; a second button
		 * pressed during the drag must not reach clients, or it too
		 * would stay pressed in the client's view. */
		return;
	}

	double sx, sy;
	struct lumine_toplevel *toplevel = lumine_toplevel_at(server,
		server->cursor->x, server->cursor->y, &sx, &sy);

	/* Remember the surface node under the pointer for grab-relative
	 * motion coordinates during the implicit grab. */
	struct wlr_scene_node *node = wlr_scene_node_at(
		&server->scene->tree.node, server->cursor->x, server->cursor->y,
		NULL, NULL);
	grab_node_set(server, node != NULL &&
		node->type == WLR_SCENE_NODE_BUFFER ? node : NULL);

	uint32_t mods = 0;
	struct lumine_keyboard *keyboard;
	wl_list_for_each(keyboard, &server->keyboards, link) {
		mods = wlr_keyboard_get_modifiers(keyboard->wlr_keyboard);
		break;
	}

	if (toplevel != NULL) {
		lumine_focus_toplevel(toplevel);
	}

	if ((mods & WLR_MODIFIER_LOGO) && toplevel != NULL) {
		if (event->button == BTN_LEFT) {
			lumine_begin_move(toplevel, event->time_msec);
			return;
		}
		if (event->button == BTN_RIGHT) {
			lumine_begin_resize(toplevel, event->time_msec, 0);
			return;
		}
	}

	wlr_seat_pointer_notify_button(server->seat, event->time_msec,
		event->button, event->state);
}

static void cursor_handle_axis(struct wl_listener *listener, void *data) {
	struct lumine_server *server =
		wl_container_of(listener, server, cursor_axis);
	struct wlr_pointer_axis_event *event = data;
	wlr_seat_pointer_notify_axis(server->seat, event->time_msec,
		event->orientation, event->delta, event->delta_discrete,
		event->source, event->relative_direction);
}

static void cursor_handle_frame(struct wl_listener *listener, void *data) {
	struct lumine_server *server =
		wl_container_of(listener, server, cursor_frame);
	wlr_seat_pointer_notify_frame(server->seat);
}

static void seat_handle_focus_change(struct wl_listener *listener, void *data) {
	struct lumine_server *server =
		wl_container_of(listener, server, pointer_focus_change);
	if (server->seat->keyboard_state.focused_surface == NULL &&
			server->cursor_mode == LUMINE_CURSOR_PASSTHROUGH) {
		wlr_cursor_set_xcursor(server->cursor, server->cursor_mgr, "default");
	}
}

void lumine_seat_init(struct lumine_server *server) {
	server->seat = wlr_seat_create(server->display, "seat0");

	server->new_input.notify = seat_handle_new_input;
	wl_signal_add(&server->backend->events.new_input, &server->new_input);

	if (server->virtual_pointer_manager != NULL) {
		server->new_virtual_pointer.notify = seat_handle_new_virtual_pointer;
		wl_signal_add(&server->virtual_pointer_manager->events.new_virtual_pointer,
			&server->new_virtual_pointer);
	}
	if (server->virtual_keyboard_manager != NULL) {
		server->new_virtual_keyboard.notify = seat_handle_new_virtual_keyboard;
		wl_signal_add(&server->virtual_keyboard_manager->events.new_virtual_keyboard,
			&server->new_virtual_keyboard);
	}

	server->request_cursor.notify = seat_handle_request_cursor;
	wl_signal_add(&server->seat->events.request_set_cursor,
		&server->request_cursor);
	server->request_set_selection.notify = seat_handle_request_set_selection;
	wl_signal_add(&server->seat->events.request_set_selection,
		&server->request_set_selection);

	server->pointer_focus_change.notify = seat_handle_focus_change;
	wl_signal_add(&server->seat->keyboard_state.events.focus_change,
		&server->pointer_focus_change);

	server->cursor_motion.notify = cursor_handle_motion;
	wl_signal_add(&server->cursor->events.motion, &server->cursor_motion);
	server->cursor_motion_absolute.notify = cursor_handle_motion_absolute;
	wl_signal_add(&server->cursor->events.motion_absolute,
		&server->cursor_motion_absolute);
	server->cursor_button.notify = cursor_handle_button;
	wl_signal_add(&server->cursor->events.button, &server->cursor_button);
	server->cursor_axis.notify = cursor_handle_axis;
	wl_signal_add(&server->cursor->events.axis, &server->cursor_axis);
	server->cursor_frame.notify = cursor_handle_frame;
	wl_signal_add(&server->cursor->events.frame, &server->cursor_frame);
}
