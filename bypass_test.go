package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestYtBypassArgs(t *testing.T) {
	bundle := []string{
		"--extractor-args", "youtube:player_client=android,ios",
		"--force-ipv4",
	}
	tests := []struct {
		name    string
		enabled bool
		profile *platformProfile
		want    []string
	}{
		{"youtube enabled → бандл", true, youtubeProfile, bundle},
		{"tiktok enabled → nil (игнор)", true, tiktokProfile, nil},
		{"youtube disabled → nil", false, youtubeProfile, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ytBypassArgs(tt.enabled, tt.profile); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ytBypassArgs(%v, %s) = %v, want %v", tt.enabled, tt.profile.name, got, tt.want)
			}
		})
	}
}

// TestBypassReachesYtdlpArgs: бандл доходит до вызова yt-dlp и стоит перед «--».
func TestBypassReachesYtdlpArgs(t *testing.T) {
	cfg := runConfig{outDir: "out", extraArgs: ytBypassArgs(true, youtubeProfile)}
	args := ytdlpArgs(cfg, "", "https://youtu.be/dQw4w9WgXcQ")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--extractor-args youtube:player_client=android,ios") {
		t.Errorf("нет extractor-args в вызове: %s", joined)
	}
	if !strings.Contains(joined, "--force-ipv4") {
		t.Errorf("нет --force-ipv4 в вызове: %s", joined)
	}
	if idxOf(args, "--force-ipv4") > idxOf(args, "--") {
		t.Errorf("bypass-флаги должны быть до «--»: %v", args)
	}
}
