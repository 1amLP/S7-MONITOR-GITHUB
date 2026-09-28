// SPDX-License-Identifier: GPL-3.0-or-later
#pragma once
#include "HostWindows.h"
#include "Protocol.h"
#include <d3d11.h>

namespace s7 {
// Owned by the acquisition thread. Exactly one outstanding conversion/readback;
// the encoder thread receives ordinary owned NV12 bytes, never GPU resources.
class DesktopReadback {
    ComPtr<ID3D11Device> device_;
    ComPtr<ID3D11DeviceContext> context_;
    ComPtr<ID3D11Texture2D> input_;
    ComPtr<ID3D11Buffer> output_, gpuStaging_;
    ComPtr<ID3D11ShaderResourceView> sourceView_;
    ComPtr<ID3D11UnorderedAccessView> outputView_;
    ComPtr<ID3D11ComputeShader> shader_;
	unsigned width_,height_,bytes_;
    bool pending_=false;
    bool initializeGPU();
public:
    DesktopReadback(ID3D11Device* device, ID3D11DeviceContext* context,unsigned width=1280,unsigned height=720);
    DesktopReadback(const DesktopReadback&)=delete;
    DesktopReadback& operator=(const DesktopReadback&)=delete;
    bool gpuConversion() const noexcept { return true; }
    void submit(ID3D11Texture2D* source);
    // nullptr discards a completed old-generation copy without copying pixels.
    // false means GPU not ready; no wait and no overwrite of the pending copy.
    bool poll(Bytes* destination);
};
}
