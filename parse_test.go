package main

import (
	"reflect"
	"testing"
)

func TestCleanURL(t *testing.T) {
	tests := []struct {
		in   string
		keep []string
		want string
	}{
		// watch: параметр v сохраняется (keepParams youtube), остальное срезается
		{"https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=PLabc&index=3&t=42s", youtubeProfile.keepParams, "https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
		// youtu.be: si — мусор
		{"https://youtu.be/dQw4w9WgXcQ?si=xyz", youtubeProfile.keepParams, "https://youtu.be/dQw4w9WgXcQ"},
		// tiktok: все параметры мусорные, включая случайный v
		{"https://www.tiktok.com/@u/video/7412345678901234567?is_copy_url=1&lang=en&v=junk", tiktokProfile.keepParams, "https://www.tiktok.com/@u/video/7412345678901234567"},
		// vm.tiktok.com — как есть
		{"https://vm.tiktok.com/ZMabc123/", tiktokProfile.keepParams, "https://vm.tiktok.com/ZMabc123/"},
		// fragment срезается
		{"https://youtu.be/dQw4w9WgXcQ#t=10", youtubeProfile.keepParams, "https://youtu.be/dQw4w9WgXcQ"},
	}
	for _, tt := range tests {
		if got := cleanURL(tt.in, tt.keep); got != tt.want {
			t.Errorf("cleanURL(%q, %v) = %q, want %q", tt.in, tt.keep, got, tt.want)
		}
	}
}

// TestMatchesDomain: точный матч или суффикс по границе точки; похожие чужие
// домены не проходят.
func TestMatchesDomain(t *testing.T) {
	tests := []struct {
		url     string
		profile *platformProfile
		want    bool
	}{
		{"https://www.youtube.com/watch?v=x", youtubeProfile, true},
		{"https://m.youtube.com/watch?v=x", youtubeProfile, true},
		{"https://youtu.be/x", youtubeProfile, true},
		{"https://youtube.com:443/watch?v=x", youtubeProfile, true}, // порт срезается
		{"https://evilyoutube.com/watch?v=x", youtubeProfile, false},
		{"https://youtube.com.evil.io/watch?v=x", youtubeProfile, false},
		{"https://vm.tiktok.com/ZMabc/", tiktokProfile, true},
		{"https://nottiktok.com/video/1", tiktokProfile, false},
	}
	for _, tt := range tests {
		if got := tt.profile.matchesDomain(tt.url); got != tt.want {
			t.Errorf("matchesDomain(%q, %s) = %v, want %v", tt.url, tt.profile.name, got, tt.want)
		}
	}
}

func TestParseLinksYouTube(t *testing.T) {
	text := `Смотри это https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=PLabc&index=3&t=42s круто!
А ещё (https://youtu.be/dQw4w9WgXcQ?si=xyz).
Shorts: https://www.youtube.com/shorts/aaaaaaaaaaa
Дубль снова: https://youtu.be/dQw4w9WgXcQ
Не ютуб: https://www.tiktok.com/@u/video/7412345678901234567`

	links, found := parseLinks(text, youtubeProfile)
	if found != 4 {
		t.Errorf("found = %d, want 4 (4 youtube-ссылки, tiktok отфильтрован)", found)
	}
	want := []string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://www.youtube.com/shorts/aaaaaaaaaaa",
	}
	if !reflect.DeepEqual(links, want) {
		t.Errorf("links = %v, want %v", links, want)
	}
}

func TestParseLinksTikTok(t *testing.T) {
	text := `микс: https://www.tiktok.com/@user/video/7412345678901234567?is_copy_url=1&lang=en, и короткая https://vm.tiktok.com/ZMabc123/ готово.
дубль https://www.tiktok.com/@user/video/7412345678901234567
ютуб игнор: https://youtu.be/dQw4w9WgXcQ`

	links, found := parseLinks(text, tiktokProfile)
	if found != 3 {
		t.Errorf("found = %d, want 3", found)
	}
	want := []string{
		"https://www.tiktok.com/@user/video/7412345678901234567",
		"https://vm.tiktok.com/ZMabc123/",
	}
	if !reflect.DeepEqual(links, want) {
		t.Errorf("links = %v, want %v", links, want)
	}
}

// TestParseLinksEmpty: текст без ссылок платформы → пусто, без паники.
func TestParseLinksEmpty(t *testing.T) {
	links, found := parseLinks("просто текст без ссылок", youtubeProfile)
	if found != 0 || len(links) != 0 {
		t.Errorf("ожидалось 0 найдено / 0 записано, получено %d / %d", found, len(links))
	}
}
