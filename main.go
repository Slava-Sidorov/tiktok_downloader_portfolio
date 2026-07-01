package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/schollz/progressbar/v3"
	"github.com/urfave/cli/v2"
)

// speedTestURLs — хосты для замера скорости прокси, пробуются по порядку.
// Первый ответивший даёт замер: если один хост лежит, авто-режим не падает,
// пока прокси живые. Каждый файл ~1 МБ. Через --test-url можно поставить свой
// хост первым в очередь.
var speedTestURLs = []string{
	"https://proof.ovh.net/files/1Mb.dat",
	"https://speed.cloudflare.com/__down?bytes=1048576",
	"http://speedtest.tele2.net/1MB.zip",
}

const maxAttempts = 3

// alreadyInArchiveMarker — сообщение yt-dlp, когда видео уже записано в
// --download-archive: процесс завершается с кодом 0, но реального скачивания
// не было. Используется, чтобы отличить skipped от downloaded.
const alreadyInArchiveMarker = "has already been recorded in the archive"

var (
	downloaded  int64
	skipped     int64
	failed      int64
	verboseMode bool
	fileLogger  *log.Logger

	bar     *progressbar.ProgressBar
	printMu sync.Mutex

	blacklist sync.Map // map[string]struct{} — заблокированные прокси
	bannedN   int32    // атомарный счётчик забаненных прокси (для раннего выхода в nextProxy)
	failCount sync.Map // map[string]*int32 — подряд-фейлы по прокси (мягкий бан)

	failedMu    sync.Mutex
	failedItems []failedItem // ссылки, упавшие после всех попыток (URL + причина)
)

// failedItem — упавшая ссылка с причиной для failed_reasons.txt.
type failedItem struct {
	url    string
	reason string
}

func safePrint(format string, a ...interface{}) {
	printMu.Lock()
	defer printMu.Unlock()
	if bar != nil {
		bar.Clear()
	}
	fmt.Printf(format, a...)
}

// barAdd продвигает прогресс-бар под printMu — тем же мьютексом, что и safePrint.
// Рендер бара и вывод сообщений идут в один stdout, поэтому должны быть
// сериализованы, иначе строки перемешиваются.
func barAdd() {
	printMu.Lock()
	defer printMu.Unlock()
	if bar != nil {
		bar.Add(1)
	}
}

func logToFile(format string, v ...interface{}) {
	if fileLogger != nil {
		fileLogger.Printf(format, v...)
	}
}

// proxyCredRe матчит userinfo в URL-подобной строке: //user:pass@host.
var proxyCredRe = regexp.MustCompile(`//[^/@\s]+@`)

// maskProxyCreds прячет логин/пароль прокси в произвольном тексте (наш лог или
// stderr yt-dlp), заменяя //user:pass@ на //***@. Нужно, чтобы креды прокси не
// оседали открытым текстом в консоли и в файле --log.
func maskProxyCreds(s string) string {
	return proxyCredRe.ReplaceAllString(s, "//***@")
}

// buildTestURLs собирает список хостов для замера в порядке приоритета:
// разовый --test-url (если задан) → постоянные хосты из конфига (`test-host`)
// → встроенные fallback-и. Дубликаты убираются, порядок сохраняется.
func buildTestURLs(custom string) []string {
	var urls []string
	if custom != "" {
		urls = append(urls, custom)
	}
	urls = append(urls, loadExtraTestHosts()...)
	urls = append(urls, speedTestURLs...)

	seen := make(map[string]bool, len(urls))
	out := urls[:0]
	for _, u := range urls {
		if seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

// testHostsConfigPath — путь к файлу с постоянными тест-хостами
// (%AppData%/tiktok-downloader/test-hosts.txt и аналоги на других ОС).
func testHostsConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tiktok-downloader", "test-hosts.txt"), nil
}

// loadExtraTestHosts читает постоянные тест-хосты. Отсутствие файла — не ошибка
// (обычная ситуация до первого `test-host`).
func loadExtraTestHosts() []string {
	path, err := testHostsConfigPath()
	if err != nil {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var hosts []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		hosts = append(hosts, line)
	}
	return hosts
}

// addTestHost добавляет хост в постоянный список. Возвращает путь конфига при
// фактической записи или ("", nil), если хост там уже есть.
func addTestHost(rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", fmt.Errorf("пустой URL")
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("нужна полная http/https ссылка, получено: %s", rawURL)
	}

	for _, h := range loadExtraTestHosts() {
		if h == rawURL {
			return "", nil // уже есть — не дублируем
		}
	}

	path, err := testHostsConfigPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, rawURL); err != nil {
		return "", err
	}
	return path, nil
}

