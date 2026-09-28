#define NOMINMAX
#include <windows.h>
#include <setupapi.h>
#include <cfgmgr32.h>
#include <newdev.h>
#include <softpub.h>
#include <mscat.h>
#include <bcrypt.h>
#include <initguid.h>
#include <devguid.h>
#include <string>
#include <vector>
#include <iostream>
#include <cwctype>
#include <shellapi.h>
#include <array>

namespace {
constexpr wchar_t HardwareId[]=L"Root\\S7H264Monitor";
void check(bool ok){if(!ok)throw GetLastError();}
struct Handle{HANDLE value;~Handle(){if(value!=INVALID_HANDLE_VALUE)CloseHandle(value);}};
struct DeviceSet{HDEVINFO value;~DeviceSet(){if(value!=INVALID_HANDLE_VALUE)SetupDiDestroyDeviceInfoList(value);}};
struct CatAdmin{HCATADMIN value=nullptr;~CatAdmin(){if(value)CryptCATAdminReleaseContext(value,0);}};
std::array<BYTE,20> signer(std::wstring const& path){
    WINTRUST_FILE_INFO file{};file.cbStruct=sizeof(file);file.pcwszFilePath=path.c_str();
    WINTRUST_DATA data{};data.cbStruct=sizeof(data);data.dwUIChoice=WTD_UI_NONE;
    data.dwUnionChoice=WTD_CHOICE_FILE;data.pFile=&file;data.dwStateAction=WTD_STATEACTION_VERIFY;
    GUID policy=WINTRUST_ACTION_GENERIC_VERIFY_V2;
    const LONG result=WinVerifyTrust(nullptr,&policy,&data);
    std::array<BYTE,20> hash{};DWORD bytes=static_cast<DWORD>(hash.size());bool valid=false;
    if(result==ERROR_SUCCESS){
        auto provider=WTHelperProvDataFromStateData(data.hWVTStateData);
        auto signedBy=provider?WTHelperGetProvSignerFromChain(provider,0,FALSE,0):nullptr;
        auto cert=signedBy?WTHelperGetProvCertFromChain(signedBy,0):nullptr;
        valid=cert&&CertGetCertificateContextProperty(cert->pCert,CERT_SHA1_HASH_PROP_ID,hash.data(),&bytes)&&bytes==hash.size();
    }
    data.dwStateAction=WTD_STATEACTION_CLOSE;WinVerifyTrust(nullptr,&policy,&data);
    if(result!=ERROR_SUCCESS)throw static_cast<DWORD>(result);
    if(!valid)throw DWORD(TRUST_E_SUBJECT_NOT_TRUSTED);
    return hash;
}
int installFromMedia(bool elevated){
    wchar_t module[32768]{};const DWORD n=GetModuleFileNameW(nullptr,module,32768);
    if(!n||n>=32768)throw DWORD(ERROR_INVALID_NAME);
    const std::wstring self(module,n),root=self.substr(0,self.find_last_of(L"\\/")+1);
    if(!elevated){
        SHELLEXECUTEINFOW open{sizeof(open)};open.lpVerb=L"runas";open.lpFile=self.c_str();open.lpParameters=L"install-media";open.nShow=SW_SHOWNORMAL;
        check(ShellExecuteExW(&open));return 0;
    }
    const auto publisher=signer(self);
    for(auto name:{L"Install-FromMedia.ps1",L"PackageCommon.ps1",L"PhonePackage.ps1"})
        if(signer(root+name)!=publisher)throw DWORD(TRUST_E_SUBJECT_NOT_TRUSTED);
    wchar_t system[32768]{};const DWORD length=GetSystemDirectoryW(system,32768);
    if(!length||length>=32768)throw DWORD(ERROR_INVALID_NAME);
    const auto shell=std::wstring(system,length)+L"\\WindowsPowerShell\\v1.0\\powershell.exe";
    std::wstring command=L"\""+shell+L"\" -NoProfile -NonInteractive -ExecutionPolicy AllSigned -File \""+root+L"Install-FromMedia.ps1\" -MediaRoot \""+root+L".\"";
    STARTUPINFOW start{sizeof(start)};PROCESS_INFORMATION child{};
    check(CreateProcessW(shell.c_str(),command.data(),nullptr,nullptr,FALSE,CREATE_NEW_CONSOLE,nullptr,nullptr,&start,&child));
    CloseHandle(child.hThread);CloseHandle(child.hProcess);return 0;
}
std::wstring fullPath(wchar_t const* value){
    std::vector<wchar_t> path(32768);DWORD n=GetFullPathNameW(value,static_cast<DWORD>(path.size()),path.data(),nullptr);
    if(!n||n>=path.size())throw DWORD(ERROR_INVALID_NAME);
    const DWORD attrs=GetFileAttributesW(path.data());
    if(attrs==INVALID_FILE_ATTRIBUTES||(attrs&(FILE_ATTRIBUTE_DIRECTORY|FILE_ATTRIBUTE_REPARSE_POINT)))throw DWORD(ERROR_INVALID_DATA);
    return {path.data(),n};
}
void verifyCatalog(std::wstring const& catalog,std::wstring const& member){
    CatAdmin admin;check(CryptCATAdminAcquireContext2(&admin.value,nullptr,BCRYPT_SHA256_ALGORITHM,nullptr,0));
    Handle file{CreateFileW(member.c_str(),GENERIC_READ,FILE_SHARE_READ,nullptr,OPEN_EXISTING,FILE_ATTRIBUTE_NORMAL,nullptr)};
    check(file.value!=INVALID_HANDLE_VALUE);DWORD count=0;
    check(CryptCATAdminCalcHashFromFileHandle2(admin.value,file.value,&count,nullptr,0));
    if(count!=32)throw DWORD(ERROR_INVALID_DATA);
    std::vector<BYTE> hash(count);check(CryptCATAdminCalcHashFromFileHandle2(admin.value,file.value,&count,hash.data(),0));
    constexpr wchar_t hex[]=L"0123456789ABCDEF";std::wstring tag;
    for(auto b:hash){tag.push_back(hex[b>>4]);tag.push_back(hex[b&15]);}
    WINTRUST_CATALOG_INFO entry{};entry.cbStruct=sizeof(entry);entry.pcwszCatalogFilePath=catalog.c_str();
    entry.pcwszMemberTag=tag.c_str();entry.pcwszMemberFilePath=member.c_str();entry.hMemberFile=file.value;
    entry.pbCalculatedFileHash=hash.data();entry.cbCalculatedFileHash=count;entry.hCatAdmin=admin.value;
    GUID policy=WINTRUST_ACTION_GENERIC_VERIFY_V2;
    WINTRUST_DATA data{};data.cbStruct=sizeof(data);data.dwUIChoice=WTD_UI_NONE;
    data.fdwRevocationChecks=WTD_REVOKE_WHOLECHAIN;data.dwUnionChoice=WTD_CHOICE_CATALOG;
    data.pCatalog=&entry;data.dwStateAction=WTD_STATEACTION_VERIFY;data.dwProvFlags=WTD_REVOCATION_CHECK_CHAIN_EXCLUDE_ROOT;
    const LONG result=WinVerifyTrust(nullptr,&policy,&data);
    data.dwStateAction=WTD_STATEACTION_CLOSE;WinVerifyTrust(nullptr,&policy,&data);
    if(result!=ERROR_SUCCESS)throw static_cast<DWORD>(result);
}
bool owns(HDEVINFO set,SP_DEVINFO_DATA& node){
    DWORD type=0,bytes=0;wchar_t raw[4096]{};
    if(!SetupDiGetDeviceRegistryPropertyW(set,&node,SPDRP_HARDWAREID,&type,reinterpret_cast<BYTE*>(raw),sizeof(raw),&bytes)){
        if(GetLastError()==ERROR_INVALID_DATA)return false;throw GetLastError();
    }
    if(type!=REG_MULTI_SZ||bytes>sizeof(raw)||bytes%sizeof(wchar_t))throw DWORD(ERROR_INVALID_DATA);
    const size_t size=bytes/sizeof(wchar_t);
    for(size_t start=0;start<size&&raw[start];){
        size_t end=start;while(end<size&&raw[end])++end;
        if(end==size)throw DWORD(ERROR_INVALID_DATA);
        if(_wcsicmp(raw+start,HardwareId)==0)return true;start=end+1;
    }
    return false;
}
int installMonitor(std::wstring const& inf){
    GUID classId{};wchar_t className[MAX_CLASS_NAME_LEN]{};
    check(SetupDiGetINFClassW(inf.c_str(),&classId,className,MAX_CLASS_NAME_LEN,nullptr));
    if(!IsEqualGUID(classId,GUID_DEVCLASS_DISPLAY))throw DWORD(ERROR_INVALID_DATA);
    DeviceSet all{SetupDiGetClassDevsW(&GUID_DEVCLASS_DISPLAY,nullptr,nullptr,DIGCF_PRESENT)};
    check(all.value!=INVALID_HANDLE_VALUE);unsigned count=0;
    for(DWORD i=0;;++i){
        SP_DEVINFO_DATA node{sizeof(node)};
        if(!SetupDiEnumDeviceInfo(all.value,i,&node)){if(GetLastError()==ERROR_NO_MORE_ITEMS)break;throw GetLastError();}
        if(owns(all.value,node))++count;
    }
    if(count>1)throw DWORD(ERROR_DUP_NAME);
    DeviceSet created{INVALID_HANDLE_VALUE};SP_DEVINFO_DATA node{sizeof(node)};bool registered=false;
    if(!count){
        created.value=SetupDiCreateDeviceInfoList(&classId,nullptr);check(created.value!=INVALID_HANDLE_VALUE);
        check(SetupDiCreateDeviceInfoW(created.value,className,&classId,L"S7 H.264 Monitor",nullptr,DICD_GENERATE_ID,&node));
        constexpr wchar_t ids[]=L"Root\\S7H264Monitor\0";
        check(SetupDiSetDeviceRegistryPropertyW(created.value,&node,SPDRP_HARDWAREID,reinterpret_cast<BYTE const*>(ids),sizeof(ids)));
        SP_DEVINSTALL_PARAMS_W params{sizeof(params)};check(SetupDiGetDeviceInstallParamsW(created.value,&node,&params));
        params.Flags|=DI_QUIETINSTALL;check(SetupDiSetDeviceInstallParamsW(created.value,&node,&params));
        check(SetupDiCallClassInstaller(DIF_REGISTERDEVICE,created.value,&node));registered=true;
    }
    BOOL restart=FALSE;
    if(!UpdateDriverForPlugAndPlayDevicesW(nullptr,HardwareId,inf.c_str(),INSTALLFLAG_FORCE|INSTALLFLAG_NONINTERACTIVE,&restart)){
        const auto error=GetLastError();
        if(registered)SetupDiCallClassInstaller(DIF_REMOVE,created.value,&node);
        throw error;
    }
    return restart?1:0;
}
}
int wmain(int argc,wchar_t** argv){
    try{
        if(argc==1)return installFromMedia(false);
        if(argc==2&&std::wstring(argv[1])==L"install-media")return installFromMedia(true);
        if(argc==4&&std::wstring(argv[1])==L"catalog-check"){
            verifyCatalog(fullPath(argv[2]),fullPath(argv[3]));return 0;
        }
        if(argc==3&&std::wstring(argv[1])==L"monitor-install"){
            const auto inf=fullPath(argv[2]);const auto dir=inf.substr(0,inf.find_last_of(L"\\/")+1);
            verifyCatalog(fullPath((dir+L"S7Monitor.cat").c_str()),inf);
            verifyCatalog(fullPath((dir+L"S7Monitor.cat").c_str()),fullPath((dir+L"S7Monitor.dll").c_str()));
            return installMonitor(inf);
        }
        return 2;
    }catch(DWORD error){std::cerr<<"S7 setup error=0x"<<std::hex<<error<<std::endl;return 2;}
    catch(...){std::cerr<<"S7 setup failed"<<std::endl;return 2;}
}
