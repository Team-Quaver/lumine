#version 450
layout(push_constant) uniform Push {
    vec4 rect;
    vec2 outSize;
    vec4 tint;
    float alpha;
    float _pad;
} pc;
layout(location = 0) out vec2 vUV;
void main() {
    vec2 corners[4] = vec2[](vec2(0.0, 0.0), vec2(1.0, 0.0), vec2(0.0, 1.0), vec2(1.0, 1.0));
    vec2 c = corners[gl_VertexIndex];
    vUV = c - vec2(0.5) / pc.rect.zw;
    vec2 px = pc.rect.xy + c * pc.rect.zw;
    vec2 ndc = vec2(px.x / pc.outSize.x * 2.0 - 1.0, px.y / pc.outSize.y * 2.0 - 1.0);
    gl_Position = vec4(ndc, 0.0, 1.0);
}
