# Fixed identities only. Read errors are not treated as an absent installation.
$S7ModernClasses=@('{4C850E86-1698-44BF-893E-43ECF61BAAA9}','{14986CDD-1DA2-4A72-B953-914E65934921}')
$S7LegacyClasses=@('{24E18666-E40B-4DC0-A151-64B642D8D7C9}','{D5C7CCFE-5E13-451A-8BDD-E47C2A978B01}')
$S7LegacyIds=@('ROOT\S7RearCamera10','ROOT\S7FrontCamera10')
function Get-S7CameraRegistration([string[]]$Classes) {
 foreach($id in $Classes){
  foreach($base in @('HKLM:\SOFTWARE\Classes\CLSID','HKCU:\SOFTWARE\Classes\CLSID')){
   $key=Join-Path $base $id
   if(Test-Path -LiteralPath $key -ErrorAction Stop){
    $dll=(Get-Item -LiteralPath (Join-Path $key 'InprocServer32') -ErrorAction Stop).GetValue('')
    if(-not $dll){throw "Incomplete camera class: $key"}
    [pscustomobject]@{Class=$id;Key=$key;Dll=[Environment]::ExpandEnvironmentVariables($dll)}
   }
  }
 }
}
function Get-S7LegacyDevices {
 # Both supplied legacy INFs register Camera class. Include its non-present
 # nodes, but do not query hardware properties of every unrelated ROOT device.
 foreach($d in @(Get-PnpDevice -Class Camera -ErrorAction Stop)){
  if($d.InstanceId -notlike 'ROOT\*'){continue}
  $p=Get-PnpDeviceProperty -InstanceId $d.InstanceId -KeyName DEVPKEY_Device_HardwareIds -ErrorAction Stop
  $matched=@($S7LegacyIds|Where-Object {@($p.Data) -contains $_})
  if($matched.Count -gt 1){throw 'Ambiguous camera hardware identity'}
  if($matched.Count -eq 1){
   [pscustomobject]@{InstanceId=$d.InstanceId;HardwareId=$matched[0];Status=$d.Status}
  }
 }
}
function Assert-S7CameraBackend([ValidateSet('modern','legacy')][string]$Backend) {
 if($Backend -eq 'legacy'){
  if(@(Get-S7CameraRegistration $S7ModernClasses).Count){throw 'Modern S7 camera registration exists. Remove it explicitly before installing legacy.'}
 } else {
  if(@(Get-S7CameraRegistration $S7LegacyClasses).Count -or @(Get-S7LegacyDevices).Count){throw 'Legacy S7 camera nodes or classes exist. Remove them explicitly before installing modern.'}
 }
}
