/*
 * Window animations: fade+lift on open, eased glide for layout changes,
 * and fade-out of a snapshot on close — driven by the display event loop
 * timer.
 *
 * wlroots' scene graph has no per-node opacity in this version, so fades
 * walk the node subtree and set opacity on every scene buffer (fresh walk
 * per tick, so clients adding/removing buffers mid-fade are safe).
 */
#include <math.h>
#include <stdlib.h>
#include <time.h>

#include "lumine.h"
#include <wlr/util/log.h>

#define LUMINE_ANIM_TICK_MS 16
#define LUMINE_ANIM_OPEN_MS 220
#define LUMINE_ANIM_CLOSE_MS 200
#define LUMINE_ANIM_MOVE_MS 170
/* Vertical distance a window lifts while fading in. */
#define LUMINE_ANIM_OPEN_LIFT 14

enum lumine_anim_type {
	LUMINE_ANIM_FADE_IN,
	LUMINE_ANIM_MOVE,
	LUMINE_ANIM_FADE_OUT, /* node is owned by the animation */
};

struct lumine_anim {
	struct wl_list link;
	struct lumine_server *server;
	struct lumine_toplevel *toplevel; /* NULL for standalone nodes */
	struct wlr_scene_node *node;
	enum lumine_anim_type type;
	int64_t start;
	int duration;

	/* LUMINE_ANIM_FADE_IN / LUMINE_ANIM_MOVE target position. */
	int to_x, to_y;
	/* LUMINE_ANIM_MOVE origin. */
	int from_x, from_y;
};

