/*
 * lumine-input: synthetic pointer input via zwlr_virtual_pointer_v1.
 * Used to exercise compositor pointer paths (drag, click) in tests.
 *
 * usage:
 *   lumine-input abs X Y W H            move to X/Y within a WxH extent
 *   lumine-input press|release|click BTN  (BTN_LEFT=0x110)
 *   lumine-input drag X1 Y1 X2 Y2 W H   press, glide, release
 */
#define _GNU_SOURCE

#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include <wayland-client.h>
#include "wlr-virtual-pointer-unstable-v1-client-protocol.h"

static struct zwlr_virtual_pointer_manager_v1 *vp_manager = NULL;
static struct wl_seat *seat = NULL;

static void registry_handle_global(void *data, struct wl_registry *registry,
		uint32_t name, const char *interface, uint32_t version) {
	if (strcmp(interface, zwlr_virtual_pointer_manager_v1_interface.name) == 0) {
		vp_manager = wl_registry_bind(registry, name,
			&zwlr_virtual_pointer_manager_v1_interface,
			version < 2 ? version : 2);
	} else if (strcmp(interface, wl_seat_interface.name) == 0) {
		seat = wl_registry_bind(registry, name, &wl_seat_interface,
			version < 7 ? version : 7);
	}
}

static void registry_handle_global_remove(void *data,
		struct wl_registry *registry, uint32_t name) {
}

static const struct wl_registry_listener registry_listener = {
	.global = registry_handle_global,
	.global_remove = registry_handle_global_remove,
};

int main(int argc, char *argv[]) {
	if (argc < 2) {
		fprintf(stderr, "usage: %s abs|press|release|click|drag ...\n",
			argv[0]);
		return 1;
	}

	struct wl_display *display = wl_display_connect(NULL);
	if (display == NULL) {
		fprintf(stderr, "failed to connect\n");
		return 1;
	}
	struct wl_registry *registry = wl_display_get_registry(display);
	wl_registry_add_listener(registry, &registry_listener, NULL);
	wl_display_roundtrip(display);
	if (vp_manager == NULL) {
		fprintf(stderr, "no virtual pointer manager\n");
		return 1;
	}

	struct zwlr_virtual_pointer_v1 *vp =
		zwlr_virtual_pointer_manager_v1_create_virtual_pointer(vp_manager,
			seat);
	uint32_t time = 100;

	if (strcmp(argv[1], "abs") == 0 && argc == 6) {
		int x = atoi(argv[2]), y = atoi(argv[3]);
		int w = atoi(argv[4]), h = atoi(argv[5]);
		zwlr_virtual_pointer_v1_motion_absolute(vp, time, x, y, w, h);
		zwlr_virtual_pointer_v1_frame(vp);
	} else if (strcmp(argv[1], "press") == 0 ||
			strcmp(argv[1], "release") == 0 ||
			strcmp(argv[1], "click") == 0) {
		uint32_t btn = argc > 2 ? (uint32_t)strtoul(argv[2], NULL, 0) : 0x110;
		if (strcmp(argv[1], "release") != 0) {
			zwlr_virtual_pointer_v1_button(vp, time, btn,
				WL_POINTER_BUTTON_STATE_PRESSED);
			zwlr_virtual_pointer_v1_frame(vp);
		}
		if (strcmp(argv[1], "release") != 0 &&
				strcmp(argv[1], "press") != 0) {
			usleep(80 * 1000);
			time += 80;
		}
		if (strcmp(argv[1], "press") != 0) {
			zwlr_virtual_pointer_v1_button(vp, time, btn,
				WL_POINTER_BUTTON_STATE_RELEASED);
			zwlr_virtual_pointer_v1_frame(vp);
		}
	} else if (strcmp(argv[1], "drag") == 0 && argc >= 8) {
		int x1 = atoi(argv[2]), y1 = atoi(argv[3]);
		int x2 = atoi(argv[4]), y2 = atoi(argv[5]);
		int w = atoi(argv[6]), h = atoi(argv[7]);
		int steps = argc > 8 ? atoi(argv[8]) : 15;
		zwlr_virtual_pointer_v1_motion_absolute(vp, time, x1, y1, w, h);
		zwlr_virtual_pointer_v1_frame(vp);
		zwlr_virtual_pointer_v1_button(vp, time, 0x110,
			WL_POINTER_BUTTON_STATE_PRESSED);
		zwlr_virtual_pointer_v1_frame(vp);
		for (int i = 1; i <= steps; i++) {
			usleep(16 * 1000);
			time += 16;
			zwlr_virtual_pointer_v1_motion_absolute(vp, time,
				x1 + (x2 - x1) * i / steps,
				y1 + (y2 - y1) * i / steps, w, h);
			zwlr_virtual_pointer_v1_frame(vp);
			wl_display_flush(display);
		}
		zwlr_virtual_pointer_v1_button(vp, time, 0x110,
			WL_POINTER_BUTTON_STATE_RELEASED);
		zwlr_virtual_pointer_v1_frame(vp);
	} else {
		fprintf(stderr, "unknown command %s\n", argv[1]);
		return 1;
	}

	wl_display_flush(display);
	usleep(50 * 1000);
	return 0;
}
