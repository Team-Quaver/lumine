#include <drm_fourcc.h>
#include <stdlib.h>
#include <time.h>

#include "lumine.h"
#include <wlr/render/color.h>
#include <wlr/util/log.h>

static void output_handle_frame(struct wl_listener *listener, void *data) {
	struct lumine_output *output =
		wl_container_of(listener, output, frame);
	struct wlr_scene *scene = output->server->scene;

	struct wlr_scene_output *scene_output =
		wlr_scene_get_scene_output(scene, output->wlr_output);
	if (scene_output == NULL) {
		return;
	}

	wlr_scene_output_commit(scene_output, NULL);

	struct timespec now = {0};
	clock_gettime(CLOCK_MONOTONIC, &now);
	wlr_scene_output_send_frame_done(scene_output, &now);
}

static void output_handle_request_state(struct wl_listener *listener,
		void *data) {
	struct lumine_output *output =
		wl_container_of(listener, output, request_state);
	const struct wlr_output_event_request_state *event = data;

	if (!wlr_output_commit_state(output->wlr_output, event->state)) {
		return;
	}
	lumine_output_arrange(output);
}

static void output_handle_destroy(struct wl_listener *listener, void *data) {
	struct lumine_output *output = wl_container_of(listener, output, destroy);
	wl_list_remove(&output->link);
	wl_list_remove(&output->frame.link);
	wl_list_remove(&output->request_state.link);
	wl_list_remove(&output->destroy.link);
	free(output);
}

/*
 * Try to enable HDR (BT.2020 primaries + SMPTE ST 2084 PQ transfer function)
 * on the output. wlroots converts the committed image description into a
 * DRM HDR_OUTPUT_METADATA property blob on the atomic DRM backend; the scene
 * graph then renders everything through the matching inverse EOTF.
 */
static bool output_try_enable_hdr(struct lumine_output *output) {
	struct wlr_output *wlr_output = output->wlr_output;

	if (!(wlr_output->supported_transfer_functions &
			WLR_COLOR_TRANSFER_FUNCTION_ST2084_PQ) ||
			!(wlr_output->supported_primaries &
			WLR_COLOR_NAMED_PRIMARIES_BT2020)) {
		wlr_log(WLR_INFO, "output %s: HDR not supported by backend/panel",
			wlr_output->name);
		return false;
	}

	struct wlr_color_primaries bt2020;
	wlr_color_primaries_from_named(&bt2020, WLR_COLOR_NAMED_PRIMARIES_BT2020);

	const struct wlr_output_image_description desc = {
		.primaries = WLR_COLOR_NAMED_PRIMARIES_BT2020,
		.transfer_function = WLR_COLOR_TRANSFER_FUNCTION_ST2084_PQ,
		.mastering_display_primaries = bt2020,
		.mastering_luminance = { .min = 0.005, .max = 10000 },
		.max_cll = 10000,
		.max_fall = 10000,
	};

	/* Prefer a 10bpc render format when the hardware allows it. */
	static const uint32_t formats[] = {
		DRM_FORMAT_XBGR2101010,
		DRM_FORMAT_ABGR2101010,
		DRM_FORMAT_XBGR8888,
	};

	for (size_t i = 0; i < sizeof(formats) / sizeof(formats[0]); i++) {
		struct wlr_output_state state;
		wlr_output_state_init(&state);
		wlr_output_state_set_image_description(&state, &desc);
		wlr_output_state_set_render_format(&state, formats[i]);

		if (wlr_output_test_state(wlr_output, &state) &&
				wlr_output_commit_state(wlr_output, &state)) {
			wlr_output_state_finish(&state);
			wlr_log(WLR_INFO,
				"output %s: HDR enabled (BT.2020 + PQ, format " "%08x)",
				wlr_output->name, formats[i]);
			return true;
		}
		wlr_output_state_finish(&state);
	}

	wlr_log(WLR_ERROR, "output %s: HDR test/commit failed", wlr_output->name);
	return false;
}