// loadProxies читает файл прокси: пустые строки и комментарии (#) пропускаются,
// невалидные строки — с предупреждением (креды в нём маскируются). Каждая
// валидная строка нормализуется через parseProxy под заданную схему.
func loadProxies(path, scheme string) ([]string, error) {
	pf, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer pf.Close()

	var proxies []string
	sc := bufio.NewScanner(pf)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parsed, err := parseProxy(line, scheme)
		if err != nil {
			log.Printf("⚠️ Пропускаю невалидный прокси %q: %v", maskProxyCreds(line), err)
			continue
		}
		proxies = append(proxies, parsed)
	}
	return proxies, nil
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

func testProxySpeed(ctx context.Context, rawProxy, scheme string, testURLs []string) (float64, error) {
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

	// Пробуем хосты по порядку: падение одного хоста не должно ронять замер,
	// пока сам прокси живой.
	var lastErr error
	for _, testURL := range testURLs {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		speed, err := measureDownload(ctx, client, testURL)
		if err != nil {
			lastErr = err
			continue
		}
		return speed, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("нет тестовых хостов")
	}
	return 0, lastErr
}

// measureDownload качает один тестовый файл через client и возвращает скорость в МБ/с.
func measureDownload(ctx context.Context, client *http.Client, testURL string) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
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
	return float64(written) / 1024 / 1024 / duration, nil
}

// targetWorking — сколько рабочих прокси достаточно найти, чтобы прекратить
// замер досрочно (в выборочном режиме).
const targetWorking = 10

// sampleProxies возвращает случайную выборку для теста: max(ceil(10%), 5),
// но не больше самого пула. Пул перемешивается, оригинал не мутируется.
func sampleProxies(proxies []string) []string {
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
	return shuffled[:maxToTest]
}

func runProxyTest(ctx context.Context, proxies []string, scheme string, testURLs []string) (float64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sample := sampleProxies(proxies)

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

			speed, err := testProxySpeed(ctx, proxy, scheme, testURLs)
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

// proxyCheckResult — результат проверки одного прокси для сабкоманды check-proxies.
type proxyCheckResult struct {
	proxy string
	speed float64
	err   error
}

// runProxyCheck замеряет скорость прокси и печатает таблицу: живые сверху по
// убыванию скорости, мёртвые снизу. Ничего не качает и не банит.
//
// По умолчанию (all=false) поведение как в основном коде: случайная выборка
// ~10% пула, ранняя остановка при targetWorking рабочих. С all=true проверяется
// весь пул.
func runProxyCheck(ctx context.Context, proxies []string, scheme string, testURLs []string, all bool) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	toTest := proxies
	if !all {
		toTest = sampleProxies(proxies)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []proxyCheckResult
		found   int32
	)
	sem := make(chan struct{}, 10)

	for _, p := range toTest {
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

			speed, err := testProxySpeed(ctx, proxy, scheme, testURLs)
			mu.Lock()
			results = append(results, proxyCheckResult{proxy: proxy, speed: speed, err: err})
			mu.Unlock()

			// Выборочный режим: набрали достаточно рабочих — останавливаем остальные.
			if err == nil && !all && atomic.AddInt32(&found, 1) >= targetWorking {
				cancel()
			}
		}(p)
	}
	wg.Wait()

	// Отменённые досрочной остановкой прокси не мёртвые — их просто не
	// доуспели проверить, поэтому в таблицу они не попадают.
	filtered := results[:0]
	for _, r := range results {
		if r.err != nil && errors.Is(r.err, context.Canceled) {
			continue
		}
		filtered = append(filtered, r)
	}
	results = filtered

	// Живые первыми, по убыванию скорости; мёртвые — в конец.
	sort.Slice(results, func(i, j int) bool {
		li, lj := results[i].err == nil, results[j].err == nil
		if li != lj {
			return li
		}
		return results[i].speed > results[j].speed
	})

	working := 0
	var sum float64
	for _, r := range results {
		if r.err == nil {
			working++
			sum += r.speed
			fmt.Printf("✅ %-50s %6.2f МБ/с\n", maskProxyCreds(r.proxy), r.speed)
		} else {
			fmt.Printf("❌ %-50s %s\n", maskProxyCreds(r.proxy), maskProxyCreds(r.err.Error()))
		}
	}

	scope := fmt.Sprintf("проверено %d из %d", len(results), len(proxies))
	if all {
		scope = fmt.Sprintf("весь пул %d", len(proxies))
	}
	fmt.Printf("\n🏁 Живых: %d/%d (%s)", working, len(results), scope)
	if working > 0 {
		fmt.Printf(" | Средняя скорость: %.2f МБ/с", sum/float64(working))
	}
	fmt.Println()
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

