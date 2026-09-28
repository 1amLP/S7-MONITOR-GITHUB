// SPDX-License-Identifier: GPL-3.0-or-later
#include "DesktopReadback.h"
#include "NV12Compute.h"
#include <d3dcompiler.h>
#include <cstring>

namespace s7 {
DesktopReadback::DesktopReadback(ID3D11Device* device, ID3D11DeviceContext* context,unsigned width,unsigned height)
    : device_(device), context_(context),width_(width),height_(height),bytes_(width*height*3/2) {
	if(!Config::mode(width,height))throw Failure(E_INVALIDARG,"Unsupported readback mode");
    if(!device || !context || context->GetType()!=D3D11_DEVICE_CONTEXT_IMMEDIATE)
        throw Failure(E_INVALIDARG,"Readback requires one owned immediate context");
    ComPtr<ID3D11Device> owner;context->GetDevice(&owner);
    if(owner.Get()!=device)throw Failure(E_INVALIDARG,"Readback context belongs to another GPU device");
    if(!initializeGPU())throw Failure(E_NOTIMPL,"Render GPU cannot provide monitor NV12 conversion");
    log("Monitor colour conversion: D3D11 compute / NV12 readback");
}

bool DesktopReadback::initializeGPU() {
    if(device_->GetFeatureLevel()<D3D_FEATURE_LEVEL_11_0)return false;
    UINT supported=0;
    const HRESULT support=device_->CheckFormatSupport(DXGI_FORMAT_B8G8R8A8_UNORM,&supported);
    check(support,"Query BGRA compute input support");
    const UINT required=D3D11_FORMAT_SUPPORT_TEXTURE2D|D3D11_FORMAT_SUPPORT_SHADER_LOAD;
    if((supported&required)!=required)return false;

    // Compiler lookup is restricted to Windows System32, never CWD/PATH or the
    // USB device. No SDK, downloaded shader or compiler install on the user PC.
    // Declared before blobs so compiler-provided blob vtables outlive Release.
    struct Library {HMODULE handle;~Library(){if(handle)FreeLibrary(handle);}} compiler{
        LoadLibraryExW(L"d3dcompiler_47.dll",nullptr,LOAD_LIBRARY_SEARCH_SYSTEM32)};
	if(!compiler.handle){
		log("System HLSL compiler unavailable; GPU monitor conversion cannot start",HRESULT_FROM_WIN32(GetLastError()));
		return false;
	}
	auto compile=reinterpret_cast<decltype(&D3DCompile)>(GetProcAddress(compiler.handle,"D3DCompile"));
	if(!compile){log("System HLSL compiler has no D3DCompile; GPU monitor conversion cannot start");return false;}
    ComPtr<ID3DBlob> code, errors;
	const auto sw=std::to_string(width_),sh=std::to_string(height_);
	const D3D_SHADER_MACRO defines[]={{"S7_WIDTH",sw.c_str()},{"S7_HEIGHT",sh.c_str()},{nullptr,nullptr}};
    const HRESULT built=compile(nv12compute::Shader,sizeof(nv12compute::Shader)-1,
        "S7-owned-NV12-compute",defines,nullptr,"convert","cs_5_0",
        D3DCOMPILE_ENABLE_STRICTNESS|D3DCOMPILE_OPTIMIZATION_LEVEL3,0,&code,&errors);
    if(FAILED(built)){
        if(errors){
            const size_t bytes=std::min<size_t>(errors->GetBufferSize(),512);
            const std::string detail(static_cast<const char*>(errors->GetBufferPointer()),bytes);
            log(detail.c_str(),built);
        }
        // A shader defect is not disguised as unsupported hardware.
        throw Failure(built,"Compile embedded monitor conversion shader");
    }
    check(device_->CreateComputeShader(code->GetBufferPointer(),code->GetBufferSize(),nullptr,&shader_),"Create NV12 compute shader");
    D3D11_TEXTURE2D_DESC image{};
    image.Width=width_;image.Height=height_;
    image.MipLevels=image.ArraySize=1;image.SampleDesc.Count=1;
    image.Format=DXGI_FORMAT_B8G8R8A8_TYPELESS;image.Usage=D3D11_USAGE_DEFAULT;
    image.BindFlags=D3D11_BIND_SHADER_RESOURCE;
    check(device_->CreateTexture2D(&image,nullptr,&input_),"Create owned desktop compute input");
    D3D11_SHADER_RESOURCE_VIEW_DESC view{};
    view.Format=DXGI_FORMAT_B8G8R8A8_UNORM;view.ViewDimension=D3D11_SRV_DIMENSION_TEXTURE2D;
    view.Texture2D.MipLevels=1;
    check(device_->CreateShaderResourceView(input_.Get(),&view,&sourceView_),"Create non-sRGB desktop read view");
    D3D11_BUFFER_DESC buffer{};
    buffer.ByteWidth=bytes_;buffer.Usage=D3D11_USAGE_DEFAULT;
    buffer.BindFlags=D3D11_BIND_UNORDERED_ACCESS;
    buffer.MiscFlags=D3D11_RESOURCE_MISC_BUFFER_STRUCTURED;buffer.StructureByteStride=4;
    check(device_->CreateBuffer(&buffer,nullptr,&output_),"Create bounded packed NV12 GPU buffer");
    D3D11_UNORDERED_ACCESS_VIEW_DESC outputView{};
    outputView.Format=DXGI_FORMAT_UNKNOWN;outputView.ViewDimension=D3D11_UAV_DIMENSION_BUFFER;
    outputView.Buffer.NumElements=bytes_/4;
    check(device_->CreateUnorderedAccessView(output_.Get(),&outputView,&outputView_),"Create packed NV12 write view");
    // CPU-access resources cannot carry miscellaneous flags. Matching byte size
    // is sufficient for a buffer CopyResource; staging has no view/stride.
    buffer.Usage=D3D11_USAGE_STAGING;buffer.BindFlags=0;
    buffer.CPUAccessFlags=D3D11_CPU_ACCESS_READ;buffer.MiscFlags=0;buffer.StructureByteStride=0;
    check(device_->CreateBuffer(&buffer,nullptr,&gpuStaging_),"Create compact NV12 readback buffer");
    return true;
}

void DesktopReadback::submit(ID3D11Texture2D* source) {
    if(!source||pending_)throw Failure(E_UNEXPECTED,"GPU readback already owns a pending copy");
    D3D11_TEXTURE2D_DESC description{};source->GetDesc(&description);
    if(description.Width!=width_||description.Height!=height_||
       description.MipLevels!=1||description.ArraySize!=1||description.SampleDesc.Count!=1||
       description.SampleDesc.Quality!=0||
       (description.Format!=DXGI_FORMAT_B8G8R8A8_UNORM&&description.Format!=DXGI_FORMAT_B8G8R8A8_UNORM_SRGB))
        throw Failure(E_NOTIMPL,"Monitor conversion requires the selected BGRA dimensions");
    ComPtr<ID3D11Device> sourceDevice;source->GetDevice(&sourceDevice);
    if(sourceDevice.Get()!=device_.Get())throw Failure(E_INVALIDARG,"Cross-device desktop surface refused");
    // The source need not expose SRV binding. Copy to our own compatible
    // typeless surface before releasing the compositor's reference.
    context_->CopyResource(input_.Get(),source);
    ID3D11ShaderResourceView* srv=sourceView_.Get();
    ID3D11UnorderedAccessView* uav=outputView_.Get();
    context_->CSSetShader(shader_.Get(),nullptr,0);
    context_->CSSetShaderResources(0,1,&srv);
    context_->CSSetUnorderedAccessViews(0,1,&uav,nullptr);
    context_->Dispatch(width_/64,(height_/2+7)/8,1);
    srv=nullptr;uav=nullptr;
    context_->CSSetShaderResources(0,1,&srv);
    context_->CSSetUnorderedAccessViews(0,1,&uav,nullptr);
    context_->CSSetShader(nullptr,nullptr,0);
    context_->CopyResource(gpuStaging_.Get(),output_.Get());
    context_->Flush();pending_=true;
}

bool DesktopReadback::poll(Bytes* destination) {
    if(!pending_)return false;
    ID3D11Resource* staging=gpuStaging_.Get();
    D3D11_MAPPED_SUBRESOURCE map{};
    const HRESULT result=context_->Map(staging,0,D3D11_MAP_READ,D3D11_MAP_FLAG_DO_NOT_WAIT,&map);
    if(result==DXGI_ERROR_WAS_STILL_DRAWING)return false;
    check(result,"Map monitor colour-conversion readback");
    struct Unmap{ID3D11DeviceContext* context;ID3D11Resource* resource;
        ~Unmap(){context->Unmap(resource,0);}} unmap{context_.Get(),staging};
    pending_=false;
    if(destination){
        if(!map.pData)throw Failure(E_POINTER,"Mapped monitor image has no data");
        destination->resize(bytes_);
        std::memcpy(destination->data(),map.pData,bytes_);
    }
    return true;
}
}
