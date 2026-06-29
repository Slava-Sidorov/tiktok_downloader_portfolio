package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/schollz/progressbar/v3"
	"github.com/urfave/cli/v2"
)

const speedTestURL = "https://proof.ovh.net/files/1Mb.dat"

const maxAttempts = 3

var (
	downloaded  int64
	skipped     int64
	failed      int64
	verboseMode bool
	fileLogger  *log.Logger

	bar      *progressbar.ProgressBar
	printMu  sync.Mutex

	blacklist  sync.Map // map[string]struct{} — заблокированные прокси
	failedMu   sync.Mutex
	failedURLs []string // ссылки, упавшие после всех попыток
)

func safePrint(format string, a ...interface{}) {
	printMu.Lock()
	defer printMu.Unlock()
	if bar != nil {
		bar.Clear()
	}
	fmt.Printf(format, a...)
}

func logToFile(format string, v ...interface{}) {
	if fileLogger != nil {
		fileLogger.Printf(format, v...)
	}
}

func parseProxy(raw, scheme string) (string, error) {
	raw = strings.TrimSpace(raw)

	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "socks5://") {
		return raw, nil
	}

	if strings.Contains(raw, "@") {
		return scheme + "://" + raw, nil
	}

	parts := strings.Split(raw, ":")

	switch len(parts) {
	case 2:
		return fmt.Sprintf("%s://%s:%s", scheme, parts[0], parts[1]), nil
	case 4:
		return fmt.Sprintf("%s://%s:%s@%s:%s", scheme, parts[2], parts[3], parts[0], parts[1]), nil
	default:
		return "", fmt.Errorf("неизвестный формат прокси: %s", raw)
	}
}

func testProxySpeed(ctx context.Context, rawProxy, scheme string) (float64, error) {
	proxyURL, err := parseProxy(rawProxy, scheme)
	if err != nil {
		return 0, err
	}
	parsedURL, err := url.Parse(proxyURL)
	if err != nil {
		return 0, err
	}

	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(parsedURL),
		},
		Timeout: 20 * time.Second,
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, speedTestURL, nil)
	if err != nil {
		return 0, err
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	written, err := io.Copy(io.Discard, resp.Body)
	if err != nil {
		return 0, err
	}

	duration := time.Since(start).Seconds()
	if duration < 0.1 {
		duration = 0.1
	}

	speedMBps := float64(written) / 1024 / 1024 / duration
	return speedMBps, nil
}

func runProxyTest(ctx context.Context, proxies []string, scheme string) (float64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	shuffled := make([]string, len(proxies))
	copy(shuffled, proxies)
	rand.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})

	maxToTest := int(math.Ceil(float64(len(shuffled)) * 0.10))
	if maxToTest < 5 {
		maxToTest = 5
	}
	if maxToTest > len(shuffled) {
		maxToTest = len(shuffled)
	}
	sample := shuffled[:maxToTest]

	const targetWorking = 10

	var (
		found  int32
		mu     sync.Mutex
		speeds []float64
		wg     sync.WaitGroup
	)
	sem := make(chan struct{}, 5)

	for _, p := range sample {
		wg.Add(1)
		go func(proxy string) {
			defer wg.Done()

			select {
			case <-ctx.Done():
				return
			case sem <- struct{}{}:
			}
			defer func() { <-sem }()

			if ctx.Err() != nil {
				return
			}

			speed, err := testProxySpeed(ctx, proxy, scheme)
			if err != nil {
				return
			}

			mu.Lock()
			speeds = append(speeds, speed)
			mu.Unlock()

			if atomic.AddInt32(&found, 1) >= targetWorking {
				cancel()
			}
		}(p)
	}

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(speeds) == 0 {
		return 0, fmt.Errorf("❌ Ни один прокси из выборки не работает.\n" +
			"   Перезапустите программу для повторного теста\n" +
			"   или проверьте валидность прокси в файле.")
	}

	var sum float64
	for _, s := range speeds {
		sum += s
	}
	return sum / float64(len(speeds)), nil
}

