#ifndef LUMINE_H
#define LUMINE_H

#define WLR_USE_UNSTABLE

#include <stdbool.h>
#include <stdint.h>

#include <wayland-server-core.h>
#include <wayland-util.h>
#include <wlr/backend.h>
#include <wlr/render/allocator.h>
#include <wlr/render/wlr_renderer.h>
#include <wlr/types/wlr_cursor.h>
#include <wlr/types/wlr_data_device.h>
#include <wlr/types/wlr_foreign_toplevel_management_v1.h>
#include <wlr/types/wlr_input_device.h>
#include <wlr/types/wlr_keyboard.h>
#include <wlr/types/wlr_layer_shell_v1.h>
#include <wlr/types/wlr_output.h>
#include <wlr/types/wlr_output_layout.h>
#include <wlr/types/wlr_pointer.h>
#include <wlr/types/wlr_scene.h>
#include <wlr/types/wlr_seat.h>
#include <wlr/types/wlr_xcursor_manager.h>
#include <wlr/types/wlr_xdg_shell.h>
#include <wlr/util/box.h>
#include <xkbcommon/xkbcommon.h>

enum lumine_layout_mode {
	LUMINE_LAYOUT_TILING,
	LUMINE_LAYOUT_STACK,
};

enum lumine_cursor_mode {
	LUMINE_CURSOR_PASSTHROUGH,
	LUMINE_CURSOR_MOVE,
	LUMINE_CURSOR_RESIZE,
};

struct lumine_server {
	struct wl_display *display;
	struct wlr_backend *backend;
	struct wlr_renderer *renderer;
	struct wlr_allocator *allocator;

	struct wlr_scene *scene;
	struct wlr_output_layout *output_layout;
	struct wlr_scene_output_layout *scene_layout;

	/* Z-ordered scene trees (creation order = bottom to top). */
	struct wlr_scene_tree *tree_background;
	struct wlr_scene_tree *tree_bottom;
	struct wlr_scene_tree *tree_tiling;
	struct wlr_scene_tree *tree_floating;
	struct wlr_scene_tree *tree_animation; /* close-animation snapshots */
	struct wlr_scene_tree *tree_top;
	struct wlr_scene_tree *tree_overlay;

	struct wlr_xdg_shell *xdg_shell;
	struct wl_listener new_xdg_toplevel;
	struct wl_listener new_xdg_popup;

	struct wlr_layer_shell_v1 *layer_shell;
	struct wl_listener new_layer_surface;

	struct wlr_foreign_toplevel_manager_v1 *foreign_toplevel;
	struct wlr_virtual_keyboard_manager_v1 *virtual_keyboard_manager;
	struct wlr_virtual_pointer_manager_v1 *virtual_pointer_manager;

	struct wlr_cursor *cursor;
	struct wlr_xcursor_manager *cursor_mgr;
	struct wl_listener cursor_motion;
	struct wl_listener cursor_motion_absolute;
	struct wl_listener cursor_button;
	struct wl_listener cursor_axis;
	struct wl_listener cursor_frame;

	struct wlr_seat *seat;
	struct wl_listener new_input;
	struct wl_listener new_virtual_keyboard;
	struct wl_listener new_virtual_pointer;
	struct wl_listener request_cursor;
	struct wl_listener request_set_selection;
	struct wl_listener pointer_focus_change;
	struct wl_list keyboards;
	size_t pointer_count;

	enum lumine_layout_mode mode;
	double master_ratio;
	struct lumine_toplevel *focused_toplevel;
	struct lumine_layer *focused_layer;

	enum lumine_cursor_mode cursor_mode;
	struct lumine_toplevel *grabbed_toplevel;
	double grab_x, grab_y;
	struct wlr_box grab_geobox;
	uint32_t resize_edges;
	/* Scene node under the pointer when an implicit grab began, for
	 * computing grab-relative motion coordinates. */
	struct wlr_scene_node *grab_node;
	struct wl_listener grab_node_destroy;
	/* Number of pointer buttons currently held; client-initiated
	 * interactive move/resize is only valid while > 0 (clients may send
	 * xdg_toplevel.move again on release, which must not re-grab). */
	unsigned pointer_buttons;

	struct wl_list outputs;   // lumine_output.link
	struct wl_list toplevels; // lumine_toplevel.link
	struct wl_list layers;    // lumine_layer.link
	struct wl_list anims;     // lumine_anim.link (opaque, animation.c)
	struct wl_event_source *anim_timer;

	struct wl_listener new_output;
};

