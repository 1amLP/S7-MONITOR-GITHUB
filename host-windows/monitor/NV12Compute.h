// SPDX-License-Identifier: GPL-3.0-or-later
#pragma once
#include <cstdint>

// These scalar functions are compiled as C++ in the portable tests AND inserted
// verbatim into the HLSL program. Keep signed rounding explicit on both targets.
#define S7_NV12_COLOR_MATH \
inline int s7Shift16(int v) { return v >= 0 ? (v >> 16) : -((-v + 65535) >> 16); } \
inline uint s7ClampByte(int v) { return uint(v < 0 ? 0 : (v > 255 ? 255 : v)); } \
inline uint s7Luma(int r, int g, int b) { return s7ClampByte(s7Shift16(11966*r + 40254*g + 4064*b + 32768) + 16); } \
inline uint s7Chroma(int r, int g, int b) { \
    return s7ClampByte(s7Shift16(-6596*r - 22189*g + 28785*b + 32768) + 128) | \
          (s7ClampByte(s7Shift16(28785*r - 26145*g - 2640*b + 32768) + 128) << 8); \
} \
inline uint s7Pack4(uint a, uint b, uint c, uint d) { return a | (b << 8) | (c << 16) | (d << 24); }
#define S7_NV12_STRING_IMPL(...) #__VA_ARGS__
#define S7_NV12_STRING(...) S7_NV12_STRING_IMPL(__VA_ARGS__)
namespace s7 { namespace nv12compute {
using uint = std::uint32_t;
S7_NV12_COLOR_MATH
inline constexpr unsigned Width = 1280, Height = 720;
inline uint s7YWord(uint x,uint y,uint width=Width){return (y*width+x)>>2;}
inline uint s7UVWord(uint x,uint y,uint width=Width,uint height=Height){return (width*height+(y>>1)*width+x)>>2;}
inline constexpr unsigned Bytes = Width * Height * 3 / 2;
inline constexpr unsigned TileWidth = 4, TileHeight = 2;
inline constexpr unsigned GroupWidth = 16, GroupHeight = 8;
inline constexpr unsigned GroupsX = Width / TileWidth / GroupWidth;
inline constexpr unsigned GroupsY = (Height / TileHeight + GroupHeight - 1) / GroupHeight;
static_assert(Width % (TileWidth * GroupWidth) == 0 && Height % TileHeight == 0, "NV12 tile geometry");

// One invocation owns four horizontal pixels on two rows. Every output uint
// has exactly one writer. No cross-group barriers, atomics or partial UV words.
// Sample an UNORM (not sRGB) view to preserve the original desktop code values.
inline constexpr char Shader[] = S7_NV12_STRING(S7_NV12_COLOR_MATH) R"HLSL(
#ifndef S7_WIDTH
#define S7_WIDTH 1280
#endif
#ifndef S7_HEIGHT
#define S7_HEIGHT 720
#endif
uint s7YWord(uint x,uint y){return (y*S7_WIDTH+x)>>2;}
uint s7UVWord(uint x,uint y){return (S7_WIDTH*S7_HEIGHT+(y>>1)*S7_WIDTH+x)>>2;}
Texture2D<float4> desktop : register(t0);
RWStructuredBuffer<uint> nv12 : register(u0);
int3 rgb(uint x, uint y) {
    return int3(floor(desktop.Load(int3(x, y, 0)).rgb * 255.0 + 0.5));
}
uint luma(int3 p) { return s7Luma(p.r, p.g, p.b); }
uint chroma(int3 p) { return s7Chroma(p.r, p.g, p.b); }
[numthreads(16, 8, 1)]
void convert(uint3 tile : SV_DispatchThreadID) {
    uint x = tile.x * 4, y = tile.y * 2;
    if (x >= S7_WIDTH || y >= S7_HEIGHT) return;
    int3 a = rgb(x, y),     b = rgb(x+1, y),     c = rgb(x+2, y),     d = rgb(x+3, y);
    int3 e = rgb(x, y+1),   f = rgb(x+1, y+1),   g = rgb(x+2, y+1),   h = rgb(x+3, y+1);
    nv12[s7YWord(x, y)] = s7Pack4(luma(a), luma(b), luma(c), luma(d));
    nv12[s7YWord(x, y+1)] = s7Pack4(luma(e), luma(f), luma(g), luma(h));
    int3 left = (a+b+e+f+2) >> 2, right = (c+d+g+h+2) >> 2;
    nv12[s7UVWord(x, y)] = chroma(left) | (chroma(right) << 16);
}
)HLSL";
}}
#undef S7_NV12_STRING
#undef S7_NV12_STRING_IMPL
#undef S7_NV12_COLOR_MATH