static void output_disable_hdr(struct lumine_output *output) {
	/* Committing a NULL image description returns the output to SDR. */
	struct wlr_output_state state;
	wlr_output_state_init(&state);
	wlr_output_state_set_image_description(&state, NULL);
	wlr_output_commit_state(output->wlr_output, &state);
	wlr_output_state_finish(&state);
}

void lumine_output_toggle_hdr(struct lumine_output *output) {
	if (output->hdr_enabled) {
		output_disable_hdr(output);
		output->hdr_enabled = false;
		wlr_log(WLR_INFO, "output %s: HDR disabled", output->wlr_output->name);
	} else {
		output->hdr_enabled = output_try_enable_hdr(output);
	}
}

void lumine_output_arrange(struct lumine_output *output) {
	struct wlr_box full = {0};
	wlr_output_layout_get_box(output->server->output_layout,
		output->wlr_output, &full);
	if (wlr_box_empty(&full)) {
		return;
	}
	output->usable = full;

	/* Configure layer surfaces in layer order; exclusive zones shrink the
	 * usable area handed to the window layout. */
	static const enum zwlr_layer_shell_v1_layer layers[] = {
		ZWLR_LAYER_SHELL_V1_LAYER_BACKGROUND,
		ZWLR_LAYER_SHELL_V1_LAYER_BOTTOM,
		ZWLR_LAYER_SHELL_V1_LAYER_TOP,
		ZWLR_LAYER_SHELL_V1_LAYER_OVERLAY,
	};
	for (size_t i = 0; i < sizeof(layers) / sizeof(layers[0]); i++) {
		struct lumine_layer *layer;
		wl_list_for_each(layer, &output->server->layers, link) {
			if (layer->output != output ||
					layer->layer_surface->current.layer != layers[i]) {
				continue;
			}
			/* Not configure-able until the client's initial commit, and
			 * configuring an unmapped (e.g. dying) surface would send
			 * events to a resource the client is destroying. */
			if (!layer->layer_surface->initialized ||
					!layer->layer_surface->surface->mapped) {
				continue;
			}
			wlr_scene_layer_surface_v1_configure(layer->scene_layer,
				&full, &output->usable);
			layer->arranged = layer->layer_surface->current;
			layer->has_arranged = true;
		}
	}

	lumine_arrange_output(output);
}

void lumine_handle_new_output(struct wl_listener *listener, void *data) {
	struct lumine_server *server =
		wl_container_of(listener, server, new_output);
	struct wlr_output *wlr_output = data;

	wlr_output_init_render(wlr_output, server->allocator, server->renderer);

	struct lumine_output *output = calloc(1, sizeof(*output));
	if (output == NULL) {
		return;
	}
	output->wlr_output = wlr_output;
	output->server = server;
	wl_list_insert(&server->outputs, &output->link);

	output->frame.notify = output_handle_frame;
	wl_signal_add(&wlr_output->events.frame, &output->frame);
	output->request_state.notify = output_handle_request_state;
	wl_signal_add(&wlr_output->events.request_state, &output->request_state);
	output->destroy.notify = output_handle_destroy;
	wl_signal_add(&wlr_output->events.destroy, &output->destroy);

	struct wlr_output_state state;
	wlr_output_state_init(&state);
	wlr_output_state_set_enabled(&state, true);
	struct wlr_output_mode *mode = wlr_output_preferred_mode(wlr_output);
	if (mode != NULL) {
		wlr_output_state_set_mode(&state, mode);
	}
	wlr_output_commit_state(wlr_output, &state);
	wlr_output_state_finish(&state);

	output->hdr_enabled = output_try_enable_hdr(output);

	output->layout_output = wlr_output_layout_add_auto(
		server->output_layout, wlr_output);
	output->scene_output = wlr_scene_output_create(server->scene, wlr_output);
	wlr_scene_output_layout_add_output(server->scene_layout,
		output->layout_output, output->scene_output);

	lumine_output_arrange(output);

	wlr_log(WLR_INFO, "output %s: %dx%d @ %.1fHz, HDR=%s",
		wlr_output->name,
		wlr_output->width, wlr_output->height,
		mode != NULL ? (float)mode->refresh / 1000.0f : 0.0f,
		output->hdr_enabled ? "on" : "off");
}