struct lumine_output {
	struct wl_list link;
	struct lumine_server *server;
	struct wlr_output *wlr_output;
	struct wlr_scene_output *scene_output;
	struct wlr_output_layout_output *layout_output;
	/* Output geometry and layer-shrunk usable area, in layout coords. */
	struct wlr_box usable;
	bool hdr_enabled;

	struct wl_listener frame;
	struct wl_listener request_state;
	struct wl_listener destroy;
};

struct lumine_toplevel {
	struct wl_list link;
	struct lumine_server *server;
	struct wlr_xdg_toplevel *xdg_toplevel;
	struct wlr_scene_tree *scene_tree;
	struct lumine_output *output;
	bool floating;
	bool placed;
	/* Current box in layout coords (valid while floating). */
	int x, y, width, height;

	struct wlr_foreign_toplevel_handle_v1 *foreign_handle;
	char *last_foreign_title;
	/* Last committed frame, locked for the close-animation snapshot
	 * (wlroots drops surface->current.buffer right after each commit). */
	struct wlr_buffer *last_buffer;

	struct wl_listener map;
	struct wl_listener unmap;
	struct wl_listener commit;
	struct wl_listener destroy;
	struct wl_listener request_move;
	struct wl_listener request_resize;
	struct wl_listener request_maximize;
	struct wl_listener request_fullscreen;
};

struct lumine_popup {
	struct wlr_xdg_popup *xdg_popup;
	struct wlr_scene_tree *scene_tree;
	struct wl_listener commit;
	struct wl_listener destroy;
};

struct lumine_layer {
	struct wl_list link;
	struct lumine_server *server;
	struct wlr_layer_surface_v1 *layer_surface;
	struct wlr_scene_layer_surface_v1 *scene_layer;
	struct lumine_output *output;
	/* Last state we arranged for, to avoid configure spam. */
	struct wlr_layer_surface_v1_state arranged;
	bool has_arranged;

	struct wl_listener map;
	struct wl_listener unmap;
	struct wl_listener commit;
	struct wl_listener destroy;
};

struct lumine_keyboard {
	struct wl_list link;
	struct lumine_server *server;
	struct wlr_keyboard *wlr_keyboard;

	struct wl_listener modifiers;
	struct wl_listener key;
	struct wl_listener destroy;
};

/* animation.c */
void lumine_anims_init(struct lumine_server *server);
void lumine_anims_cancel(struct lumine_server *server,
	struct lumine_toplevel *toplevel);
void lumine_anim_fade_in(struct lumine_server *server,
	struct lumine_toplevel *toplevel);
void lumine_anim_move_to(struct lumine_server *server,
	struct lumine_toplevel *toplevel, int to_x, int to_y);
/* Fade out and destroy a standalone node (close-animation snapshot). */
void lumine_anim_fade_out(struct lumine_server *server,
	struct wlr_scene_node *node);

/* main.c */
void lumine_spawn(const char *cmd);
struct lumine_output *lumine_focused_output(struct lumine_server *server);

/* output.c */
void lumine_handle_new_output(struct wl_listener *listener, void *data);
void lumine_output_arrange(struct lumine_output *output);
void lumine_output_toggle_hdr(struct lumine_output *output);

/* toplevel.c */
void lumine_handle_new_xdg_toplevel(struct wl_listener *listener, void *data);
void lumine_handle_new_xdg_popup(struct wl_listener *listener, void *data);
void lumine_toplevel_float(struct lumine_toplevel *toplevel);
void lumine_toplevel_close(struct lumine_toplevel *toplevel);

/* layer.c */
void lumine_handle_new_layer_surface(struct wl_listener *listener, void *data);

/* layout.c */
void lumine_arrange_all(struct lumine_server *server);
void lumine_arrange_output(struct lumine_output *output);
void lumine_toggle_mode(struct lumine_server *server);
void lumine_adjust_master_ratio(struct lumine_server *server, double delta);
void lumine_focus_toplevel(struct lumine_toplevel *toplevel);
void lumine_focus_next(struct lumine_server *server, int dir);
void lumine_move_focused(struct lumine_server *server, int dir);
struct lumine_toplevel *lumine_toplevel_at(struct lumine_server *server,
	double lx, double ly, double *sx, double *sy);

/* input.c */
void lumine_seat_init(struct lumine_server *server);
void lumine_begin_move(struct lumine_toplevel *toplevel, uint32_t time);
void lumine_begin_resize(struct lumine_toplevel *toplevel, uint32_t time,
	uint32_t edges);

#endif