func dedupKey(rawURL string) string {
	if id := extractTikTokID(rawURL); id != "" {
		return "id:" + id
	}
	if u, err := url.Parse(rawURL); err == nil {
		u.RawQuery = ""
		u.Fragment = ""
		return "url:" + u.String()
	}
	return "raw:" + rawURL
}

// nextProxy возвращает следующий не заблокированный прокси по кругу.
// Если все прокси в блэклисте — возвращает ("", false).
func nextProxy(proxies []string, idx *int32) (string, bool) {
	if int(atomic.LoadInt32(&bannedN)) >= len(proxies) {
		return "", false // все мертвы — не крутим холостые круги
	}
	for range proxies {
		i := int(uint32(atomic.AddInt32(idx, 1)) % uint32(len(proxies)))
		p := proxies[i]
		if _, banned := blacklist.Load(p); !banned {
			return p, true
		}
	}
	return "", false
}

type ytdlpErrKind int

const (
	errVideo ytdlpErrKind = iota // прокси не виноват — видео не оживёт от смены прокси
	errProxy                     // прокси/сеть — всё, что не video
)

const failsToBan = 2 // мягкий бан: после стольких подряд-фейлов прокси банится

// videoMarkers — стабильные сообщения экстрактора yt-dlp: видео недоступно
// независимо от прокси. Стартовый набор по issue yt-dlp; вынесен отдельно,
// чтобы легко дополнять по реальным логам TikTok-экстрактора.
var videoMarkers = []struct{ marker, reason string }{
	{"video unavailable", "видео недоступно"},
	{"this video is not available", "видео недоступно"},
	{"this post is not available", "пост недоступен"},
	{"private", "приватное видео"},
	{"has been removed", "видео удалено"},
	{"not available in your country", "geo-блокировка"},
	{"http error 404", "404 — не найдено"},
	{"unsupported url", "неподдерживаемый URL"},
}

// proxyReasons — распознаваемые сетевые/прокси-ошибки для понятного лога.
// На логику бана не влияют: всё, что не video, и так считается errProxy.
var proxyReasons = []struct{ marker, reason string }{
	{"tunnel connection failed: 407", "407 — неверные креды прокси"},
	{"unable to connect to proxy", "прокси не отвечает"},
	{"actively refused", "прокси отклонил соединение"},
	{"connection refused", "соединение отклонено"},
	{"timed out", "таймаут"},
	{"max retries exceeded", "превышены ретраи соединения"},
}

// classifyYtdlpError — стратегия «от обратного»: сначала ищем явные video-маркеры
// (стабильны), иначе любая ошибка трактуется как errProxy.
func classifyYtdlpError(stderr string) (ytdlpErrKind, string) {
	s := strings.ToLower(stderr)
	for _, v := range videoMarkers {
		if strings.Contains(s, v.marker) {
			return errVideo, v.reason
		}
	}
	for _, p := range proxyReasons {
		if strings.Contains(s, p.marker) {
			return errProxy, p.reason
		}
	}
	return errProxy, "сетевая ошибка / прокси"
}

// incFailAndMaybeBan увеличивает счётчик подряд-фейлов прокси и банит его при
// достижении порога. Бан выполняется РОВНО ОДИН раз через blacklist.LoadOrStore —
// даже если два воркера одновременно довели счётчик до порога, bannedN
// инкрементируется только при первом фактическом помещении в блэклист.
func incFailAndMaybeBan(proxy string) {
	v, _ := failCount.LoadOrStore(proxy, new(int32))
	n := atomic.AddInt32(v.(*int32), 1)
	if n >= failsToBan {
		if _, already := blacklist.LoadOrStore(proxy, struct{}{}); !already {
			atomic.AddInt32(&bannedN, 1)
		}
		atomic.StoreInt32(v.(*int32), 0) // чистим счётчик после бана
	}
}

// resetFail обнуляет счётчик подряд-фейлов: успех «оживляет» прокси, иначе
// банились бы и живые прокси, у которых фейлы были не подряд.
func resetFail(proxy string) {
	if v, ok := failCount.Load(proxy); ok {
		atomic.StoreInt32(v.(*int32), 0)
	}
}

