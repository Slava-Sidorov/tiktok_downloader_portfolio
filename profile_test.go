package main

import "testing"

func TestExtractTikTokID(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://www.tiktok.com/@user/video/7412345678901234567", "7412345678901234567"},
		{"https://www.tiktok.com/@user/video/7412345678901234567?is_copy_url=1&lang=en", "7412345678901234567"},
		{"https://vm.tiktok.com/ZMabcдеf/", ""}, // короткая ссылка — ID не извлекается
		{"https://youtu.be/dQw4w9WgXcQ", ""},    // чужая платформа
	}
	for _, tt := range tests {
		if got := extractTikTokID(tt.url); got != tt.want {
			t.Errorf("extractTikTokID(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestExtractYouTubeID(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=PLabc&index=3&t=42s", "dQw4w9WgXcQ"},
		{"https://youtu.be/dQw4w9WgXcQ?si=Xyz123", "dQw4w9WgXcQ"},
		{"https://www.youtube.com/shorts/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"https://www.youtube.com/embed/dQw4w9WgXcQ", "dQw4w9WgXcQ"},
		{"https://www.tiktok.com/@user/video/7412345678901234567", ""}, // чужая платформа
		{"https://www.youtube.com/", ""},                               // без ID
	}
	for _, tt := range tests {
		if got := extractYouTubeID(tt.url); got != tt.want {
			t.Errorf("extractYouTubeID(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

// TestDedupKeyCollapsesVariants: разные формы одной ссылки → один ключ дедупа.
func TestDedupKeyCollapsesVariants(t *testing.T) {
	yt := []string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=PLabc&index=3",
		"https://youtu.be/dQw4w9WgXcQ?si=abc",
		"https://www.youtube.com/shorts/dQw4w9WgXcQ",
	}
	first := youtubeProfile.dedupKey(yt[0])
	if first != "id:dQw4w9WgXcQ" {
		t.Fatalf("youtube dedupKey = %q, want id:dQw4w9WgXcQ", first)
	}
	for _, u := range yt[1:] {
		if k := youtubeProfile.dedupKey(u); k != first {
			t.Errorf("youtube dedupKey(%q) = %q, want %q", u, k, first)
		}
	}

	tk := []string{
		"https://www.tiktok.com/@user/video/7412345678901234567",
		"https://www.tiktok.com/@user/video/7412345678901234567?is_copy_url=1",
	}
	tkFirst := tiktokProfile.dedupKey(tk[0])
	if tkFirst != "id:7412345678901234567" {
		t.Fatalf("tiktok dedupKey = %q", tkFirst)
	}
	for _, u := range tk[1:] {
		if k := tiktokProfile.dedupKey(u); k != tkFirst {
			t.Errorf("tiktok dedupKey(%q) = %q, want %q", u, k, tkFirst)
		}
	}
}

func TestClassifyYtdlpErrorYouTube(t *testing.T) {
	tests := []struct {
		name     string
		stderr   string
		wantKind ytdlpErrKind
	}{
		{"private video", "ERROR: [youtube] xxx: Private video. Sign in if you've been granted access", errVideo},
		{"unavailable", "ERROR: [youtube] xxx: Video unavailable", errVideo},
		{"bot check", "ERROR: [youtube] xxx: Sign in to confirm you're not a bot", errVideo},
		{"403 network", "ERROR: unable to download video data: HTTP Error 403: Forbidden", errProxy},
		{"429 network", "ERROR: HTTP Error 429: Too Many Requests", errProxy},
		{"generic network", "ERROR: [Errno 104] Connection reset by peer", errProxy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, reason := classifyYtdlpError(tt.stderr, youtubeProfile)
			if kind != tt.wantKind {
				t.Errorf("classifyYtdlpError(%q) kind = %v, want %v (reason=%q)", tt.stderr, kind, tt.wantKind, reason)
			}
		})
	}
}

func TestProfileFor(t *testing.T) {
	if p, err := profileFor("tiktok"); err != nil || p != tiktokProfile {
		t.Errorf("profileFor(tiktok) = %v, %v", p, err)
	}
	if p, err := profileFor("youtube"); err != nil || p != youtubeProfile {
		t.Errorf("profileFor(youtube) = %v, %v", p, err)
	}
	if _, err := profileFor("vimeo"); err == nil {
		t.Errorf("profileFor(vimeo) должно вернуть ошибку")
	}
}
