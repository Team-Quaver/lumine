#version 450
// Straight (non-premultiplied) source-over: the blend state in the pipeline
// multiplies by the emitted alpha, matching the software renderer's formula
// (src*a + dst*(1-a)).
layout(binding = 0) uniform sampler2D tex;
layout(push_constant) uniform Push {
    vec4 rect;
    vec2 outSize;
    vec4 tint;
    float alpha;
    float _pad;
} pc;

layout(location = 0) in vec2 vUV;
layout(location = 0) out vec4 outColor;

void main() {
    outColor = texture(tex, vUV) * pc.tint * vec4(1.0, 1.0, 1.0, pc.alpha);
}
