// oo-encprobe — headless H.264 encoder-MFT enumeration probe.
//
// Не захоплює екран, не потребує сесії користувача чи робочого столу, не
// стрімить — лише перелічує зареєстровані H.264 video-encoder MFT-и й друкує,
// хто з них ловиться прапорцем MFT_ENUM_FLAG_HARDWARE, а хто ні. Мета: на ПК,
// де наш агент упав у софт (Computer, Intel), побачити, чи присутній
// "Intel Quick Sync Video H.264 Encoder MFT", який HARDWARE-only енум міг
// пропустити (Intel часто реєструє його як SYNC/LOCAL без HARDWARE-прапорця).
//
// Вивід — по одному рядку на MFT: PROBE mft="<friendly>" hw=<0|1> d3daware=<0|1>
// плюс підсумок PROBE summary hw_count=N all_count=M. Виходить одразу.
package main

/*
#cgo windows LDFLAGS: -lmfplat -lmfuuid -lole32 -lksuser -lstrmiids
#define COBJMACROS
#include <windows.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mfobjects.h>
#include <mftransform.h>
#include <mferror.h>
#include <stdio.h>
#include <string.h>

#ifndef SAFE_RELEASE
#define SAFE_RELEASE(p) do { if (p) { (p)->lpVtbl->Release(p); (p) = NULL; } } while (0)
#endif

// MF_SA_D3D11_AWARE — не завжди в mingw-заголовках; визначаємо локально
// як static const, щоб не залежати від INITGUID-сховища.
static const GUID OOS_MF_SA_D3D11_AWARE_GUID =
    {0x206b4fc8,0xfcf9,0x4c51,{0xaf,0xe3,0x97,0x64,0x36,0x9e,0x33,0xa0}};

// enum_probe перелічує H.264-encoder MFT-и для заданого набору прапорців і
// друкує кожен. hw=1 означає, що MFT знайдено саме HARDWARE-прапорцем.
static int enum_probe(UINT32 flags, int mark_hw) {
    MFT_REGISTER_TYPE_INFO out = { MFMediaType_Video, MFVideoFormat_H264 };
    IMFActivate **acts = NULL;
    UINT32 count = 0;
    HRESULT hr = MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER,
                           flags | MFT_ENUM_FLAG_SORTANDFILTER,
                           NULL, &out, &acts, &count);
    if (FAILED(hr) || count == 0) { if (acts) CoTaskMemFree(acts); return 0; }
    for (UINT32 i = 0; i < count; i++) {
        char name[256] = {0};
        WCHAR *w = NULL; UINT32 wl = 0;
        if (SUCCEEDED(IMFActivate_GetAllocatedString(acts[i],
                &MFT_FRIENDLY_NAME_Attribute, &w, &wl)) && w) {
            WideCharToMultiByte(CP_UTF8, 0, w, -1, name, sizeof name - 1, NULL, NULL);
            CoTaskMemFree(w);
        }
        UINT32 d3d = 0;
        IMFActivate_GetUINT32(acts[i], &OOS_MF_SA_D3D11_AWARE_GUID, &d3d);
        UINT32 vend = 0; // MFT_ENUM_HARDWARE_VENDOR_ID присутній лише в hw-MFT
        printf("PROBE mft=\"%s\" hw=%d d3daware=%u\n", name, mark_hw, d3d);
        SAFE_RELEASE(acts[i]);
    }
    CoTaskMemFree(acts);
    return (int)count;
}

static int run_probe(char *err, int err_len) {
    HRESULT hr = CoInitializeEx(NULL, COINIT_MULTITHREADED);
    if (FAILED(hr) && hr != RPC_E_CHANGED_MODE) {
        snprintf(err, err_len, "CoInitializeEx hr=0x%08lx", (unsigned long)hr); return -1;
    }
    hr = MFStartup(MF_VERSION, MFSTARTUP_LITE);
    if (FAILED(hr)) { snprintf(err, err_len, "MFStartup hr=0x%08lx", (unsigned long)hr); return -1; }

    int hw = enum_probe(MFT_ENUM_FLAG_HARDWARE, 1);
    // Софт/локальні/sync — те, у що падає агент. Позначаємо hw=0.
    int sw = enum_probe(MFT_ENUM_FLAG_SYNCMFT | MFT_ENUM_FLAG_ASYNCMFT |
                        MFT_ENUM_FLAG_LOCALMFT | MFT_ENUM_FLAG_TRANSCODE_ONLY, 0);
    printf("PROBE summary hw_count=%d nonhw_count=%d\n", hw, sw);
    MFShutdown();
    return 0;
}
*/
import "C"

import (
	"fmt"
	"os"
)

func main() {
	var buf [256]C.char
	rc := C.run_probe(&buf[0], 256)
	if rc != 0 {
		fmt.Printf("PROBE error: %s\n", C.GoString(&buf[0]))
		os.Exit(1)
	}
}
