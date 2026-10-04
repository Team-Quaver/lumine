/*
 * lumine-shot: tiny screencopy screenshot client.
 *
 * grim cannot read the 24bpp BGR888 readback format that NVIDIA GLES2
 * produces, so Lumine ships its own capture tool. Supports the common shm
 * formats and writes a PNG via zlib.
 */
#define _GNU_SOURCE

#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>
#include <zlib.h>

#include <wayland-client.h>
#include "wlr-screencopy-unstable-v1-client-protocol.h"

struct shot_state {
	struct zwlr_screencopy_manager_v1 *screencopy;
	struct wl_shm *shm;
	struct wl_output *output;

	int32_t width, height, stride;
	uint32_t format;
	uint8_t *data;
	bool size_known, done, failed;
};

static void frame_handle_buffer(void *data,
		struct zwlr_screencopy_frame_v1 *frame, uint32_t format,
		uint32_t width, uint32_t height, uint32_t stride) {
	struct shot_state *shot = data;
	shot->format = format;
	shot->width = width;
	shot->height = height;
	shot->stride = stride;
	shot->size_known = true;
}

static void frame_handle_flags(void *data,
		struct zwlr_screencopy_frame_v1 *frame, uint32_t flags) {
}

static void frame_handle_ready(void *data,
		struct zwlr_screencopy_frame_v1 *frame, uint32_t tv_sec_hi,
		uint32_t tv_sec_lo, uint32_t tv_nsec) {
	struct shot_state *shot = data;
	shot->done = true;
}

static void frame_handle_failed(void *data,
		struct zwlr_screencopy_frame_v1 *frame) {
	struct shot_state *shot = data;
	shot->failed = true;
}

static void frame_handle_damage(void *data,
		struct zwlr_screencopy_frame_v1 *frame, uint32_t x, uint32_t y,
		uint32_t width, uint32_t height) {
}

static void frame_handle_linux_dmabuf(void *data,
		struct zwlr_screencopy_frame_v1 *frame, uint32_t format,
		uint32_t width, uint32_t height) {
}

static void frame_handle_buffer_done(void *data,
		struct zwlr_screencopy_frame_v1 *frame) {
}

static const struct zwlr_screencopy_frame_v1_listener frame_listener = {
	.buffer = frame_handle_buffer,
	.flags = frame_handle_flags,
	.ready = frame_handle_ready,
	.failed = frame_handle_failed,
	.damage = frame_handle_damage,
	.linux_dmabuf = frame_handle_linux_dmabuf,
	.buffer_done = frame_handle_buffer_done,
};

static void output_handle_geometry(void *data, struct wl_output *output,
		int32_t x, int32_t y, int32_t pw, int32_t ph, int32_t subpixel,
		const char *make, const char *model, int32_t transform) {
}
static void output_handle_mode(void *data, struct wl_output *output,
		uint32_t flags, int32_t width, int32_t height, int32_t refresh) {
}
static void output_handle_done(void *data, struct wl_output *output) {
}
static void output_handle_scale(void *data, struct wl_output *output,
		int32_t scale) {
}
static void output_handle_name(void *data, struct wl_output *output,
		const char *name) {
}
static void output_handle_description(void *data, struct wl_output *output,
		const char *description) {
}

static const struct wl_output_listener output_listener = {
	.geometry = output_handle_geometry,
	.mode = output_handle_mode,
	.done = output_handle_done,
	.scale = output_handle_scale,
	.name = output_handle_name,
	.description = output_handle_description,
};

static void registry_handle_global(void *data, struct wl_registry *registry,
		uint32_t name, const char *interface, uint32_t version) {
	struct shot_state *shot = data;
	if (strcmp(interface, wl_output_interface.name) == 0) {
		if (shot->output == NULL) {
			shot->output = wl_registry_bind(registry, name,
				&wl_output_interface, version < 4 ? version : 4);
		}
	} else if (strcmp(interface, zwlr_screencopy_manager_v1_interface.name) == 0) {
		shot->screencopy = wl_registry_bind(registry, name,
			&zwlr_screencopy_manager_v1_interface,
			version < 3 ? version : 3);
	} else if (strcmp(interface, wl_shm_interface.name) == 0) {
		shot->shm = wl_registry_bind(registry, name, &wl_shm_interface, 1);
	}
}

static void registry_handle_global_remove(void *data,
		struct wl_registry *registry, uint32_t name) {
}

static const struct wl_registry_listener registry_listener = {
	.global = registry_handle_global,
	.global_remove = registry_handle_global_remove,
};

/* PNG writing: 8-bit truecolor, zlib deflate, hand-rolled chunks. */
static uint32_t crc_table[256];
static bool crc_ready = false;

static void crc_init(void) {
	for (uint32_t n = 0; n < 256; n++) {
		uint32_t c = n;
		for (int k = 0; k < 8; k++) {
			c = c & 1 ? 0xedb88320u ^ (c >> 1) : c >> 1;
		}
		crc_table[n] = c;
	}
	crc_ready = true;
}

static uint32_t crc_update(uint32_t crc, const uint8_t *buf, size_t len) {
	crc ^= 0xffffffffu;
	for (size_t i = 0; i < len; i++) {
		crc = crc_table[(crc ^ buf[i]) & 0xff] ^ (crc >> 8);
	}
	return crc ^ 0xffffffffu;
}

static void chunk_write(FILE *f, const char *type, const uint8_t *data,
		size_t len) {
	uint8_t header[8] = {
		len >> 24, len >> 16, len >> 8, len,
		type[0], type[1], type[2], type[3],
	};
	fwrite(header, 1, 8, f);
	if (len > 0) {
		fwrite(data, 1, len, f);
	}
	uint32_t crc = crc_update(0, (const uint8_t *)type, 4);
	crc = crc_update(crc, data, len);
	uint8_t crcb[4] = { crc >> 24, crc >> 16, crc >> 8, crc };
	fwrite(crcb, 1, 4, f);
}

