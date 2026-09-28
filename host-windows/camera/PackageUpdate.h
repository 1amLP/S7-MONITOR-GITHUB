#pragma once
#include "../monitor/HostWindows.h"

// The update runs in a separate owned process: replacing EndpointSync must not
// make the installer wait for a service thread that is waiting for the installer.
class PhonePackageUpdate {
    s7::Handle process_;
    uint32_t desired_=0,started_=0;
    unsigned attempts_=0;
    ULONGLONG next_=0;
    bool verified_=false;
    HRESULT error_=S_OK;
public:
    void disconnected(){desired_=0;attempts_=0;verified_=false;next_=0;}
    HRESULT tick(uint32_t release){
        if(desired_!=release){desired_=release;attempts_=0;verified_=false;next_=0;}
        if(process_){
            DWORD code=0;if(!GetExitCodeProcess(process_.get(),&code))return HRESULT_FROM_WIN32(GetLastError());
            if(code==STILL_ACTIVE)return HRESULT_FROM_WIN32(ERROR_IO_PENDING);
            if(code==ERROR_INSTALL_ALREADY_RUNNING){process_.reset();if(attempts_)--attempts_;next_=GetTickCount64()+2000;return HRESULT_FROM_WIN32(ERROR_IO_PENDING);}
            process_.reset();verified_=code==0&&started_==desired_;
            error_=code==0?S_OK:HRESULT_FROM_WIN32(ERROR_INSTALL_FAILURE);
            next_=GetTickCount64()+30000;
        }
        if(release==0)return S_OK; // Old firmware has no package delivery contract.
        if(verified_)return S_OK;
        if(attempts_>=3||GetTickCount64()<next_)return FAILED(error_)?error_:HRESULT_FROM_WIN32(ERROR_IO_PENDING);
        ++attempts_;started_=release;
        wchar_t module[32768]{},system[32768]{};
        const DWORD n=GetModuleFileNameW(nullptr,module,32768),m=GetSystemDirectoryW(system,32768);
        if(!n||n>=32768||!m||m>=32768)return HRESULT_FROM_WIN32(ERROR_INVALID_NAME);
        const std::wstring path(module,n);
        const auto script=path.substr(0,path.find_last_of(L"\\/")+1)+L"delivery\\Sync-FromPhone.ps1";
        const auto shell=std::wstring(system,m)+L"\\WindowsPowerShell\\v1.0\\powershell.exe";
        const DWORD attributes=GetFileAttributesW(script.c_str());
        if(attributes==INVALID_FILE_ATTRIBUTES||(attributes&(FILE_ATTRIBUTE_DIRECTORY|FILE_ATTRIBUTE_REPARSE_POINT))){
            error_=HRESULT_FROM_WIN32(ERROR_FILE_NOT_FOUND);next_=GetTickCount64()+30000;return error_;
        }
        std::wstring command=L"\""+shell+L"\" -NoProfile -NonInteractive -ExecutionPolicy AllSigned -File \""+script+L"\" -Release "+std::to_wstring(release);
        STARTUPINFOW startup{sizeof(startup)};PROCESS_INFORMATION child{};
        if(!CreateProcessW(shell.c_str(),command.data(),nullptr,nullptr,FALSE,CREATE_NO_WINDOW,nullptr,nullptr,&startup,&child)){
            error_=HRESULT_FROM_WIN32(GetLastError());next_=GetTickCount64()+30000;return error_;
        }
        CloseHandle(child.hThread);process_.reset(child.hProcess);
        return HRESULT_FROM_WIN32(ERROR_IO_PENDING);
    }
};
