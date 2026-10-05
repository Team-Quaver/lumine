/*
 * lumine-drag-test: regression test for interactive-move pointer state.
 *
 * Acts like a CSD title bar: on button press it sends xdg_toplevel.move,
 * handing the drag to the compositor. The compositor must still deliver
 * the matching button release afterwards — one that swallows the release
 * leaves the seat's implicit grab stuck forever and the pointer freezes
 * for every client. Exits 0 after two full press→release drag cycles, so
 * a wedged seat (second press never delivered) also fails the run.
 */
#define _GNU_SOURCE

#include <signal.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

#include <wayland-client.h>
#include "xdg-shell-client-protocol.h"

static struct wl_compositor *compositor = NULL;
static struct xdg_wm_base *wm_base = NULL;
static struct wl_seat *seat = NULL;
static struct wl_shm *shm = NULL;
static struct wl_pointer *pointer = NULL;

static struct wl_surface *surface = NULL;
static struct xdg_surface *xdg_surface = NULL;
static struct xdg_toplevel *xdg_toplevel = NULL;

static bool mapped = false;
static bool pressed = false;
static unsigned press_count = 0, release_count = 0;

#define WIDTH 200
#define HEIGHT 100

static const struct wl_seat_listener seat_listener_impl;
static const struct wl_pointer_listener pointer_listener_impl;
static int make_shm_pool(size_t size, void **data);

static void registry_handle_global(void *data, struct wl_registry *registry,
		uint32_t name, const char *interface, uint32_t version) {
	if (strcmp(interface, wl_compositor_interface.name) == 0) {
		compositor = wl_registry_bind(registry, name,
			&wl_compositor_interface, 4);
	} else if (strcmp(interface, xdg_wm_base_interface.name) == 0) {
		wm_base = wl_registry_bind(registry, name, &xdg_wm_base_interface,
			version < 1 ? version : 1);
	} else if (strcmp(interface, wl_seat_interface.name) == 0) {
		seat = wl_registry_bind(registry, name, &wl_seat_interface,
			version < 7 ? version : 7);
		wl_seat_add_listener(seat, &seat_listener_impl, NULL);
	} else if (strcmp(interface, wl_shm_interface.name) == 0) {
		shm = wl_registry_bind(registry, name, &wl_shm_interface, 1);
	}
}

static void registry_handle_global_remove(void *data,
		struct wl_registry *registry, uint32_t name) {
}

static const struct wl_registry_listener registry_listener = {
	.global = registry_handle_global,
	.global_remove = registry_handle_global_remove,
};

static void wm_base_handle_ping(void *data, struct xdg_wm_base *base,
		uint32_t serial) {
	xdg_wm_base_pong(base, serial);
}

static const struct xdg_wm_base_listener wm_base_listener = {
	.ping = wm_base_handle_ping,
};

static void xdg_surface_handle_configure(void *data,
		struct xdg_surface *xdg_surface, uint32_t serial) {
	xdg_surface_ack_configure(xdg_surface, serial);
	if (!mapped) {
		void *pixels = NULL;
		int fd = make_shm_pool(WIDTH * HEIGHT * 4, &pixels);
		if (fd < 0) {
			fprintf(stderr, "shm pool failed\n");
			exit(1);
		}
		memset(pixels, 0x60, WIDTH * HEIGHT * 4);
		struct wl_shm_pool *pool = wl_shm_create_pool(shm, fd,
			WIDTH * HEIGHT * 4);
		struct wl_buffer *buffer = wl_shm_pool_create_buffer(pool, 0,
			WIDTH, HEIGHT, WIDTH * 4, WL_SHM_FORMAT_XRGB8888);
		wl_shm_pool_destroy(pool);
		close(fd);
		wl_surface_attach(surface, buffer, 0, 0);
		wl_surface_commit(surface);
		mapped = true;
	}
}

static const struct xdg_surface_listener xdg_surface_listener = {
	.configure = xdg_surface_handle_configure,
};

static void toplevel_handle_configure(void *data,
		struct xdg_toplevel *toplevel, int32_t width, int32_t height,
		struct wl_array *states) {
}

static void toplevel_handle_close(void *data,
		struct xdg_toplevel *toplevel) {
	exit(0);
}

static const struct xdg_toplevel_listener toplevel_listener = {
	.configure = toplevel_handle_configure,
	.close = toplevel_handle_close,
	.configure_bounds = NULL,
	.wm_capabilities = NULL,
};

static void seat_handle_capabilities(void *data, struct wl_seat *s,
		uint32_t caps) {
	if ((caps & WL_SEAT_CAPABILITY_POINTER) != 0 && pointer == NULL) {
		pointer = wl_seat_get_pointer(s);
		wl_pointer_add_listener(pointer, &pointer_listener_impl, NULL);
	} else if ((caps & WL_SEAT_CAPABILITY_POINTER) == 0 &&
			pointer != NULL) {
		wl_pointer_release(pointer);
		pointer = NULL;
	}
}

