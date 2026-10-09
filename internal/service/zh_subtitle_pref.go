package service

import (
	"os"
	"sort"
	"strings"
)

func embySubtitleLanguagePref() string {
	if v := strings.TrimSpace(os.Getenv("MEBOX_EMBY_SUBTITLE_LANGUAGE")); v != "" {
		if v == "-" {
			return ""
		}
		return v
	}
	return "chi"
}

func embySubtitleModePref() string {
	if v := strings.TrimSpace(os.Getenv("MEBOX_EMBY_SUBTITLE_MODE")); v != "" {
		return v
	}
	return "Always"
}

// zhSubtitleRank: 0 = explicit Simplified, 1 = generic Chinese,
// 2 = Traditional, 9 = not Chinese.
func zhSubtitleRank(lang string) int {
	l := strings.ToLower(strings.TrimSpace(lang))
	l = strings.ReplaceAll(l, "_", "-")
	switch l {
	case "chs", "sc", "zh-hans", "zh-cn", "zh-sg", "zhs", "gb", "jian", "jianti", "simplified":
		return 0
	case "chi", "zho", "zh", "chinese", "cn", "zhi":
		return 1
	case "cht", "tc", "zh-hant", "zh-tw", "zh-hk", "zh-mo", "zht", "big5", "fan", "fanti", "traditional":
		return 2
	}
	return 9
}

func zhSubtitleLabel(lang string) string {
	switch zhSubtitleRank(lang) {
	case 0, 1:
		return "简体中文"
	case 2:
		return "繁體中文"
	}
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "en", "eng":
		return "English"
	}
	return lang
}

// embySubtitleLanguageCode normalises Chinese variants to the ISO 639-2
// code Emby clients match SubtitleLanguagePreference against.
func embySubtitleLanguageCode(lang string) string {
	if zhSubtitleRank(lang) < 9 {
		return "chi"
	}
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "en":
		return "eng"
	}
	return lang
}

func sortSubtitleTracksZhFirst(tracks []SubtitleTrack) {
	sort.SliceStable(tracks, func(i, j int) bool {
		return zhSubtitleRank(tracks[i].Lang) < zhSubtitleRank(tracks[j].Lang)
	})
}

// markPreferredEmbySubtitle flags the best Chinese subtitle stream as default.
func markPreferredEmbySubtitle(streams []map[string]any, tracks []SubtitleTrack, firstIndex int) {
	best, bestRank := -1, 9
	for i, t := range tracks {
		if r := zhSubtitleRank(t.Lang); r < 2 && r < bestRank {
			best, bestRank = i, r
		}
	}
	if best < 0 {
		return
	}
	want := firstIndex + best
	for _, s := range streams {
		if s["Type"] == "Subtitle" {
			if idx, ok := s["Index"].(int); ok && idx == want {
				s["IsDefault"] = true
			}
		}
	}
}

func defaultEmbySubtitleIndex(streams []map[string]any) (int, bool) {
	for _, s := range streams {
		if s["Type"] != "Subtitle" {
			continue
		}
		if d, _ := s["IsDefault"].(bool); d {
			if idx, ok := s["Index"].(int); ok {
				return idx, true
			}
		}
	}
	return 0, false
}