// runYtDlp запускает yt-dlp для одной ссылки через указанный прокси.
// Возвращает stdout (для детекта «уже в архиве») и stderr (для классификации
// ошибки) наверх.
func runYtDlp(ctx context.Context, proxy, urlLink, outDir string) (string, string, error) {
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
		"--", // конец опций: ссылка из недоверенного links.txt не должна трактоваться как флаг yt-dlp
		urlLink,
	)

	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	setSysProcAttr(cmd)
	err := cmd.Run()

	stderr := stderrBuf.String()
	if err != nil && verboseMode && len(bytes.TrimSpace(stderrBuf.Bytes())) > 0 {
		masked := maskProxyCreds(stderr)
		safePrint("[%s] yt-dlp stderr:\n%s\n", time.Now().Format("15:04:05"), masked)
		logToFile("yt-dlp stderr: %s", masked)
	}

	return stdoutBuf.String(), stderr, err
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
				barAdd()
				continue
			}

			start := time.Now()
			done := false
			lastReason := "неизвестная причина"

			for attempt := 1; attempt <= maxAttempts; attempt++ {
				proxy, ok := nextProxy(proxies, idx)
				if !ok {
					safePrint("[%s] ⛔ W%d | свободных прокси не осталось\n",
						time.Now().Format("15:04:05"), id)
					logToFile("W%d свободных прокси не осталось", id)
					lastReason = "нет живых прокси"
					break
				}

				stdout, stderr, err := runYtDlp(ctx, proxy, urlLink, outDir)
				if err == nil {
					resetFail(proxy) // прокси жив → обнуляем подряд-фейлы
					if strings.Contains(stdout, alreadyInArchiveMarker) {
						// короткая ссылка (vm.tiktok.com): пред-чек по ID её не поймал,
						// но yt-dlp сверился с архивом по реальному ID и ничего не качал
						atomic.AddInt64(&skipped, 1)
						logToFile("⏭ W%d | уже в архиве | %s", id, urlLink)
					} else {
						atomic.AddInt64(&downloaded, 1)
						logToFile("✅ W%d | %.1fs | %s", id, time.Since(start).Seconds(), urlLink)
					}
					done = true
					break
				}

				kind, reason := classifyYtdlpError(stderr)
				lastReason = reason

				if kind == errVideo {
					// видео не оживёт от смены прокси — прокси не виноват, не ретраим
					break
				}

				// errProxy — +1 подряд-фейл (бан при failsToBan); причина в консоль и лог
				incFailAndMaybeBan(proxy)
				safePrint("[%s] ⚠ W%d | прокси-ошибка (%s), попытка %d/%d\n",
					time.Now().Format("15:04:05"), id, reason, attempt, maxAttempts)
				logToFile("⚠ W%d | прокси-ошибка (%s), попытка %d/%d: %s",
					id, reason, attempt, maxAttempts, urlLink)
			}

			if !done {
				atomic.AddInt64(&failed, 1)
				safePrint("[%s] ❌ W%d | %.1fs | не скачано (%s): %s\n",
					time.Now().Format("15:04:05"), id, time.Since(start).Seconds(), lastReason, urlLink)
				logToFile("❌ W%d | %.1fs | не скачано (%s): %s",
					id, time.Since(start).Seconds(), lastReason, urlLink)
				failedMu.Lock()
				failedItems = append(failedItems, failedItem{url: urlLink, reason: lastReason})
				failedMu.Unlock()
			}

			barAdd()

			if delay > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(delay):
				}
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
			&cli.StringFlag{
				Name:  "test-url",
				Value: "",
				Usage: "Свой хост для замера скорости прокси (пробуется первым, затем встроенные)",
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
			customTestURL := c.String("test-url")

			verboseMode = c.Bool("verbose")

			// Свой хост (если задан) пробуется первым, затем встроенные fallback-и.
			testURLs := buildTestURLs(customTestURL)

			if numWorkers < 0 {
				fmt.Printf("❌ --workers должно быть >= 0 (0 = автоопределение), получено %d\n", numWorkers)
				os.Exit(1)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-sigCh
				safePrint("\n⚠️  Получен сигнал завершения, останавливаем загрузки... (повторный Ctrl+C — принудительный выход)\n")
				cancel()
				<-sigCh // второй сигнал: не ждём, пока зависший yt-dlp отпустит соединение
				safePrint("\n⛔ Принудительный выход\n")
				os.Exit(130) // 128 + SIGINT
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
			proxies, err := loadProxies(fileProxy, scheme)
			if err != nil {
				fmt.Printf("❌ Прокси: %v\n", err)
				os.Exit(1)
			}
			if len(proxies) == 0 {
				fmt.Println("❌ Прокси пусты")
				os.Exit(1)
			}

			if err := os.MkdirAll(outDir, 0755); err != nil {
				fmt.Printf("❌ Не удалось создать папку для загрузок %q: %v\n", outDir, err)
				os.Exit(1)
			}

			// Тест прокси / авто-расчёт воркеров
			recommendedWorkers := numWorkers
			if numWorkers == 0 {
				fmt.Println("🧪 Тестирую прокси и замеряю скорость...")
				avgSpeed, err := runProxyTest(ctx, proxies, scheme, testURLs)
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
				if line == "" {
					continue
				}
				key := dedupKey(line)
				if !seen[key] {
					seen[key] = true
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

			// Инициализация progressbar под printMu: sig-handler читает bar
			// через safePrint (тот же мьютекс), поэтому запись обязана быть под ним.
			printMu.Lock()
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
			printMu.Unlock()

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
			fails := failedItems
			failedMu.Unlock()
			if len(fails) > 0 {
				urlPath := filepath.Join(outDir, "failed.txt")
				reasonPath := filepath.Join(outDir, "failed_reasons.txt")

				// failed.txt — чистый список URL для повторного прогона
				if uf, err := os.Create(urlPath); err != nil {
					fmt.Printf("⚠️ Не удалось записать %s: %v\n", urlPath, err)
				} else {
					for _, it := range fails {
						fmt.Fprintln(uf, it.url)
					}
					uf.Close()
				}

				// failed_reasons.txt — URL + причина (tab-separated)
				if rf, err := os.Create(reasonPath); err != nil {
					fmt.Printf("⚠️ Не удалось записать %s: %v\n", reasonPath, err)
				} else {
					for _, it := range fails {
						fmt.Fprintf(rf, "%s\t%s\n", it.url, it.reason)
					}
					rf.Close()
				}

				fmt.Printf("📄 Упавшие ссылки: %s (%d) | с причинами: %s\n",
					urlPath, len(fails), reasonPath)
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
			{
				Name:  "check-proxies",
				Usage: "Проверить прокси и вывести живые/мёртвые со скоростью (выборку или весь пул с --all; без скачивания)",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "proxies",
						Value: "proxies.txt",
						Usage: "Путь к файлу с прокси",
					},
					&cli.BoolFlag{
						Name:  "socks5",
						Value: false,
						Usage: "Использовать socks5 вместо http для прокси",
					},
					&cli.StringFlag{
						Name:  "test-url",
						Value: "",
						Usage: "Свой хост для замера (пробуется первым, затем встроенные)",
					},
					&cli.BoolFlag{
						Name:  "all",
						Value: false,
						Usage: "Проверить весь пул (по умолчанию — выборка ~10%, до 10 рабочих)",
					},
				},
				Action: func(c *cli.Context) error {
					scheme := "http"
					if c.Bool("socks5") {
						scheme = "socks5"
					}

					proxies, err := loadProxies(c.String("proxies"), scheme)
					if err != nil {
						return fmt.Errorf("прокси: %w", err)
					}
					if len(proxies) == 0 {
						return fmt.Errorf("прокси пусты")
					}

					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					sigCh := make(chan os.Signal, 1)
					signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
					go func() {
						<-sigCh
						cancel()
					}()

					all := c.Bool("all")
					if all {
						fmt.Printf("🧪 Проверяю весь пул: %d прокси...\n\n", len(proxies))
					} else {
						fmt.Printf("🧪 Проверяю выборку из %d прокси...\n\n", len(proxies))
					}
					runProxyCheck(ctx, proxies, scheme, buildTestURLs(c.String("test-url")), all)
					return nil
				},
			},
			{
				Name:      "test-host",
				Usage:     "Добавить хост в постоянный список для замера скорости прокси (не запускает загрузку)",
				ArgsUsage: "<url>",
				Action: func(c *cli.Context) error {
					if c.NArg() != 1 {
						return fmt.Errorf("укажите ровно один URL: tiktok-downloader test-host <url>")
					}
					raw := c.Args().First()
					path, err := addTestHost(raw)
					if err != nil {
						return err
					}
					if path == "" {
						fmt.Printf("ℹ️  Хост уже в списке: %s\n", raw)
					} else {
						fmt.Printf("✅ Хост добавлен: %s\n   Сохранён в %s и будет использоваться при тесте прокси.\n", raw, path)
					}
					return nil
				},
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}