func calculateWorkers(speedMBps float64, videoSizeMB float64) int {
	if speedMBps <= 0 || videoSizeMB <= 0 {
		return 1
	}
	targetTime := 10.0
	workers := (speedMBps * targetTime) / videoSizeMB
	w := int(workers)
	if w < 1 {
		w = 1
	}
	if w > 20 {
		w = 20
	}
	return w
}

func loadArchive(path string) map[string]bool {
	archive := make(map[string]bool)
	f, err := os.Open(path)
	if err != nil {
		return archive
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) == 2 {
			archive[parts[1]] = true
		}
	}
	return archive
}

func extractTikTokID(rawURL string) string {
	re := regexp.MustCompile(`/video/(\d+)`)
	if m := re.FindStringSubmatch(rawURL); len(m) > 1 {
		return m[1]
	}
	return ""
}

// nextProxy возвращает следующий не заблокированный прокси по кругу.
// Если все прокси в блэклисте — возвращает ("", false).
func nextProxy(proxies []string, idx *int32) (string, bool) {
	for range proxies {
		i := int(atomic.AddInt32(idx, 1)) % len(proxies)
		p := proxies[i]
		if _, banned := blacklist.Load(p); !banned {
			return p, true
		}
	}
	return "", false
}

// runYtDlp запускает yt-dlp для одной ссылки через указанный прокси.
func runYtDlp(ctx context.Context, proxy, urlLink, outDir string) error {
	var stdoutBuf, stderrBuf bytes.Buffer

	cmd := exec.CommandContext(ctx, "yt-dlp",
		"-f", "bestvideo*+bestaudio/best",
		"--merge-output-format", "mp4",
		"-o", filepath.Join(outDir, "%(id)s.%(ext)s"),
		"--proxy", proxy,
		"--socket-timeout", "20",
		"--retries", "2",
		"--concurrent-fragments", "4",
		"--no-overwrites",
		"--no-playlist",
		"--newline",
		"--no-check-certificates",
		"--download-archive", filepath.Join(outDir, ".archive.txt"),
		urlLink,
	)

	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	setSysProcAttr(cmd)
	err := cmd.Run()

	if err != nil && verboseMode && len(bytes.TrimSpace(stderrBuf.Bytes())) > 0 {
		safePrint("[%s] yt-dlp stderr:\n%s\n", time.Now().Format("15:04:05"), stderrBuf.String())
		logToFile("yt-dlp stderr: %s", stderrBuf.String())
	}

	return err
}

func worker(ctx context.Context, id int, urls <-chan string, proxies []string, idx *int32, delay time.Duration, outDir string, archive map[string]bool, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case urlLink, ok := <-urls:
			if !ok {
				return
			}

			// Детект уже скачанного через локальный архив
			if videoID := extractTikTokID(urlLink); videoID != "" && archive[videoID] {
				atomic.AddInt64(&skipped, 1)
				bar.Add(1)
				continue
			}

			start := time.Now()
			done := false

			for attempt := 1; attempt <= maxAttempts; attempt++ {
				proxy, ok := nextProxy(proxies, idx)
				if !ok {
					safePrint("[%s] ⛔ W%d | свободных прокси не осталось\n",
						time.Now().Format("15:04:05"), id)
					logToFile("W%d свободных прокси не осталось", id)
					break
				}

				err := runYtDlp(ctx, proxy, urlLink, outDir)
				if err == nil {
					atomic.AddInt64(&downloaded, 1)
					logToFile("✅ W%d | %.1fs | %s", id, time.Since(start).Seconds(), urlLink)
					done = true
					break
				}

				// упавший прокси — в блэклист, следующая попытка с другим
				blacklist.Store(proxy, struct{}{})
			}

			if !done {
				atomic.AddInt64(&failed, 1)
				safePrint("[%s] ❌ W%d | %.1fs | все попытки исчерпаны: %s\n",
					time.Now().Format("15:04:05"), id, time.Since(start).Seconds(), urlLink)
				logToFile("❌ W%d | %.1fs | все попытки: %s", id, time.Since(start).Seconds(), urlLink)
				failedMu.Lock()
				failedURLs = append(failedURLs, urlLink)
				failedMu.Unlock()
			}

			bar.Add(1)

			if delay > 0 {
				time.Sleep(delay)
			}
		}
	}
}

