# oo-agent для Windows 7

Go 1.21+ офіційно не запускається на Windows 7: рантайм бере випадкові числа з
`bcryptprimitives!ProcessPrng` (з'явилась у Windows 10), і на Win7 процес
падає ще до `main` (`Exception 0xc0000005 … PC=0x0` в `asmstdcall`).
Сторонніх збірок Go не беремо — патчимо ОФІЦІЙНИЙ дистрибутив тієї версії,
якою збирається решта агента.

## Що змінено в Go (2 файли, ~20 рядків)
- `runtime-os_windows.patch` — якщо `ProcessPrng` немає, рантайм бере
  `advapi32!SystemFunction036` (RtlGenRandom): той самий контракт `(buf, len) -> BOOLEAN`.
- `zsyscall_windows.patch` — те саме для `crypto/rand` (`internal/syscall/windows.ProcessPrng`).

## Що змінено в агенті (діє й на Win10/11, поведінка там та сама)
- `agent/encode/mft.c` — `MFCreateDXGIDeviceManager`/`MFCreateDXGISurfaceBuffer`
  (Win8+) резолвляться динамічно; без них апаратний D3D11-шлях повертає
  `OOS_ENC_NOHW`, і агент бере програмний H.264 MFT (`mfh264enc.dll` є у Win7).
- `agent/capture/dxgi.c` — режим «лише GDI»: коли нема D3D11 video /
  `IDXGIOutput1` (Desktop Duplication), захоплення — `BitBlt` → BT.709 NV12 на
  CPU, незмінний екран = `OOS_TIMEOUT`. `OO_SCREEN_FORCE_GDI=1` вмикає цей
  режим будь-де (тест `TestGDIOnlyPipeline`, аварійний відкат).

## Збірка
```bash
GO_W7=/d/Claude/caches/go-win7            # копія C:\Program Files\Go (та сама версія!)
cp -r "/c/Program Files/Go/." "$GO_W7/"
cd "$GO_W7" && patch -p1 < <репо>/tools/oo-screen/deploy/go-win7/runtime-os_windows.patch \
            && patch -p1 < <репо>/tools/oo-screen/deploy/go-win7/zsyscall_windows.patch
cd <репо>/tools/oo-screen
GOROOT=$GO_W7 GOTOOLCHAIN=local GOOS=windows CGO_ENABLED=1 \
  $GO_W7/bin/go build -ldflags "-H=windowsgui" -o oo-agent-w7.exe ./agent/cmd/oo-agent
```
Перевірка імпортів: `objdump -p oo-agent-w7.exe | grep MFCreateDXGI` — має бути порожньо.

## Розкочування
У Win7 нема модуля ScheduledTasks, а meshctrl RunCommand на цих ПК мертвий.
`deploy/go-win7/w7deploy.py` (+ `mup.py`, `mshell.py`; exe поруч) кладе exe + XML задачі
файловим тунелем Mesh і виконує `schtasks /create /xml` через термінал агента;
задача та сама (`oo-screen-pilot`: вхід користувача + вартовий кожні 5 хв).

Перевірено 06.10.2026: Myroslava (Win7 SP1, 1920×1080) — живе відео, звук Opus, FEC.
