#pragma once
#include "Camera.h"
#include <setupapi.h>
namespace s7camera {
GUID containerForDevice(HDEVINFO set,SP_DEVINFO_DATA& device);
GUID containerForInterface(std::wstring const& path);
}