func main() {
	app := &cli.App{
		Name:  "tiktok-downloader",
		Usage: "Массовое скачивание видео из TikTok через yt-dlp",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "links",
				Value: "links.txt",
				Usage: "Путь к файлу со ссылками",
			},
			&cli.StringFlag{
				Name:  "proxies",
				Value: "proxies.txt",
				Usage: "Путь к файлу с прокси",
			},
			&cli.StringFlag{
				Name:  "output",
				Value: "./downloads",
				Usage: "Папка для сохранения видео",
			},
			&cli.IntFlag{
				Name:  "workers",
				Value: 0,
				Usage: "Кол-во воркеров (0 = автоопределение)",
			},
			&cli.BoolFlag{
				Name:  "socks5",
				Value: false,
				Usage: "Использовать socks5 вместо http для прокси",
			},
			&cli.DurationFlag{
				Name:  "delay",
				Value: 500 * time.Millisecond,
				Usage: "Задержка между запросами одного воркера",
			},
			&cli.StringFlag{
				Name:  "log",
				Value: "",
				Usage: "Путь к файлу лога (если не указан — только консоль)",
			},
			&cli.BoolFlag{
				Name:  "verbose",
				Value: false,
				Usage: "Подробный вывод yt-dlp для каждого видео",
			},
			&cli.Float64Flag{
				Name:  "size",
				Value: 15.0,
				Usage: "Примерный размер видео в МБ",
			},
		},
		Action: func(c *cli.Context) error {
			fileLinks := c.String("links")
			fileProxy := c.String("proxies")
			outDir := c.String("output")
			numWorkers := c.Int("workers")
			useSocks5 := c.Bool("socks5")
			delay := c.Duration("delay")
			logPath := c.String("log")
			videoSize := c.Float64("size")

			verboseMode = c.Bool("verbose")

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-sigCh
				safePrint("\n⚠️  Получен сигнал завершения, останавливаем загрузки...\n")
				cancel()
			}()

			tools := map[string]string{
				"yt-dlp": "pip install yt-dlp",
				"ffmpeg": "https://ffmpeg.org/download.html",
			}
			for bin, hint := range tools {
				if _, err := exec.LookPath(bin); err != nil {
					fmt.Printf("❌ %s не найден. Установите: %s\n", bin, hint)
					os.Exit(1)
				}
			}

			// Лог в файл — только если --log задан
			if logPath != "" {
				f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
				if err != nil {
					fmt.Printf("❌ Не удалось открыть лог-файл: %v\n", err)
					os.Exit(1)
				}
				defer f.Close()
				fileLogger = log.New(f, "", log.LstdFlags)
				fmt.Printf("📝 Логи пишутся в файл: %s\n", logPath)
				logToFile("Запуск tiktok-downloader")
			}

			scheme := "http"
			if useSocks5 {
				scheme = "socks5"
			}

			// Читаем прокси
			pf, err := os.Open(fileProxy)
			if err != nil {
				fmt.Printf("❌ Прокси: %v\n", err)
				os.Exit(1)
			}
			var proxies []string
			sc := bufio.NewScanner(pf)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				parsed, err := parseProxy(line, scheme)
				if err != nil {
					log.Printf("⚠️ Пропускаю невалидный прокси %q: %v", line, err)
					continue
				}
				proxies = append(proxies, parsed)
			}
			pf.Close()
			if len(proxies) == 0 {
				fmt.Println("❌ Прокси пусты")
				os.Exit(1)
			}

			// Тест прокси / авто-расчёт воркеров
			recommendedWorkers := numWorkers
			if numWorkers == 0 {
				fmt.Println("🧪 Тестирую прокси и замеряю скорость...")
				avgSpeed, err := runProxyTest(ctx, proxies, scheme)
				if err != nil {
					fmt.Println(err)
					os.Exit(1)
				}
				fmt.Printf("✅ Средняя скорость рабочих прокси: %.2f МБ/с\n", avgSpeed)
				recommendedWorkers = calculateWorkers(avgSpeed, videoSize)
				fmt.Printf("🔧 Авто-расчёт: %d воркеров (видео ~%.1f МБ)\n", recommendedWorkers, videoSize)
			} else {
				fmt.Printf("⚙️ Ручной режим: %d воркеров (тест скорости пропущен)\n", numWorkers)
			}

			os.MkdirAll(outDir, 0755)

			// Загружаем архив уже скачанных видео
			archive := loadArchive(filepath.Join(outDir, ".archive.txt"))

			// Читаем ссылки в память с дедупликацией
			uf, err := os.Open(fileLinks)
			if err != nil {
				fmt.Printf("❌ Ссылки: %v\n", err)
				os.Exit(1)
			}
			seen := make(map[string]bool)
			var urlList []string
			sc2 := bufio.NewScanner(uf)
			for sc2.Scan() {
				line := strings.TrimSpace(sc2.Text())
				if line != "" && !seen[line] {
					seen[line] = true
					urlList = append(urlList, line)
				}
			}
			uf.Close()

			totalVideos := len(urlList)
			if totalVideos == 0 {
				fmt.Println("❌ Файл ссылок пуст")
				os.Exit(1)
			}
			fmt.Printf("🚀 СТАРТ: %d ссылок | %d воркеров | Прокси: %d\n",
				totalVideos, recommendedWorkers, len(proxies))
			logToFile("Старт: %d ссылок, %d воркеров, %d прокси", totalVideos, recommendedWorkers, len(proxies))

			// Инициализация progressbar
			bar = progressbar.NewOptions(totalVideos,
				progressbar.OptionSetDescription("Загрузка видео"),
				progressbar.OptionShowCount(),
				progressbar.OptionSetTheme(progressbar.Theme{
					Saucer:        "=",
					SaucerHead:    ">",
					SaucerPadding: " ",
					BarStart:      "[",
					BarEnd:        "]",
				}),
			)

			// Канал ссылок
			urlCh := make(chan string, recommendedWorkers)
			go func() {
				defer close(urlCh)
				for _, u := range urlList {
					select {
					case <-ctx.Done():
						return
					case urlCh <- u:
					}
				}
			}()

			var wg sync.WaitGroup
			var proxyIdx int32 = -1

			for i := 0; i < recommendedWorkers; i++ {
				wg.Add(1)
				go worker(ctx, i, urlCh, proxies, &proxyIdx, delay, outDir, archive, &wg)
			}

			wg.Wait()
			bar.Finish()

			fmt.Printf("\n🏁 Готово | ✅ Скачано: %d | ⏭ Пропущено: %d | ❌ Ошибок: %d\n",
				atomic.LoadInt64(&downloaded),
				atomic.LoadInt64(&skipped),
				atomic.LoadInt64(&failed),
			)
			logToFile("Итог: скачано=%d, пропущено=%d, ошибок=%d",
				atomic.LoadInt64(&downloaded), atomic.LoadInt64(&skipped), atomic.LoadInt64(&failed))

			failedMu.Lock()
			fails := failedURLs
			failedMu.Unlock()
			if len(fails) > 0 {
				path := filepath.Join(outDir, "failed.txt")
				f, err := os.Create(path)
				if err != nil {
					fmt.Printf("⚠️ Не удалось записать failed.txt: %v\n", err)
				} else {
					for _, u := range fails {
						fmt.Fprintln(f, u)
					}
					f.Close()
					fmt.Printf("📄 Список упавших ссылок сохранён: %s\n", path)
				}
			}
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

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}
