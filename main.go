package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	logMutex    sync.Mutex
	success     int64
	failed      int64
	verboseMode bool
	logFile     *os.File
)

func safeLog(format string, v ...interface{}) {
	logMutex.Lock()
	defer logMutex.Unlock()
	msg := fmt.Sprintf(format+"\n", v...)
	fmt.Print(msg)
	if logFile != nil {
		logFile.WriteString(msg)
	}
}

func logWorkerOutput(workerId int, output []byte, success bool) {
	if !verboseMode || len(bytes.TrimSpace(output)) == 0 {
		return
	}
	
	logMutex.Lock()
	defer logMutex.Unlock()
	
	msg := fmt.Sprintf("\n=== W%d OUTPUT ===\n%s=================\n", workerId, string(output))
	fmt.Print(msg)
	if logFile != nil {
		logFile.WriteString(msg)
	}
}

func normalizeProxy(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "http://") && !strings.HasPrefix(p, "socks5://") {
		return "http://" + p
	}
	return p
}

func testProxySpeed(rawProxy string) (float64, error) {
	proxyURL := normalizeProxy(rawProxy)
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

	// Пробуем скачать маленький файл
	testFile := "https://proof.ovh.net/files/1Mb.dat"

	start := time.Now()
	resp, err := client.Get(testFile)
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
	} // защита от деления на 0

	speedMBps := float64(written) / 1024 / 1024 / duration
	return speedMBps, nil
}

func calculateWorkers(speedMBps float64, videoSizeMB float64) int {
	if speedMBps <= 0 || videoSizeMB <= 0 {
		return 1
	}
	targetTime := 10.0 // Целевое время в секундах (чуть больше для медленных прокси)
	workers := (speedMBps * targetTime) / videoSizeMB
	w := int(workers)
	if w < 1 {
		w = 1
	}
	if w > 20 {
		w = 20
	} // Лимит безопасности
	return w
}

func worker(id int, urls <-chan string, proxies []string, idx *int32, delay time.Duration, outDir string, wg *sync.WaitGroup) {
	defer wg.Done()

	for urlLink := range urls {
		i := atomic.AddInt32(idx, 1)
		proxy := normalizeProxy(proxies[int(i)%len(proxies)])

		safeLog("🔹 W%d | %s", id, proxy)

		var stdoutBuf, stderrBuf bytes.Buffer
		
		cmd := exec.Command("yt-dlp",
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
			urlLink,
		)
		
		// Захватываем вывод в буфер вместо прямого вывода в консоль
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stderrBuf

		start := time.Now()
		err := cmd.Run()
		
		// Логируем вывод только если есть ошибка или включен verbose режим
		if err != nil {
			logWorkerOutput(id, stderrBuf.Bytes(), false)
		}
		
		if err != nil {
			atomic.AddInt64(&failed, 1)
			safeLog("❌ W%d | %.1fs", id, time.Since(start).Seconds())
		} else {
			atomic.AddInt64(&success, 1)
			safeLog("✅ W%d | %.1fs", id, time.Since(start).Seconds())
		}

		if delay > 0 {
			time.Sleep(delay)
		}
	}
}

func main() {
	fileLinks := flag.String("f", "links.txt", "Файл со ссылками")
	fileProxy := flag.String("p", "proxies.txt", "Файл с прокси")
	videoSize := flag.Float64("size", 15.0, "Примерный размер видео в МБ")
	workers := flag.Int("w", 0, "Кол-во воркеров (0 = авто-расчёт)")
	delay := flag.Duration("d", 500*time.Millisecond, "Мин. задержка")
	outDir := flag.String("o", "downloads", "Папка для скачивания")
	logPath := flag.String("log", "", "Путь к файлу лога (по умолчанию только консоль)")
	verbose := flag.Bool("v", false, "Подробный вывод (логи yt-dlp для каждого видео)")
	flag.Parse()

	verboseMode = *verbose

	// Открываем файл лога если указан
	if *logPath != "" {
		var err error
		logFile, err = os.OpenFile(*logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Printf("❌ Не удалось открыть лог-файл: %v\n", err)
			os.Exit(1)
		}
		defer logFile.Close()
		safeLog("📝 Логи пишутся в файл: %s", *logPath)
	}

	// Читаем прокси
	pf, err := os.Open(*fileProxy)
	if err != nil {
		fmt.Printf("❌ Прокси: %v\n", err)
		os.Exit(1)
	}
	var proxies []string
	sc := bufio.NewScanner(pf)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			proxies = append(proxies, line)
		}
	}
	pf.Close()
	if len(proxies) == 0 {
		fmt.Println("❌ Прокси пусты")
		os.Exit(1)
	}

	// Тест скорости (только если не задан -w вручную)
	recommendedWorkers := *workers
	if *workers == 0 {
		fmt.Println("🧪 Тестирую скорость через первый прокси...")
		speed, err := testProxySpeed(proxies[0])
		if err != nil {
			fmt.Printf("⚠️ Тест не прошёл (%v). Ставлю 1 воркер.\n", err)
			recommendedWorkers = 1
		} else {
			fmt.Printf("✅ Скорость: %.2f МБ/с\n", speed)
			recommendedWorkers = calculateWorkers(speed, *videoSize)
			fmt.Printf("🔧 Авто-расчёт: %d воркеров (видео ~%.1f МБ)\n", recommendedWorkers, *videoSize)
		}
	} else {
		fmt.Printf("⚙️ Ручной режим: %d воркеров (тест скорости пропущен)\n", *workers)
	}

	// Читаем ссылки
	uf, err := os.Open(*fileLinks)
	if err != nil {
		fmt.Printf("❌ Ссылки: %v\n", err)
		os.Exit(1)
	}
	urls := make(chan string, recommendedWorkers)
	go func() {
		defer close(urls)
		sc := bufio.NewScanner(uf)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				urls <- line
			}
		}
		uf.Close()
	}()

	os.MkdirAll(*outDir, 0755)

	var wg sync.WaitGroup
	var proxyIdx int32 = -1

	safeLog("🚀 СТАРТ: %d воркеров | Прокси: %d", recommendedWorkers, len(proxies))
	for i := 0; i < recommendedWorkers; i++ {
		wg.Add(1)
		go worker(i, urls, proxies, &proxyIdx, *delay, *outDir, &wg)
	}

	wg.Wait()
	safeLog("🏁 Итог: ✅ %d | ❌ %d", atomic.LoadInt64(&success), atomic.LoadInt64(&failed))
}