static void pixel_convert(const struct shot_state *shot, uint8_t *dst,
		int32_t x, int32_t y) {
	const uint8_t *row = shot->data + (size_t)y * shot->stride;
	uint8_t *out = dst + (size_t)x * 3;
	switch (shot->format) {
	case WL_SHM_FORMAT_XRGB8888:
	case WL_SHM_FORMAT_ARGB8888: {
		const uint8_t *px = row + (size_t)x * 4;
		out[0] = px[2];
		out[1] = px[1];
		out[2] = px[0];
		break;
	}
	case WL_SHM_FORMAT_XBGR8888:
	case WL_SHM_FORMAT_ABGR8888: {
		const uint8_t *px = row + (size_t)x * 4;
		out[0] = px[0];
		out[1] = px[1];
		out[2] = px[2];
		break;
	}
	case WL_SHM_FORMAT_BGR888: {
		const uint8_t *px = row + (size_t)x * 3;
		out[0] = px[2];
		out[1] = px[1];
		out[2] = px[0];
		break;
	}
	case WL_SHM_FORMAT_RGB888: {
		const uint8_t *px = row + (size_t)x * 3;
		out[0] = px[0];
		out[1] = px[1];
		out[2] = px[2];
		break;
	}
	default:
		out[0] = out[1] = out[2] = 255;
		break;
	}
}

int main(int argc, char *argv[]) {
	if (argc != 2) {
		fprintf(stderr, "usage: lumine-shot <output.png>\n");
		return 1;
	}
	if (!crc_ready) {
		crc_init();
	}

	struct shot_state shot = {0};
	struct wl_display *display = wl_display_connect(NULL);
	if (display == NULL) {
		fprintf(stderr, "failed to connect to a Wayland compositor\n");
		return 1;
	}

	struct wl_registry *registry = wl_display_get_registry(display);
	wl_registry_add_listener(registry, &registry_listener, &shot);
	wl_display_roundtrip(display);
	if (shot.screencopy == NULL || shot.shm == NULL || shot.output == NULL) {
		fprintf(stderr,
			"missing globals: screencopy=%p shm=%p output=%p\n",
			(void *)shot.screencopy, (void *)shot.shm,
			(void *)shot.output);
		return 1;
	}

	struct zwlr_screencopy_frame_v1 *frame =
		zwlr_screencopy_manager_v1_capture_output(shot.screencopy, 0,
			shot.output);
	zwlr_screencopy_frame_v1_add_listener(frame, &frame_listener, &shot);

	/* Buffer event carries the geometry; then allocate and copy. */
	while (!shot.size_known) {
		if (wl_display_dispatch(display) < 0) {
			fprintf(stderr, "display error\n");
			return 1;
		}
	}

	size_t size = (size_t)shot.stride * shot.height;
	int fd = memfd_create("lumine-shot", 0);
	if (fd < 0 || ftruncate(fd, size) < 0) {
		perror("memfd");
		return 1;
	}
	shot.data = mmap(NULL, size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
	if (shot.data == MAP_FAILED) {
		perror("mmap");
		return 1;
	}

	struct wl_shm_pool *pool = wl_shm_create_pool(shot.shm, fd, size);
	struct wl_buffer *buffer = wl_shm_pool_create_buffer(pool, 0,
		shot.width, shot.height, shot.stride, shot.format);
	wl_shm_pool_destroy(pool);
	close(fd);

	zwlr_screencopy_frame_v1_copy(frame, buffer);
	while (!shot.done && !shot.failed) {
		if (wl_display_dispatch(display) < 0) {
			fprintf(stderr, "display error\n");
			return 1;
		}
	}
	if (shot.failed) {
		fprintf(stderr, "screencopy failed\n");
		return 1;
	}

	size_t row_bytes = (size_t)shot.width * 3;
	size_t raw_size = shot.height * (1 + row_bytes);
	uint8_t *raw = malloc(raw_size);
	uint8_t *row_rgb = malloc(row_bytes);
	for (int32_t y = 0; y < shot.height; y++) {
		for (int32_t x = 0; x < shot.width; x++) {
			pixel_convert(&shot, row_rgb, x, y);
		}
		raw[y * (1 + row_bytes)] = 0; /* filter: none */
		memcpy(raw + y * (1 + row_bytes) + 1, row_rgb, row_bytes);
	}

	uLongf compressed_size = compressBound(raw_size);
	uint8_t *compressed = malloc(compressed_size);
	if (compress2(compressed, &compressed_size, raw, raw_size,
			Z_BEST_SPEED) != Z_OK) {
		fprintf(stderr, "compress failed\n");
		return 1;
	}

	FILE *f = fopen(argv[1], "wb");
	if (f == NULL) {
		perror("fopen");
		return 1;
	}
	static const uint8_t signature[8] = { 137, 80, 78, 71, 13, 10, 26, 10 };
	fwrite(signature, 1, 8, f);

	uint8_t ihdr[13] = {
		shot.width >> 24, shot.width >> 16, shot.width >> 8, shot.width,
		shot.height >> 24, shot.height >> 16, shot.height >> 8, shot.height,
		8, /* bit depth */
		2, /* color type: truecolor */
		0, 0, 0,
	};
	chunk_write(f, "IHDR", ihdr, 13);
	chunk_write(f, "IDAT", compressed, compressed_size);
	chunk_write(f, "IEND", NULL, 0);
	fclose(f);

	fprintf(stderr, "wrote %s (%dx%d, shm format %08x)\n", argv[1],
		shot.width, shot.height, shot.format);
	return 0;
}
