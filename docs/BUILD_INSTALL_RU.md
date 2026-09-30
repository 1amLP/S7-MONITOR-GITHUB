# Сборка и установка

Этот порядок относится только к [проверенной модели](COMPATIBILITY_RU.md).
Готовых прошивок и драйверов в Git нет. Не прошивайте BOOT от другого
аппарата и не отключайте проверку подписи Windows.

## 1. Подготовить входы и откат

Нужны Linux/WSL с Go 1.23 и Python 3, Windows 11 x64 с MSVC 2022, SDK/WDK,
PowerShell 7.2, TWRP и законно полученные файлы своего SM-G930F.
Сохраните исходный BOOT и RECOVERY вне телефона. Перед любой записью
сверьте хеши BOOT, kernel и DTB из [совместимости](COMPATIBILITY_RU.md).
`tools/native_boot.py` намеренно отказывает при несовпадении. Каталоги
`hardware/firmware`, `hardware/sensorhub`, `hardware/mediacodec`,
`hardware/gpu` и `hardware/fonts` заполняются локально из разрешённых
источников; в Git они игнорируются. Для GPU, sensor hub и MediaCodec
сборочные процедуры находятся в `tools/build_*.py`.

Серийный номер телефона здесь не записан. Во всех командах ниже `<SERIAL>`
означает **серийник вашего телефона из `/proc/cmdline`**, 18 шестнадцатеричных
символов. Не добавляйте его в коммит, issue или снимок терминала.

## 2. Собрать код телефона

Из каталога `native`:

```sh
go build -trimpath -buildvcs=false \
  -ldflags '-X perimode/native/internal/appliance.PinnedSerial=<SERIAL>' \
  -o <OUTPUT_OUTSIDE_REPO>/s7-native ./cmd/s7-native
```

Пустой `PinnedSerial` безопасно запрещает привязку к USB. Сборка без `-X`
подходит только для проверки компиляции, не для установки.

## 3. Собрать Windows-компоненты

`host-windows/Build-Package.ps1` создаёт **неподписанный** кандидат. Передайте
свои каталоги MSVC/SDK/WDK, новый выходной каталог вне Git и `-DeviceSerial
<SERIAL>`. Скрипт собирает Monitor, Camera и диагностический FPS-инструмент.
Ни сертификат, ни частный ключ, ни обход политики Windows здесь не создаются.

Кандидат и `S7Setup.exe`/`S7PackageCheck.exe` нужно подписать уже доверенным
издателем. Затем `host-windows/Seal-Candidate.ps1` проверяет и фиксирует
точные байты. `host-windows/Build-PhonePackage.ps1` принимает
`-DeviceSerial <SERIAL>`, `-PublisherThumbprint <THUMBPRINT>`, `-Release
<NUMBER>` и подписанные входы. Серийник включается только в локальный
подписанный пакет. `host-windows/Build-InstallerMedia.ps1` и
`tools/build_install_media.py` создают read-only установочный диск и его
`PHONE_PACKAGE.json` вне репозитория.

## 4. Собрать BOOT

После подготовки каталогов `hardware` и локального `PHONE_PACKAGE.json`:

```sh
python3 tools/native_boot.py \
  --input-boot <OWN_MATCHING_BOOT.img> \
  --init <OUTPUT_OUTSIDE_REPO>/s7-native \
  --firmware-dir hardware/firmware \
  --output-dir <NEW_OUTPUT_OUTSIDE_REPO> \
  --direct-mfc --windows-package-pin <PHONE_PACKAGE.json>
```

Проверьте `BOOT_BUILD.json`: `kernel_and_dtb_unchanged=true`, исходный и
выходной SHA-256, размер BOOT 41943040 байт и совпадение package pin.
Сборщик не прошивает устройство. Собранный BOOT содержит полученные вами
firmware-входы и поэтому не должен попадать в GitHub.

Для исправленного DWC3 ядра добавьте `--kernel-image <PINNED_KERNEL_IMAGE>`
и `--kernel-pin hardware/source-config/kernel-usb-wakeup.json`. Сборщик
проверяет исходный commit, конфигурацию, SHA-256, ARM64 header и строку
kernel release. В отчёте должно быть `dtb_unchanged=true`; поле
`kernel_and_dtb_unchanged` в этом случае равно `false`.
В Git есть патч и пин, но нет бинарного ядра. Другая самостоятельная сборка
может иметь другой хеш: существующий пин не подтверждает её совместимость.
Не заменяйте хеши только ради обхода защиты.
Исходное ядро сохранено для сравнения и отката. В нём воспроизведён panic
при отсутствующем `resume` callback; его не считать исправленным вариантом.

## 5. Установить на проверенный S7

Переведите телефон в TWRP. Сверьте серийник, хеш текущих разделов BOOT и
RECOVERY с ожидаемыми. Поместите новый BOOT, проверенную копию старого BOOT
и `tools/Install-NativePersistent.sh` в новый каталог TWRP RAM. Скрипт
принимает `--install-native <NEW_BOOT_SHA256> <OLD_BOOT_SHA256> <SERIAL>`;
он отказывает при отличии текущего BOOT/RECOVERY, сохраняет старый BOOT,
читает обратно запись и только затем перезагружает телефон. Без отдельной
проверенной резервной копии эту команду не запускать.

Для нового подписанного пакета ПК `tools/Install-PhonePackage.sh` в TWRP
принимает `--stage-signed-package <NEW_ZIP_SHA256> <OLD_ZIP_SHA256|none>
<NEW_MEDIA_SHA256> <OLD_MEDIA_SHA256|none> <SERIAL>`. Это **отдельная запись
на SYSTEM**. Новая версия этого сценария ещё не прошла сквозную установку;
не применяйте её к действующему телефону без отдельной проверки и отката.

После загрузки телефона откройте установочный USB-диск только кнопкой
`Device > Install Driver` в меню PeriMode. На Windows запустите `S7Setup.exe`
с этого диска. Сначала проверьте монитор, обе камеры, Zoom, Audio/Mic и
Sniper Touch. Статус и нерешённые пункты: [STATUS_RU.md](STATUS_RU.md).

## GitHub Desktop

Добавьте свой локальный каталог PeriMode через **File > Add Local Repository**.
После нашего проверенного коммита используйте **Publish repository**,
имя **PeriMode**, отметьте **Keep this code private**. Не добавляйте в
публикацию локальные `hardware`, BOOT, логи, снимки, ключи и пакеты.
