/*
 * lumine-layer-test: exercises the map-then-destroy lifecycle of a layer
 * surface, which is what shells like plasmashell do when they bail early.
 * A compositor that arranges dying layer surfaces will crash on this.
 */
#define _GNU_SOURCE

#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

#include <wayland-client.h>
#include "wlr-layer-shell-unstable-v1-client-protocol.h"

static struct wl_compositor *compositor = NULL;
static struct zwlr_layer_shell_v1 *layer_shell = NULL;
static struct wl_output *output = NULL;
static struct wl_shm *shm = NULL;
static bool configured = false;

static void registry_handle_global(void *data, struct wl_registry *registry,
		uint32_t name, const char *interface, uint32_t version) {
	if (strcmp(interface, wl_compositor_interface.name) == 0) {
		compositor = wl_registry_bind(registry, name,
			&wl_compositor_interface, 4);
	} else if (strcmp(interface, zwlr_layer_shell_v1_interface.name) == 0) {
		layer_shell = wl_registry_bind(registry, name,
			&zwlr_layer_shell_v1_interface, version < 4 ? version : 4);
	} else if (strcmp(interface, wl_output_interface.name) == 0) {
		if (output == NULL) {
			output = wl_registry_bind(registry, name,
				&wl_output_interface, 2);
		}
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

static void layer_surface_handle_configure(void *data,
		struct zwlr_layer_surface_v1 *surface, uint32_t serial,
		uint32_t width, uint32_t height) {
	configured = true;
	zwlr_layer_surface_v1_ack_configure(surface, serial);
}

static void layer_surface_handle_closed(void *data,
		struct zwlr_layer_surface_v1 *surface) {
}

static const struct zwlr_layer_surface_v1_listener layer_surface_listener = {
	.configure = layer_surface_handle_configure,
	.closed = layer_surface_handle_closed,
};

static int make_shm_pool(size_t size, void **data) {
	int fd = memfd_create("lumine-layer-test", 0);
	if (fd < 0 || ftruncate(fd, size) < 0) {
		return -1;
	}
	*data = mmap(NULL, size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
	return fd;
}

int main(void) {
	struct wl_display *display = wl_display_connect(NULL);
	if (display == NULL) {
		fprintf(stderr, "failed to connect\n");
		return 1;
	}
	struct wl_registry *registry = wl_display_get_registry(display);
	wl_registry_add_listener(registry, &registry_listener, NULL);
	wl_display_roundtrip(display);
	if (compositor == NULL || layer_shell == NULL || output == NULL ||
			shm == NULL) {
		fprintf(stderr, "missing globals\n");
		return 1;
	}

	struct wl_surface *surface = wl_compositor_create_surface(compositor);
	struct zwlr_layer_surface_v1 *ls = zwlr_layer_shell_v1_get_layer_surface(
		layer_shell, surface, output,
		ZWLR_LAYER_SHELL_V1_LAYER_BACKGROUND, "lumine-layer-test");
	zwlr_layer_surface_v1_set_anchor(ls, ZWLR_LAYER_SURFACE_V1_ANCHOR_TOP |
		ZWLR_LAYER_SURFACE_V1_ANCHOR_BOTTOM |
		ZWLR_LAYER_SURFACE_V1_ANCHOR_LEFT |
		ZWLR_LAYER_SURFACE_V1_ANCHOR_RIGHT);
	zwlr_layer_surface_v1_add_listener(ls, &layer_surface_listener, NULL);
	wl_surface_commit(surface);

	while (!configured) {
		if (wl_display_dispatch(display) < 0) {
			return 1;
		}
	}

	/* Map with a 1x1 buffer. */
	void *data = NULL;
	int fd = make_shm_pool(4, &data);
	memset(data, 0x80, 4);
	struct wl_shm_pool *pool = wl_shm_create_pool(shm, fd, 4);
	struct wl_buffer *buffer = wl_shm_pool_create_buffer(pool, 0, 1, 1, 4,
		WL_SHM_FORMAT_XRGB8888);
	wl_shm_pool_destroy(pool);
	close(fd);
	wl_surface_attach(surface, buffer, 0, 0);
	wl_surface_commit(surface);
	wl_display_roundtrip(display);
	fprintf(stderr, "layer surface mapped\n");

	/* Now destroy the role resource while mapped: the crash path. */
	zwlr_layer_surface_v1_destroy(ls);
	wl_surface_commit(surface);
	wl_display_roundtrip(display);
	fprintf(stderr, "layer surface destroyed, still alive\n");
	usleep(200 * 1000);
	wl_display_roundtrip(display);
	fprintf(stderr, "compositor survived\n");
	return 0;
}