static int64_t now_msec(void) {
	struct timespec ts = {0};
	clock_gettime(CLOCK_MONOTONIC, &ts);
	return (int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
}

static double ease_out_cubic(double t) {
	return 1.0 - pow(1.0 - t, 3.0);
}

static void anim_start(struct lumine_server *server) {
	wl_event_source_timer_update(server->anim_timer, LUMINE_ANIM_TICK_MS);
}

/* Set opacity on every scene buffer under the node. */
static void tree_set_opacity(struct wlr_scene_node *node, float opacity) {
	switch (node->type) {
	case WLR_SCENE_NODE_TREE: {
		struct wlr_scene_tree *tree = wlr_scene_tree_from_node(node);
		struct wlr_scene_node *child;
		wl_list_for_each(child, &tree->children, link) {
			tree_set_opacity(child, opacity);
		}
		break;
	}
	case WLR_SCENE_NODE_BUFFER:
		wlr_scene_buffer_set_opacity(wlr_scene_buffer_from_node(node),
			opacity);
		break;
	default:
		break;
	}
}

static void anim_apply_end_state(struct lumine_anim *anim) {
	switch (anim->type) {
	case LUMINE_ANIM_FADE_IN:
		tree_set_opacity(anim->node, 1.0f);
		wlr_scene_node_set_position(anim->node, anim->to_x, anim->to_y);
		break;
	case LUMINE_ANIM_MOVE:
		wlr_scene_node_set_position(anim->node, anim->to_x, anim->to_y);
		break;
	case LUMINE_ANIM_FADE_OUT:
		wlr_scene_node_destroy(anim->node);
		break;
	}
}

static int anim_tick(void *data) {
	struct lumine_server *server = data;
	int64_t now = now_msec();

	struct lumine_anim *anim, *tmp;
	wl_list_for_each_safe(anim, tmp, &server->anims, link) {
		double progress = (double)(now - anim->start) / anim->duration;
		if (progress >= 1.0) {
			anim_apply_end_state(anim);
			wl_list_remove(&anim->link);
			free(anim);
			continue;
		}
		double t = ease_out_cubic(progress);
		switch (anim->type) {
		case LUMINE_ANIM_FADE_IN:
			tree_set_opacity(anim->node, (float)t);
			wlr_scene_node_set_position(anim->node, anim->to_x,
				anim->to_y + (int)((1.0 - t) * LUMINE_ANIM_OPEN_LIFT));
			break;
		case LUMINE_ANIM_MOVE:
			wlr_scene_node_set_position(anim->node,
				anim->from_x + (int)((anim->to_x - anim->from_x) * t),
				anim->from_y + (int)((anim->to_y - anim->from_y) * t));
			break;
		case LUMINE_ANIM_FADE_OUT:
			tree_set_opacity(anim->node, (float)(1.0 - t));
			break;
		}
	}

	if (!wl_list_empty(&server->anims)) {
		wl_event_source_timer_update(server->anim_timer,
			LUMINE_ANIM_TICK_MS);
	}
	return 0;
}

void lumine_anims_init(struct lumine_server *server) {
	wl_list_init(&server->anims);
	server->anim_timer = wl_event_loop_add_timer(
		wl_display_get_event_loop(server->display), anim_tick, server);
}

void lumine_anims_cancel(struct lumine_server *server,
		struct lumine_toplevel *toplevel) {
	struct lumine_anim *anim, *tmp;
	wl_list_for_each_safe(anim, tmp, &server->anims, link) {
		if (anim->toplevel == toplevel) {
			anim_apply_end_state(anim);
			wl_list_remove(&anim->link);
			free(anim);
		}
	}
}

void lumine_anim_fade_in(struct lumine_server *server,
		struct lumine_toplevel *toplevel) {
	if (server->anim_timer == NULL) {
		return;
	}
	lumine_anims_cancel(server, toplevel);

	struct lumine_anim *anim = calloc(1, sizeof(*anim));
	if (anim == NULL) {
		return;
	}
	anim->server = server;
	anim->toplevel = toplevel;
	anim->node = &toplevel->scene_tree->node;
	anim->type = LUMINE_ANIM_FADE_IN;
	anim->start = now_msec();
	anim->duration = LUMINE_ANIM_OPEN_MS;
	anim->to_x = toplevel->x;
	anim->to_y = toplevel->y;

	tree_set_opacity(anim->node, 0.0f);
	wlr_scene_node_set_position(anim->node, anim->to_x,
		anim->to_y + LUMINE_ANIM_OPEN_LIFT);
	wl_list_insert(server->anims.prev, &anim->link);
	anim_start(server);
}

void lumine_anim_move_to(struct lumine_server *server,
		struct lumine_toplevel *toplevel, int to_x, int to_y) {
	struct wlr_scene_node *node = &toplevel->scene_tree->node;
	if (server->anim_timer == NULL) {
		wlr_scene_node_set_position(node, to_x, to_y);
		return;
	}
	if (node->x == to_x && node->y == to_y) {
		return;
	}

	/* While a window is still fading in, retarget that animation instead
	 * of fighting it with a second writer on the node position. */
	struct lumine_anim *anim = NULL;
	struct lumine_anim *candidate;
	wl_list_for_each(candidate, &server->anims, link) {
		if (candidate->toplevel != toplevel) {
			continue;
		}
		if (candidate->type == LUMINE_ANIM_FADE_IN) {
			candidate->to_x = to_x;
			candidate->to_y = to_y;
			return;
		}
		if (candidate->type == LUMINE_ANIM_MOVE) {
			anim = candidate;
		}
	}

	if (anim == NULL) {
		anim = calloc(1, sizeof(*anim));
		if (anim == NULL) {
			wlr_scene_node_set_position(node, to_x, to_y);
			return;
		}
		anim->server = server;
		anim->toplevel = toplevel;
		anim->node = node;
		anim->type = LUMINE_ANIM_MOVE;
		anim->from_x = node->x;
		anim->from_y = node->y;
		anim->start = now_msec();
		anim->duration = LUMINE_ANIM_MOVE_MS;
		wl_list_insert(server->anims.prev, &anim->link);
	}
	anim->to_x = to_x;
	anim->to_y = to_y;
	anim_start(server);
}

void lumine_anim_fade_out(struct lumine_server *server,
		struct wlr_scene_node *node) {
	if (server->anim_timer == NULL) {
		wlr_scene_node_destroy(node);
		return;
	}

	struct lumine_anim *anim = calloc(1, sizeof(*anim));
	if (anim == NULL) {
		wlr_scene_node_destroy(node);
		return;
	}
	anim->server = server;
	anim->toplevel = NULL; /* survives the toplevel */
	anim->node = node;
	anim->type = LUMINE_ANIM_FADE_OUT;
	anim->start = now_msec();
	anim->duration = LUMINE_ANIM_CLOSE_MS;

	wl_list_insert(server->anims.prev, &anim->link);
	anim_start(server);
}
