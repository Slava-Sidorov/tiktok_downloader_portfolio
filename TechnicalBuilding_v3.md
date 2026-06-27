# ТЗ v3: Модернизация TikTok Auto Video Downloader

> **Статус:** Актуальное  
> **Язык:** Go  
> **Версия документа:** 3.0  
> **Дата:** 2026-06-27  
> **Заменяет:** TechnicalBuilding_1.md, TechnicalBuilding_2.md

---

## 1. Контекст и цель

Существующий проект — CLI-утилита на Go для автоматической загрузки видео из TikTok через `yt-dlp`. Ядро (Worker Pool, `calculateWorkers`, `testProxySpeed`) работает стабильно и **не переписывается**.

**Цель:** перевести утилиту из статуса «скрипт под себя» в production-ready инструмент для фриланс-клиентов (RU и EN рынки). Улучшить UX, надёжность, кроссплатформенность и качество работы с прокси.

---

## 2. Задачи

### 2.1 Pre-flight проверки зависимостей

При старте программы, **до любых других действий**, проверить наличие внешних зависимостей через `exec.LookPath`.

```go
tools := map[string]string{
    "yt-dlp": "pip install yt-dlp",
    "ffmpeg":  "https://ffmpeg.org/download.html",
}
for bin, hint := range tools {
    if _, err := exec.LookPath(bin); err != nil {
        fmt.Printf("❌ %s не найден. Установите: %s\n", bin, hint)
        os.Exit(1)
    }
}
```

Если хоть одна зависимость отсутствует — завершить программу с понятным сообщением. Не запускать тест прокси и не читать файлы ссылок.

---

### 2.2 Переход с `flag` на `urfave/cli`

**Зависимость:** `github.com/urfave/cli/v2`

Заменить текущую инициализацию через пакет `flag` на структуру `cli.App`.

#### Глобальные флаги

| Флаг | Тип | Default | Описание |
|---|---|---|---|
| `--links` | string | `links.txt` | Путь к файлу со ссылками |
| `--proxies` | string | `proxies.txt` | Путь к файлу с прокси |
| `--output` | string | `./downloads` | Папка для сохранения видео |
| `--workers` | int | `0` | Кол-во воркеров (0 = автоопределение) |
| `--socks5` | bool | `false` | Использовать socks5 вместо http для прокси |
| `--delay` | duration | `500ms` | Задержка между запросами одного воркера |
| `--log` | string | `""` | Путь к файлу лога (если не указан — только консоль) |
| `--verbose` | bool | `false` | Подробный вывод yt-dlp для каждого видео |

#### Структура `main.go`

```go
app := &cli.App{
    Name:  "tiktok-downloader",
    Usage: "Массовое скачивание видео из TikTok через yt-dlp",
    Flags: []cli.Flag{ /* см. таблицу выше */ },
    Action: func(c *cli.Context) error {
        // вся логика main() переезжает сюда
        return nil
    },
    Commands: []*cli.Command{
        {
            Name:  "update",
            Usage: "Обновить yt-dlp до последней версии",
            Action: func(c *cli.Context) error {
                cmd := exec.Command("yt-dlp", "-U")
                cmd.Stdout = os.Stdout
                cmd.Stderr = os.Stderr
                return cmd.Run()
            },
        },
    },
}
```

---

### 2.3 Умный парсинг прокси (`parseProxy`)

Поддержать четыре формата входной строки. Протокол определяется глобальным флагом `--socks5` (default: `http`). Если строка уже содержит схему — не переопределять.

#### Форматы

| Формат | Пример |
|---|---|
| Готовый URL (pass-through) | `http://alice:secret@1.2.3.4:8080` |
| `USER:PASS@IP:PORT` | `alice:secret@1.2.3.4:8080` |
| `IP:PORT:USER:PASS` | `1.2.3.4:8080:alice:secret` |
| `IP:PORT` | `1.2.3.4:8080` |

#### Логика разбора (приоритетный порядок)

