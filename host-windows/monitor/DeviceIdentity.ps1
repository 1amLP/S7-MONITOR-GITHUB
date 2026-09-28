function Get-S7MonitorDevice {
    Get-PnpDevice -PresentOnly -Class Display -ErrorAction SilentlyContinue | Where-Object {
        $ids=Get-PnpDeviceProperty -InstanceId $_.InstanceId -KeyName DEVPKEY_Device_HardwareIds -ErrorAction SilentlyContinue
        @($ids.Data) -contains 'Root\S7H264Monitor'
    }
}

function Get-S7MonitorDriverFile([string]$InstanceId) {
    if(-not ('S7DriverStore' -as [type])){
        Add-Type -TypeDefinition @'
using System;
using System.Text;
using System.Runtime.InteropServices;
public static class S7DriverStore {
 [DllImport("setupapi.dll",CharSet=CharSet.Unicode,SetLastError=true)]
 public static extern bool SetupGetInfDriverStoreLocationW(string file,IntPtr platform,IntPtr reserved,StringBuilder path,uint size,out uint needed);
}
'@
    }
    $inf=(Get-PnpDeviceProperty -InstanceId $InstanceId -KeyName DEVPKEY_Device_DriverInfPath).Data
    if($inf -notmatch '^oem\d+\.inf$'){throw 'Unexpected S7 published INF name'}
    $path=New-Object Text.StringBuilder 2048
    [uint32]$needed=0
    if(-not [S7DriverStore]::SetupGetInfDriverStoreLocationW((Join-Path "$env:windir\INF" $inf),[IntPtr]::Zero,[IntPtr]::Zero,$path,2048,[ref]$needed)){
        throw "DriverStore lookup failed: $([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
    }
    Join-Path (Split-Path $path.ToString() -Parent) 'S7Monitor.dll'
}
