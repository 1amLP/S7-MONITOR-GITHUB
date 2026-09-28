__kernel void gaussian(__global const uint *src,__global uint *dst,int width,int height,int vertical) {
    int x=get_global_id(0),y=get_global_id(1);
    const float weights[5]={13.0f,10.0f,7.0f,4.0f,1.0f};
    float3 sum=(float3)(0.0f);
    for(int i=-4;i<=4;i++) {
        int sx=clamp(x+(vertical?0:i),0,width-1),sy=clamp(y+(vertical?i:0),0,height-1);
        uint p=src[sy*width+sx];
        sum+=(float3)((p>>16)&255,(p>>8)&255,p&255)*weights[abs(i)];
    }
    uint3 color=convert_uint3_sat_rte(sum/57.0f);
    dst[y*width+x]=0xff000000u|(color.x<<16)|(color.y<<8)|color.z;
}

__kernel void preview_rgb(__global const uchar *yPlane, __global const uchar *uvPlane, __global uint *rgb,
                          int width, int height, int cw, int ch, int rotation, int mirror, int panelRotation,
                          int yStride, int uvStride, int yOffset, int uvOffset, int matrix, int canvasWidth, int canvasHeight) {
    int ox = get_global_id(0), oy = get_global_id(1);
    int physicalWidth = panelRotation % 180 ? canvasHeight : canvasWidth;
    int output = oy * physicalWidth + ox;
    int x = ox, y = oy;
    if (panelRotation == 90) { x = oy; y = canvasHeight - 1 - ox; }
    else if (panelRotation == 180) { x = canvasWidth - 1 - ox; y = canvasHeight - 1 - oy; }
    else if (panelRotation == 270) { x = canvasWidth - 1 - oy; y = ox; }
    int swapped = rotation == 90 || rotation == 270;
    int rw = swapped ? ch : cw, rh = swapped ? cw : ch;
    int dw = canvasWidth, dh = (canvasWidth * rh / rw) & ~1;
    if (dh > canvasHeight) { dh = canvasHeight; dw = (canvasHeight * rw / rh) & ~1; }
    int left = ((canvasWidth - dw) / 2) & ~1, top = ((canvasHeight - dh) / 2) & ~1;
    if (x < left || y < top || x >= left + dw || y >= top + dh) {
        rgb[output] = 0xff000000u; return;
    }
    int sx = min(rw - 1, (2 * (x - left) + 1) * rw / (2 * dw));
    int sy = min(rh - 1, (2 * (y - top) + 1) * rh / (2 * dh));
    if (mirror) sx = rw - 1 - sx;
    int px = sx, py = sy;
    if (rotation == 90) { px = sy; py = ch - 1 - sx; }
    else if (rotation == 180) { px = cw - 1 - sx; py = ch - 1 - sy; }
    else if (rotation == 270) { px = cw - 1 - sy; py = sx; }
    px += ((width - cw) / 2) & ~1;
    py += ((height - ch) / 2) & ~1;
    int c = (int)yPlane[yOffset + py * yStride + px] - 16;
    int uv = uvOffset + (py / 2) * uvStride + (px & ~1);
    int d = (int)uvPlane[uv] - 128, e = (int)uvPlane[uv + 1] - 128;
    uint r = (uint)clamp((298 * c + (matrix ? 459 : 409) * e + 128) >> 8, 0, 255);
    uint g = (uint)clamp((298 * c - (matrix ? 55 : 100) * d - (matrix ? 136 : 208) * e + 128) >> 8, 0, 255);
    uint b = (uint)clamp((298 * c + (matrix ? 541 : 516) * d + 128) >> 8, 0, 255);
    rgb[output] = 0xff000000u | (r << 16) | (g << 8) | b;
}

// Canonical ARGB words throughout Mali/G2D and BGRA WIN_CONFIG scanout.
__kernel void compose_rgb(__global const uint *video, __global const uint *menu, __global uint *target) {
    uint i=get_global_id(0),m=menu[i],a=m>>24;
    if(a==255){target[i]=m;return;}
    uint v=video[i];
    if(a==0){target[i]=v|0xff000000u;return;}
    uint inverse=255-a;
    uint r=min(255u,((m>>16)&255)+(((v>>16)&255)*inverse+127)/255);
    uint g=min(255u,((m>>8)&255)+(((v>>8)&255)*inverse+127)/255);
    uint b=min(255u,(m&255)+((v&255)*inverse+127)/255);
    target[i]=0xff000000u|(r<<16)|(g<<8)|b;
}

// Coverage, glyph sampling and premultiplied composition all execute on Mali.
__kernel void raster(__global uint *dst, __global const uchar *atlas,
                     __global const int *commands, __global const uint *offsets,
                     __global const uint *indices, int width, int height,
                     int rotation, int tileColumns) {
    int px = get_global_id(0), py = get_global_id(1);
    int x = px, y = py;
    if (rotation == 90) { x = py; y = 1439 - px; }
    else if (rotation == 180) { x = 1439 - px; y = 2559 - py; }
    else if (rotation == 270) { x = 2559 - py; y = px; }
    if (x < 0 || y < 0 || x >= width || y >= height) return;
    uint tile = (y / 32) * tileColumns + x / 32;
    float2 point = (float2)(x + 0.5f, y + 0.5f);
    float4 result = (float4)(0.0f);
    for (uint index = offsets[tile]; index < offsets[tile + 1]; ++index) {
        __global const int *c = commands + indices[index] * 16;
        if (x < c[1] || y < c[2] || x >= c[3] || y >= c[4]) continue;
        float coverage = 0.0f;
        if (c[0] == 1) {
            float2 center = (float2)((c[7] + c[9]) * 0.5f, (c[8] + c[10]) * 0.5f);
            float2 halfSize = (float2)((c[9] - c[7]) * 0.5f, (c[10] - c[8]) * 0.5f);
            float radius = clamp((float)c[11], 0.0f, min(halfSize.x, halfSize.y));
            float2 q = fabs(point - center) - (halfSize - radius);
            float distance = length(max(q, (float2)(0.0f))) + min(max(q.x, q.y), 0.0f) - radius;
            coverage = clamp(0.5f - distance, 0.0f, 1.0f);
        } else if (c[0] == 2) {
            int gx = x - c[7], gy = y - c[8];
            if (gx >= 0 && gy >= 0 && gx < c[9] && gy < c[10])
                coverage = atlas[c[11] + gy * c[9] + gx] * (1.0f / 255.0f);
        } else if (c[0] == 3) {
            float2 a = (float2)(c[7] + 0.5f, c[8] + 0.5f);
            float2 b = (float2)(c[9] + 0.5f, c[10] + 0.5f);
            float2 direction = b - a;
            float fraction = clamp(dot(point - a, direction) / max(dot(direction, direction), 0.001f), 0.0f, 1.0f);
            coverage = clamp(c[11] * 0.5f + 0.5f - length(point - (a + fraction * direction)), 0.0f, 1.0f);
        }
        float alpha = coverage * c[6] * (1.0f / 255.0f);
        uint rgb = (uint)c[5];
        float4 source = (float4)((rgb >> 16) & 255, (rgb >> 8) & 255, rgb & 255, 255) * (alpha / 255.0f);
        result = source + result * (1.0f - alpha);
    }
    uint4 color = convert_uint4_sat_rte(clamp(result, 0.0f, 1.0f) * 255.0f);
    dst[py * 1440 + px] = (color.w << 24) | (color.x << 16) | (color.y << 8) | color.z;
}