```go
func parseProxy(raw, scheme string) (string, error) {
    raw = strings.TrimSpace(raw)

    // 1. Уже есть схема — пропустить как есть
    if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "socks5://") {
        return raw, nil
    }

    // 2. Формат USER:PASS@IP:PORT
    if strings.Contains(raw, "@") {
        return scheme + "://" + raw, nil
    }

    parts := strings.Split(raw, ":")

    switch len(parts) {
    case 2:
        // Формат IP:PORT
        return fmt.Sprintf("%s://%s:%s", scheme, parts[0], parts[1]), nil
    case 4:
        // Формат IP:PORT:USER:PASS
        return fmt.Sprintf("%s://%s:%s@%s:%s", scheme, parts[2], parts[3], parts[0], parts[1]), nil
    default:
        return "", fmt.Errorf("неизвестный формат прокси: %s", raw)
    }
}
```

#### Правила чтения файла прокси

- Пустые строки — игнорировать.
- Строки начинающиеся с `#` — игнорировать (комментарии).
- Невалидные строки — пропускать с выводом предупреждения, не крашить программу.

---

### 2.4 Умное тестирование прокси

Выполняется при старте, **только если `--workers` не задан вручную**. Цель — найти рабочие прокси и вычислить среднюю скорость для `calculateWorkers`.

#### Алгоритм

```
1. Перемешать весь список прокси (math/rand.Shuffle)
2. max_to_test = max(ceil(len(proxies) * 0.10), 5)
3. Взять первые max_to_test прокси из перемешанного списка
4. Запустить 5 горутин-тестировщиков (семафор chan struct{})
5. Атомарный счётчик found — при достижении 10 вызвать cancel()
6. Собрать скорости всех рабочих прокси
7. avgSpeed = sum(speeds) / len(speeds)
8. Передать avgSpeed в calculateWorkers
```

#### Hard fail при 0 рабочих

Если ни один прокси из выборки не прошёл тест:

```
❌ Ни один прокси из выборки не работает.
   Перезапустите программу для повторного теста
   или проверьте валидность прокси в файле.
```

Завершить программу с `os.Exit(1)`.

#### Адаптация `testProxySpeed`

Функция должна принимать `context.Context` для корректной отмены при достижении лимита:

```go
func testProxySpeed(ctx context.Context, rawProxy, scheme string) (float64, error)
```

URL тестового файла вынести в константу:

```go
const speedTestURL = "https://proof.ovh.net/files/1Mb.dat"
```

---

### 2.5 Стратегия прокси при скачивании: ленивый блэклист + retry

#### Блэклист

Хранить заблокированные прокси в потокобезопасной структуре:

```go
var blacklist sync.Map // map[string]struct{}
```

Функция выбора следующего прокси пропускает заблокированные:

```go
func nextProxy(proxies []string, idx *int32) (string, bool) {
    for range proxies {
        i := int(atomic.AddInt32(idx, 1)) % len(proxies)
        p := proxies[i]
        if _, banned := blacklist.Load(p); !banned {
            return p, true
        }
    }
    return "", false // все прокси в блэклисте
}
```

Если yt-dlp вернул ошибку — добавить использованный прокси в блэклист:

```go
blacklist.Store(proxy, struct{}{})
```

Если `nextProxy` не нашёл ни одного свободного — завершить загрузку с ошибкой.

#### Retry упавших ссылок

Каждая ссылка получает максимум **3 попытки** (первая + 2 retry) на разных прокси (блэклист гарантирует смену прокси).

```go
const maxAttempts = 3

for attempt := 1; attempt <= maxAttempts; attempt++ {
    proxy, ok := nextProxy(proxies, &proxyIdx)
    if !ok {
        // все прокси мертвы
        break
    }
    err := runYtDlp(ctx, proxy, urlLink, outDir)
    if err == nil {
        // успех
        break
    }
    blacklist.Store(proxy, struct{}{})
}
```

После 3 неудачных попыток — записать ссылку в `failed.txt`.

