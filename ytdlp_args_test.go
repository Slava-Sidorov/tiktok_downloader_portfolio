package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestYtdlpArgsDefaultVideo фиксирует, что дефолтный cfg (видео через прокси)
// собирает ровно исторический набор флагов в прежнем порядке — регресс-страховка
// для рефакторинга runYtDlp на runConfig.
func TestYtdlpArgsDefaultVideo(t *testing.T) {
	cfg := runConfig{outDir: "out"}
	got := ytdlpArgs(cfg, "http://user:pass@1.2.3.4:8080", "https://example.com/video/1")

	want := []string{
		"-f", "bestvideo*+bestaudio/best",
		"--merge-output-format", "mp4",
		"-o", filepath.Join("out", "%(id)s.%(ext)s"),
		"--proxy", "http://user:pass@1.2.3.4:8080",
		"--socket-timeout", "20",
		"--retries", "2",
		"--concurrent-fragments", "4",
		"--no-overwrites",
		"--no-playlist",
		"--newline",
		"--no-check-certificates",
		"--download-archive", filepath.Join("out", ".archive.txt"),
		"--",
		"https://example.com/video/1",
	}

	assertArgs(t, got, want)
}

// TestYtdlpArgsAudio проверяет аудио-ветку: формат bestaudio, извлечение, шаблон
// имени по title и --windows-filenames.
func TestYtdlpArgsAudio(t *testing.T) {
	cfg := runConfig{outDir: "out", audio: true}
	got := ytdlpArgs(cfg, "http://p", "https://youtu.be/abc")

	joined := strings.Join(got, " ")
	for _, sub := range []string{
		"-f bestaudio/best",
		"-x",
		filepath.Join("out", "%(title)s.%(ext)s"),
		"--windows-filenames",
	} {
		if !strings.Contains(joined, sub) {
			t.Errorf("аудио-режим: ожидался %q в args, получено: %s", sub, joined)
		}
	}
	if strings.Contains(joined, "%(id)s") || strings.Contains(joined, "merge-output-format") {
		t.Errorf("аудио-режим не должен содержать видео-флаги, получено: %s", joined)
	}
}

// TestYtdlpArgsNoProxy: при пустом proxy не добавляются ни --proxy, ни
// --no-check-certificates (TLS-компромисс нужен только для недоверенных прокси).
func TestYtdlpArgsNoProxy(t *testing.T) {
	got := ytdlpArgs(runConfig{outDir: "out"}, "", "https://x/video/1")
	if contains(got, "--proxy") {
		t.Errorf("при пустом proxy не должно быть --proxy, получено: %v", got)
	}
	if contains(got, "--no-check-certificates") {
		t.Errorf("в прямом соединении TLS-проверка должна остаться включённой, получено: %v", got)
	}
}

// TestYtdlpArgsLimitRateAndExtra: limitRate и extraArgs попадают в вызов, когда заданы.
func TestYtdlpArgsLimitRateAndExtra(t *testing.T) {
	cfg := runConfig{
		outDir:    "out",
		limitRate: "5M",
		extraArgs: []string{"--force-ipv4"},
	}
	got := ytdlpArgs(cfg, "http://p", "https://x/video/1")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--limit-rate 5M") {
		t.Errorf("ожидался --limit-rate 5M, получено: %s", joined)
	}
	if !strings.Contains(joined, "--force-ipv4") {
		t.Errorf("ожидался extraArgs --force-ipv4, получено: %s", joined)
	}
	// extraArgs должны идти перед разделителем «--».
	if idxOf(got, "--force-ipv4") > idxOf(got, "--") {
		t.Errorf("extraArgs должны быть до «--», получено: %v", got)
	}
}

func assertArgs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("длина args: got %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func contains(s []string, v string) bool { return idxOf(s, v) >= 0 }

func idxOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}
