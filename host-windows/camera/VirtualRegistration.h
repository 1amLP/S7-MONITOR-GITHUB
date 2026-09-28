#pragma once
#include "Camera.h"
bool registeredName(wchar_t const* expected);
HRESULT removeCamera(IMFVirtualCamera* camera,wchar_t const* name);