---

### 2.6 Прогресс-бар и логирование

**Зависимость:** `github.com/schollz/progressbar/v3`

#### Что убрать

- Функцию `safeLog` — удалить.
- `sync.Mutex logMutex` — удалить.

#### Инициализация прогресс-бара

Файл ссылок читать **полностью в память** до запуска воркеров — чтобы знать `totalVideos` для прогресс-бара.

```go
bar := progressbar.NewOptions(totalVideos,
    progressbar.OptionSetDescription("Загрузка видео"),
    progressbar.OptionShowCount(),
    progressbar.OptionSetTheme(progressbar.Theme{
        Saucer: "=", SaucerHead: ">", SaucerPadding: " ",
        BarStart: "[", BarEnd: "]",
    }),
)
```

После каждого завершённого видео (успех, пропуск или окончательная ошибка) вызывать `bar.Add(1)`.

#### Вывод ошибок без поломки бара

Нельзя использовать `fmt.Println` напрямую — сломает отрисовку. Использовать:

```go
bar.Clear()
fmt.Printf("[%s] ❌ Ошибка: %v\n", time.Now().Format("15:04:05"), err)
```

#### Лог в файл

Лог пишется **только при наличии флага `--log`**. Реализовать через `log.New` — он потокобезопасен:

```go
var fileLogger *log.Logger

if logPath != "" {
    f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
    if err != nil { /* handle */ }
    defer f.Close()
    fileLogger = log.New(f, "", log.LstdFlags) // LstdFlags добавляет дату и время
}

func logToFile(format string, v ...interface{}) {
    if fileLogger != nil {
        fileLogger.Printf(format, v...)
    }
}
```

Таймстампы в логе обеспечиваются флагом `log.LstdFlags` (формат: `2009/11/10 23:00:00`).

---

### 2.7 Три счётчика статистики

Заменить текущие два счётчика (`success`, `failed`) на три:

```go
var (
    downloaded int64 // реально скачано
    skipped    int64 // уже существовало (--no-overwrites)
    failed     int64 // окончательная ошибка после всех попыток
)
```

yt-dlp при `--no-overwrites` завершается с кодом 0 и выводит в stderr строку, содержащую `has already been downloaded`. Детектировать это по stderr и инкрементировать `skipped` вместо `downloaded`.

Финальная строка итогов:

```
🏁 Готово | ✅ Скачано: X | ⏭ Пропущено: Y | ❌ Ошибок: Z
```

---

### 2.8 Graceful Shutdown

При нажатии `Ctrl+C` все запущенные процессы `yt-dlp` должны завершиться, новые загрузки не начинаться.

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

go func() {
    <-sigCh
    bar.Clear()
    fmt.Println("\n⚠️  Получен сигнал завершения, останавливаем загрузки...")
    cancel()
}()
```

Заменить `exec.Command` на `exec.CommandContext(ctx, "yt-dlp", ...)` — процесс убьётся автоматически при отмене контекста.

#### Корректный выход из воркера

Текущий цикл `for url := range urls` не реагирует на отмену контекста. Заменить на:

```go
for {
    select {
    case <-ctx.Done():
        return
    case urlLink, ok := <-urls:
        if !ok {
            return
        }
        // логика скачивания
    }
}
```

После завершения всех горутин — записать `failed.txt` и вывести итоговую статистику.

---

### 2.9 Кроссплатформенность: подавление окон cmd.exe на Windows

При запуске `yt-dlp` через `exec.Command` на Windows открывается консольное окно. Решение — build tags.

**`exec_windows.go`**
```go
//go:build windows

package main

import (
    "os/exec"
    "syscall"
)

func setSysProcAttr(cmd *exec.Cmd) {
    cmd.SysProcAttr = &syscall.SysProcAttr{
        HideWindow:    true,
        CreationFlags: 0x08000000, // CREATE_NO_WINDOW
    }
}
```

**`exec_unix.go`**
```go
//go:build !windows