static void seat_handle_name(void *data, struct wl_seat *s,
		const char *name) {
}

static const struct wl_seat_listener seat_listener_impl = {
	.capabilities = seat_handle_capabilities,
	.name = seat_handle_name,
};

static void pointer_handle_enter(void *data, struct wl_pointer *p,
		uint32_t serial, struct wl_surface *s, wl_fixed_t sx,
		wl_fixed_t sy) {
}

static void pointer_handle_leave(void *data, struct wl_pointer *p,
		uint32_t serial, struct wl_surface *s) {
}

static void pointer_handle_motion(void *data, struct wl_pointer *p,
		uint32_t time, wl_fixed_t sx, wl_fixed_t sy) {
}

static void pointer_handle_button(void *data, struct wl_pointer *p,
		uint32_t serial, uint32_t time, uint32_t button, uint32_t state) {
	if (state == WL_POINTER_BUTTON_STATE_PRESSED) {
		pressed = true;
		press_count++;
		fprintf(stderr, "PRESS serial=%u button=%u\n", serial, button);
		/* CSD title-bar behavior: hand the drag to the compositor. */
		xdg_toplevel_move(xdg_toplevel, seat, serial);
	} else {
		pressed = false;
		release_count++;
		fprintf(stderr, "RELEASE serial=%u button=%u\n", serial, button);
	}
}

static void pointer_handle_axis(void *data, struct wl_pointer *p,
		uint32_t time, uint32_t axis, wl_fixed_t value) {
}

static void pointer_handle_frame(void *data, struct wl_pointer *p) {
}

static void pointer_handle_axis_source(void *data, struct wl_pointer *p,
		uint32_t source) {
}

static void pointer_handle_axis_stop(void *data, struct wl_pointer *p,
		uint32_t time, uint32_t axis) {
}

static void pointer_handle_axis_discrete(void *data, struct wl_pointer *p,
		uint32_t axis, int32_t discrete) {
}

static void pointer_handle_axis_value120(void *data, struct wl_pointer *p,
		uint32_t axis, int32_t value) {
}

static void pointer_handle_axis_relative_direction(void *data,
		struct wl_pointer *p, uint32_t axis, uint32_t direction) {
}

static const struct wl_pointer_listener pointer_listener_impl = {
	.enter = pointer_handle_enter,
	.leave = pointer_handle_leave,
	.motion = pointer_handle_motion,
	.button = pointer_handle_button,
	.axis = pointer_handle_axis,
	.frame = pointer_handle_frame,
	.axis_source = pointer_handle_axis_source,
	.axis_stop = pointer_handle_axis_stop,
	.axis_discrete = pointer_handle_axis_discrete,
	.axis_value120 = pointer_handle_axis_value120,
	.axis_relative_direction = pointer_handle_axis_relative_direction,
};

static int make_shm_pool(size_t size, void **data) {
	int fd = memfd_create("lumine-drag-test", 0);
	if (fd < 0 || ftruncate(fd, size) < 0) {
		return -1;
	}
	*data = mmap(NULL, size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
	return fd;
}

int main(void) {
	signal(SIGALRM, SIG_DFL);
	alarm(15);

	struct wl_display *display = wl_display_connect(NULL);
	if (display == NULL) {
		fprintf(stderr, "failed to connect\n");
		return 1;
	}
	struct wl_registry *registry = wl_display_get_registry(display);
	wl_registry_add_listener(registry, &registry_listener, NULL);
	wl_display_roundtrip(display);
	if (compositor == NULL || wm_base == NULL || seat == NULL ||
			shm == NULL) {
		fprintf(stderr, "missing globals\n");
		return 1;
	}
	xdg_wm_base_add_listener(wm_base, &wm_base_listener, NULL);

	surface = wl_compositor_create_surface(compositor);
	xdg_surface = xdg_wm_base_get_xdg_surface(wm_base, surface);
	xdg_surface_add_listener(xdg_surface, &xdg_surface_listener, NULL);
	xdg_toplevel = xdg_surface_get_toplevel(xdg_surface);
	xdg_toplevel_add_listener(xdg_toplevel, &toplevel_listener, NULL);
	xdg_toplevel_set_title(xdg_toplevel, "lumine-drag-test");
	wl_surface_commit(surface);

	while (release_count < 2) {
		if (wl_display_dispatch(display) < 0) {
			return 1;
		}
	}

	fprintf(stderr,
		"drag cycles complete: %u press / %u release, seat state healthy\n",
		press_count, release_count);
	return press_count == release_count && !pressed ? 0 : 1;
}
