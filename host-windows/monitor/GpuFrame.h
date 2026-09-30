#pragma once
#include "HostWindows.h"
#include "Protocol.h"
#include <d3d11.h>
#include <d3d10.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mftransform.h>
#include <mferror.h>

namespace s7 {
struct GpuFrame {
    ComPtr<IMFSample> allocation;
};

// The MF allocator owns texture recycling. A retained allocation cannot be
// reused by capture while the handoff, static snapshot or encoder still owns it.
class GpuFramePool {
    ComPtr<ID3D11Device> device_;
    ComPtr<ID3D11DeviceContext> context_;
    ComPtr<ID3D11VideoDevice> video_;
    ComPtr<ID3D11VideoContext> videoContext_;
    ComPtr<ID3D11VideoProcessorEnumerator> enumerator_;
    ComPtr<ID3D11VideoProcessor> processor_;
    ComPtr<ID3D11Texture2D> input_;
    ComPtr<ID3D11VideoProcessorInputView> inputView_;
    ComPtr<IMFDXGIDeviceManager> manager_;
    ComPtr<IMFVideoSampleAllocatorEx> allocator_;
    UINT width_,height_;
public:
    GpuFramePool(ID3D11Device* device,ID3D11DeviceContext* context,UINT width,UINT height)
        :device_(device),context_(context),width_(width),height_(height){
        if(!device||!context||!Config::mode(width,height))throw Failure(E_INVALIDARG,"GPU frame pool geometry");
        ComPtr<ID3D10Multithread> multithread;check(context_.As(&multithread),"D3D11 multithread protection");
        multithread->SetMultithreadProtected(TRUE);
        check(device_.As(&video_),"D3D11 video device");check(context_.As(&videoContext_),"D3D11 video context");
        D3D11_VIDEO_PROCESSOR_CONTENT_DESC content{};
        content.InputFrameFormat=D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE;
        content.InputWidth=content.OutputWidth=width;content.InputHeight=content.OutputHeight=height;
        content.InputFrameRate={60,1};content.OutputFrameRate={60,1};content.Usage=D3D11_VIDEO_USAGE_PLAYBACK_NORMAL;
        check(video_->CreateVideoProcessorEnumerator(&content,&enumerator_),"NV12 processor enumerator");
        UINT support=0;check(enumerator_->CheckVideoProcessorFormat(DXGI_FORMAT_NV12,&support),"NV12 GPU output support");
        if(!(support&D3D11_VIDEO_PROCESSOR_FORMAT_SUPPORT_OUTPUT))throw Failure(E_NOTIMPL,"GPU NV12 output unsupported");
        check(video_->CreateVideoProcessor(enumerator_.Get(),0,&processor_),"NV12 video processor");
        D3D11_TEXTURE2D_DESC image{};image.Width=width;image.Height=height;image.MipLevels=image.ArraySize=1;
        image.SampleDesc.Count=1;image.Format=DXGI_FORMAT_B8G8R8A8_UNORM;image.Usage=D3D11_USAGE_DEFAULT;
        image.BindFlags=D3D11_BIND_RENDER_TARGET;
        check(device_->CreateTexture2D(&image,nullptr,&input_),"Owned GPU conversion input");
        D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC inputView{};inputView.ViewDimension=D3D11_VPIV_DIMENSION_TEXTURE2D;
        check(video_->CreateVideoProcessorInputView(input_.Get(),enumerator_.Get(),&inputView,&inputView_),"GPU conversion input view");
        videoContext_->VideoProcessorSetStreamFrameFormat(processor_.Get(),0,D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE);
        videoContext_->VideoProcessorSetStreamAutoProcessingMode(processor_.Get(),0,FALSE);
        RECT extent{0,0,LONG(width),LONG(height)};
        videoContext_->VideoProcessorSetStreamSourceRect(processor_.Get(),0,TRUE,&extent);
        videoContext_->VideoProcessorSetStreamDestRect(processor_.Get(),0,TRUE,&extent);
        videoContext_->VideoProcessorSetOutputTargetRect(processor_.Get(),TRUE,&extent);
        D3D11_VIDEO_PROCESSOR_COLOR_SPACE rgb{},yuv{};
        rgb.Nominal_Range=D3D11_VIDEO_PROCESSOR_NOMINAL_RANGE_0_255;
        yuv.YCbCr_Matrix=1;yuv.Nominal_Range=D3D11_VIDEO_PROCESSOR_NOMINAL_RANGE_16_235;
        videoContext_->VideoProcessorSetStreamColorSpace(processor_.Get(),0,&rgb);
        videoContext_->VideoProcessorSetOutputColorSpace(processor_.Get(),&yuv);
        UINT token=0;check(MFCreateDXGIDeviceManager(&token,&manager_),"GPU encoder device manager");
        check(manager_->ResetDevice(device_.Get(),token),"GPU encoder device binding");
        check(MFCreateVideoSampleAllocatorEx(IID_PPV_ARGS(&allocator_)),"GPU sample allocator");
        check(allocator_->SetDirectXManager(manager_.Get()),"GPU allocator device manager");
        ComPtr<IMFAttributes> attributes;check(MFCreateAttributes(&attributes,2),"GPU allocator attributes");
        check(attributes->SetUINT32(MF_SA_D3D11_BINDFLAGS,D3D11_BIND_RENDER_TARGET),"GPU allocator bind flags");
        check(attributes->SetUINT32(MF_SA_BUFFERS_PER_SAMPLE,1),"GPU allocator buffer count");
        ComPtr<IMFMediaType> type;check(MFCreateMediaType(&type),"GPU allocator media type");
        check(type->SetGUID(MF_MT_MAJOR_TYPE,MFMediaType_Video),"GPU video type");
        check(type->SetGUID(MF_MT_SUBTYPE,MFVideoFormat_NV12),"GPU NV12 type");
        check(MFSetAttributeSize(type.Get(),MF_MT_FRAME_SIZE,width,height),"GPU sample size");
        check(MFSetAttributeRatio(type.Get(),MF_MT_FRAME_RATE,60,1),"GPU sample rate");
        check(type->SetUINT32(MF_MT_INTERLACE_MODE,MFVideoInterlace_Progressive),"GPU progressive type");
        check(allocator_->InitializeSampleAllocatorEx(4,6,attributes.Get(),type.Get()),"Bounded GPU sample pool");
    }
    IMFDXGIDeviceManager* manager()const{return manager_.Get();}
    std::shared_ptr<GpuFrame> convert(ID3D11Texture2D* source){
        if(!source)throw Failure(E_POINTER,"GPU source missing");
        D3D11_TEXTURE2D_DESC desc{};source->GetDesc(&desc);
        ComPtr<ID3D11Device> owner;source->GetDevice(&owner);
        if(owner.Get()!=device_.Get()||desc.Width!=width_||desc.Height!=height_||desc.ArraySize!=1||desc.MipLevels!=1||
           desc.SampleDesc.Count!=1||(desc.Format!=DXGI_FORMAT_B8G8R8A8_UNORM&&desc.Format!=DXGI_FORMAT_B8G8R8A8_UNORM_SRGB))
            throw Failure(E_INVALIDARG,"GPU source identity or geometry changed");
        auto frame=std::make_shared<GpuFrame>();
        const auto hr=allocator_->AllocateSample(&frame->allocation);
        if(hr==MF_E_SAMPLEALLOCATOR_EMPTY)return {};
        check(hr,"Allocate retained GPU sample");
        ComPtr<IMFMediaBuffer> buffer;check(frame->allocation->GetBufferByIndex(0,&buffer),"GPU output buffer");
        ComPtr<IMFDXGIBuffer> dxgi;check(buffer.As(&dxgi),"GPU output surface");
        ComPtr<ID3D11Texture2D> output;check(dxgi->GetResource(IID_PPV_ARGS(&output)),"GPU output texture");
        UINT subresource=0;check(dxgi->GetSubresourceIndex(&subresource),"GPU output subresource");
        D3D11_TEXTURE2D_DESC target{};output->GetDesc(&target);
        if(target.Format!=DXGI_FORMAT_NV12||target.Width!=width_||target.Height!=height_||target.MipLevels!=1||subresource>=target.ArraySize)
            throw Failure(E_INVALIDARG,"GPU allocator returned unexpected layout");
        D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC view{};
        if(target.ArraySize>1){view.ViewDimension=D3D11_VPOV_DIMENSION_TEXTURE2DARRAY;view.Texture2DArray.FirstArraySlice=subresource;view.Texture2DArray.ArraySize=1;}
        else view.ViewDimension=D3D11_VPOV_DIMENSION_TEXTURE2D;
        ComPtr<ID3D11VideoProcessorOutputView> outputView;
        check(video_->CreateVideoProcessorOutputView(output.Get(),enumerator_.Get(),&view,&outputView),"GPU output view");
        context_->CopyResource(input_.Get(),source);
        D3D11_VIDEO_PROCESSOR_STREAM stream{};stream.Enable=TRUE;stream.pInputSurface=inputView_.Get();
        check(videoContext_->VideoProcessorBlt(processor_.Get(),outputView.Get(),0,1,&stream),"GPU BGRA to NV12");
        context_->Flush();
        return frame;
    }
};
}