package main

import "os/exec"

func setSysProcAttr(cmd *exec.Cmd) {
    // no-op на Linux/macOS
}
```

Вызывать `setSysProcAttr(cmd)` перед каждым `cmd.Run()` в воркере.

---

### 2.10 `failed.txt`

После завершения всех воркеров, если есть ссылки, которые не удалось скачать после всех попыток — записать их в файл `failed.txt` в директории `--output`.

```go
if len(failedURLs) > 0 {
    path := filepath.Join(outDir, "failed.txt")
    f, _ := os.Create(path)
    defer f.Close()
    for _, u := range failedURLs {
        fmt.Fprintln(f, u)
    }
    fmt.Printf("📄 Список упавших ссылок сохранён: %s\n", path)
}
```

---

## 3. Что не меняется

- Язык: Go
- Инструмент загрузки: `yt-dlp` (внешний процесс)
- Хранение: локальная папка
- Функции `calculateWorkers` и `testProxySpeed` — только расширяются (добавляется `context`, `scheme`), логика не переписывается

---

## 4. Порядок реализации

1. `go get github.com/urfave/cli/v2 github.com/schollz/progressbar/v3`
2. Pre-flight проверки (`yt-dlp`, `ffmpeg`)
3. `parseProxy` с четырьмя форматами и флагом `--socks5`
4. Умный тест прокси (shuffle, 5 горутин, 10 рабочих, hard fail)
5. Build tags: `exec_windows.go` / `exec_unix.go`
6. Переписать `main()` в `cli.App` + субкоманда `update`
7. Чтение всех ссылок в память, инициализация progressbar
8. Ленивый блэклист + retry (3 попытки) + запись `failed.txt`
9. Три счётчика статистики (детект пропущенных по stderr)
10. Graceful Shutdown: context + select в воркере
11. Лог в файл через `log.New` с таймстампами (если `--log` задан)
12. `go build ./...` + ручное тестирование

---

## 5. Критерии готовности

- [ ] При отсутствии `yt-dlp` или `ffmpeg` — понятное сообщение с инструкцией, выход
- [ ] `flag` полностью удалён, все параметры через `urfave/cli`
- [ ] `tiktok-downloader update` запускает `yt-dlp -U` и транслирует вывод
- [ ] Все четыре формата прокси парсятся корректно
- [ ] `--socks5` переключает протокол для всех прокси без явной схемы
- [ ] Комментарии (`#`) и пустые строки в файле прокси игнорируются
- [ ] Тест прокси: рандомная выборка, 5 горутин, стоп при 10 рабочих
- [ ] Hard fail с понятным сообщением если 0 рабочих прокси
- [ ] Дохлые прокси попадают в блэклист и не переиспользуются
- [ ] Каждая ссылка получает максимум 3 попытки на разных прокси
- [ ] Окончательно упавшие ссылки записываются в `failed.txt`
- [ ] Прогресс-бар отображается корректно при параллельных загрузках
- [ ] Ошибки выводятся без поломки прогресс-бара
- [ ] Три счётчика: скачано / пропущено / ошибок — все корректны
- [ ] `Ctrl+C` завершает все дочерние процессы `yt-dlp`, нет зомби
- [ ] Воркер реагирует на отмену контекста через `select`
- [ ] Окна `cmd.exe` не всплывают на Windows
- [ ] `go build ./...` — без ошибок на Windows и Linux/macOS
- [ ] При флаге `--log` пишется файл с таймстампами; без флага — не пишется

---

## 6. Заметки

- Этот файл — единственный источник истины по требованиям. TechnicalBuilding_1.md и TechnicalBuilding_2.md считать устаревшими.
- Перед рефакторингом сохранить текущий `main.go` как `main_legacy.go` или в ветке `legacy`.
- Если требование конфликтует с кодовой базой — зафиксировать здесь и уточнить у владельца.
- Изменения добавлять с датой и пометкой `[обновлено YYYY-MM-DD]`.
