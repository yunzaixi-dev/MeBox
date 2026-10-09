package service

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/truewhile/MeBox/internal/config"
	"github.com/truewhile/MeBox/internal/model"
	"github.com/truewhile/MeBox/internal/perftrace"
)

// writeTempVideoWithSubtitle creates a temp directory with a fake video and a
// same-name external subtitle, and inserts the media row into the test DB. It
// returns the media id. The subtitle content is valid ASS for .ass/.ssa and
// valid SRT for .srt.
func writeTempVideoWithSubtitle(t *testing.T, svc *EmbyService, lib *model.Library, container, subExt string) string {
	t.Helper()
	dir := t.TempDir()
	video := "MovieName" + container
	sub := "MovieName" + subExt
	if err := os.WriteFile(filepath.Join(dir, video), []byte("video"), 0o644); err != nil {
		t.Fatalf("write video: %v", err)
	}
	var subBody string
	switch strings.ToLower(subExt) {
	case ".srt":
		subBody = "1\n00:00:01,000 --> 00:00:02,000\nhello\n"
	default:
		subBody = "Dialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,hello\n"
	}
	if err := os.WriteFile(filepath.Join(dir, sub), []byte(subBody), 0o644); err != nil {
		t.Fatalf("write subtitle: %v", err)
	}
	m := model.Media{
		LibraryID:  lib.ID,
		Title:      "MovieName",
		Path:       filepath.Join(dir, video),
		Container:  container,
		VideoCodec: "h264",
		AudioCodec: "aac",
	}
	if err := svc.repo.DB.Create(&m).Error; err != nil {
		t.Fatalf("create media: %v", err)
	}
	return m.ID
}

func newTestSubtitleService(t *testing.T, svc *EmbyService) *SubtitleService {
	t.Helper()
	return NewSubtitleService(&config.Config{}, zap.NewNop(), svc.repo)
}

func TestEmbyMediaStreamsAttachSameNameSubtitle(t *testing.T) {
	svc := newTestEmbyService(t)
	lib := model.Library{Name: "电影", Path: `/media/movies`, Type: "movie", Enabled: true}
	if err := svc.repo.Library.Create(t.Context(), &lib); err != nil {
		t.Fatalf("create library: %v", err)
	}
	mediaID := writeTempVideoWithSubtitle(t, svc, &lib, ".mp4", ".ass")
	svc.SetSubtitleService(newTestSubtitleService(t, svc))

	m, err := svc.repo.Media.FindByID(t.Context(), mediaID)
	if err != nil || m == nil {
		t.Fatalf("find media: %v", err)
	}
	streams := svc.mediaStreams(t.Context(), m)

	// Video (0) + Audio (1) + one Subtitle (2)
	if len(streams) != 3 {
		t.Fatalf("expected 3 streams (video/audio/subtitle), got %d: %#v", len(streams), streams)
	}
	sub := streams[2]
	if sub["Type"] != "Subtitle" {
		t.Fatalf("stream[2] type = %v, want Subtitle", sub["Type"])
	}
	if sub["Index"] != 2 {
		t.Fatalf("subtitle index = %v, want 2", sub["Index"])
	}
	// An untagged sidecar is not automatically selected.
	if sub["Codec"] != "ass" {
		t.Fatalf("subtitle codec = %v, want ass", sub["Codec"])
	}
	if sub["DeliveryFormat"] != "ass" {
		t.Fatalf("subtitle delivery format = %v, want ass", sub["DeliveryFormat"])
	}
	if sub["IsDefault"] != false {
		t.Fatalf("subtitle IsDefault = %v, want false", sub["IsDefault"])
	}
	if sub["DeliveryMethod"] != "External" {
		t.Fatalf("subtitle DeliveryMethod = %v, want External", sub["DeliveryMethod"])
	}
	if got, _ := sub["Path"].(string); got == "" {
		t.Fatalf("subtitle Path should not be empty: %#v", sub)
	}
	// Official Emby shape: /Videos/{Id}/{MediaSourceId}/Subtitles/{Index}/Stream.{Format}
	// (no "mediasource_" prefix; that prefix belongs to the Id value, not the route)
	wantURL := "/Videos/" + mediaID + "/" + mediaID + "/Subtitles/2/Stream.ass"
	if sub["DeliveryUrl"] != wantURL {
		t.Fatalf("DeliveryUrl = %v, want %v", sub["DeliveryUrl"], wantURL)
	}
	if sub["IsTextSubtitleStream"] != true {
		t.Fatalf("IsTextSubtitleStream = %v, want true", sub["IsTextSubtitleStream"])
	}
	if sub["SupportsExternalStream"] != true {
		t.Fatalf("SupportsExternalStream = %v, want true", sub["SupportsExternalStream"])
	}
}

func TestEmbyMediaStreamsSubtitleDisplayTitleFriendly(t *testing.T) {
	svc := newTestEmbyService(t)
	lib := model.Library{Name: "电影", Path: `/media/movies`, Type: "movie", Enabled: true}
	if err := svc.repo.Library.Create(t.Context(), &lib); err != nil {
		t.Fatalf("create library: %v", err)
	}
	mediaID := writeTempVideoWithSubtitle(t, svc, &lib, ".mp4", ".ass")
	svc.SetSubtitleService(newTestSubtitleService(t, svc))

	m, err := svc.repo.Media.FindByID(t.Context(), mediaID)
	if err != nil || m == nil {
		t.Fatalf("find media: %v", err)
	}
	streams := svc.mediaStreams(t.Context(), m)
	sub := streams[2]
	// No language tag on the filename -> should fall back to "字幕 (ASS)",
	// matching official Emby's friendly label rather than a bare "und".
	if sub["DisplayTitle"] != "字幕 (ASS)" {
		t.Fatalf("DisplayTitle = %v, want 字幕 (ASS)", sub["DisplayTitle"])
	}
}

func TestEmbyServeSubtitleStreamServesRawSource(t *testing.T) {
	svc := newTestEmbyService(t)
	lib := model.Library{Name: "电影", Path: `/media/movies`, Type: "movie", Enabled: true}
	if err := svc.repo.Library.Create(t.Context(), &lib); err != nil {
		t.Fatalf("create library: %v", err)
	}
	mediaID := writeTempVideoWithSubtitle(t, svc, &lib, ".mp4", ".ass")
	svc.SetSubtitleService(newTestSubtitleService(t, svc))
	ctx, trace := perftrace.New(t.Context())
	tracks, err := svc.subtitle.DiscoverExternalOnly(ctx, mediaID)
	if err != nil || len(tracks) != 1 {
		t.Fatalf("discover subtitle: tracks=%v err=%v", tracks, err)
	}
	want, err := os.ReadFile(tracks[0].Path)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := svc.ServeSubtitleStream(ctx, &buf, mediaID, "2", ""); err != nil {
		t.Fatalf("serve subtitle: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("raw subtitle changed: got %q want %q", buf.Bytes(), want)
	}
	metrics := make(map[string]perftrace.Metric)
	for _, metric := range trace.Finish().Metrics {
		metrics[metric.Name] = metric
	}
	for _, stage := range []string{"subtitle.cache.miss", "subtitle.cache.hit", "subtitle.files.scan", "subtitle.serve.raw", "subtitle.file.open", "subtitle.transfer"} {
		if metrics[stage].Count != 1 {
			t.Fatalf("stage %s count=%d, want 1", stage, metrics[stage].Count)
		}
	}
	if metrics["subtitle.files.read_dir"].Count != 5 || metrics["stream.read"].Bytes != int64(len(want)) {
		t.Fatalf("subtitle IO metrics: read_dir=%+v read=%+v", metrics["subtitle.files.read_dir"], metrics["stream.read"])
	}
}

func TestEmbyServeSubtitleStreamBadIndexNotFound(t *testing.T) {
	svc := newTestEmbyService(t)
	lib := model.Library{Name: "电影", Path: `/media/movies`, Type: "movie", Enabled: true}
	if err := svc.repo.Library.Create(t.Context(), &lib); err != nil {
		t.Fatalf("create library: %v", err)
	}
	mediaID := writeTempVideoWithSubtitle(t, svc, &lib, ".mp4", ".ass")
	svc.SetSubtitleService(newTestSubtitleService(t, svc))

	var buf bytes.Buffer
	if err := svc.ServeSubtitleStream(t.Context(), &buf, mediaID, "99", ""); err != ErrSubtitleNotFound {
		t.Fatalf("expected ErrSubtitleNotFound for index 99, got %v", err)
	}
}

func TestEmbyMediaStreamsNoSubtitleServiceKeepsVideoAudio(t *testing.T) {
	svc := newTestEmbyService(t)
	lib := model.Library{Name: "电影", Path: `/media/movies`, Type: "movie", Enabled: true}
	if err := svc.repo.Library.Create(t.Context(), &lib); err != nil {
		t.Fatalf("create library: %v", err)
	}
	// Intentionally do NOT SetSubtitleService.
	mediaID := writeTempVideoWithSubtitle(t, svc, &lib, ".mp4", ".srt")

	m, err := svc.repo.Media.FindByID(t.Context(), mediaID)
	if err != nil || m == nil {
		t.Fatalf("find media: %v", err)
	}
	streams := svc.mediaStreams(t.Context(), m)
	if len(streams) != 2 {
		t.Fatalf("expected 2 streams (video/audio) without subtitle service, got %d: %#v", len(streams), streams)
	}
}

func TestEmbySubtitlePlaybackPreferences(t *testing.T) {
	for _, tc := range []struct {
		name        string
		audio       string
		suffixes    []string // Expected discovery order, including stable ties.
		wantDefault bool
	}{
		{"mixed", "aac", []string{"chs.ass", "zh_CN.srt", "chi.vtt", "zh.ssa", "zh_Hant.srt", "en.srt"}, true},
		{"without_audio", "", []string{"chs.srt", "cht.srt"}, true},
		{"generic_chinese", "aac", []string{"zh.srt", "cht.srt"}, true},
		{"traditional_only", "aac", []string{"zh_Hant.srt"}, false},
		{"other_only", "", []string{"en.srt"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestEmbyService(t)
			dir := t.TempDir()
			lib := model.Library{Name: "电影", Path: dir, Type: "movie", Enabled: true}
			if err := svc.repo.Library.Create(t.Context(), &lib); err != nil {
				t.Fatal(err)
			}
			m := model.Media{LibraryID: lib.ID, Title: "MovieName", Path: filepath.Join(dir, "MovieName.mkv"), VideoCodec: "h264", AudioCodec: tc.audio}
			if err := svc.repo.DB.Create(&m).Error; err != nil {
				t.Fatal(err)
			}
			for _, suffix := range tc.suffixes {
				if err := os.WriteFile(filepath.Join(dir, "MovieName."+suffix), []byte(suffix), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			subtitle := newTestSubtitleService(t, svc)
			svc.SetSubtitleService(subtitle)
			tracks, err := subtitle.DiscoverExternalOnly(t.Context(), m.ID)
			if err != nil || len(tracks) != len(tc.suffixes) {
				t.Fatalf("discovery = %#v, err = %v", tracks, err)
			}
			out, err := svc.PlaybackInfo(t.Context(), m.ID, "", model.EmbyPlaybackInfoRequest{})
			if err != nil {
				t.Fatal(err)
			}
			sources := out["MediaSources"].([]map[string]any)
			if len(sources) != 1 {
				t.Fatalf("media sources = %#v", sources)
			}
			source := sources[0]
			streams := source["MediaStreams"].([]map[string]any)
			first := 1
			if tc.audio != "" {
				first = 2
			}
			if len(streams) != first+len(tracks) {
				t.Fatalf("media streams = %#v", streams)
			}
			defaultIndex, hasDefault := source["DefaultSubtitleStreamIndex"]
			if hasDefault != tc.wantDefault || (hasDefault && defaultIndex != first) {
				t.Fatalf("default subtitle index = %v, present = %v", defaultIndex, hasDefault)
			}
			for i, suffix := range tc.suffixes {
				track, stream := tracks[i], streams[first+i]
				path := filepath.Join(dir, "MovieName."+suffix)
				lang := strings.ToLower(strings.TrimSuffix(suffix, filepath.Ext(suffix)))
				language, label := "chi", "简体中文"
				switch lang {
				case "cht", "zh_hant":
					label = "繁體中文"
				case "en":
					language, label = "eng", "English"
				}
				if track.Path != path || track.Lang != lang || track.Label != label {
					t.Fatalf("discovered track[%d] = %#v, want %s (%s)", i, track, path, label)
				}
				index := first + i
				if stream["Index"] != index || stream["Type"] != "Subtitle" || stream["Path"] != path || stream["Language"] != language || stream["DisplayTitle"] != label {
					t.Fatalf("subtitle stream[%d] = %#v", i, stream)
				}
				if stream["IsDefault"] != (tc.wantDefault && i == 0) {
					t.Fatalf("subtitle IsDefault = %v for %s", stream["IsDefault"], suffix)
				}
				codec := strings.TrimPrefix(filepath.Ext(suffix), ".")
				if codec == "srt" {
					codec = "subrip"
				}
				wantURL := "/Videos/" + m.ID + "/" + m.ID + "/Subtitles/" + strconv.Itoa(index) + "/Stream." + codec
				if stream["DeliveryUrl"] != wantURL || stream["Codec"] != codec {
					t.Fatalf("subtitle delivery = %#v, want %s", stream, wantURL)
				}
				var body bytes.Buffer
				if err := svc.ServeSubtitleStream(t.Context(), &body, m.ID, strconv.Itoa(index), ""); err != nil {
					t.Fatal(err)
				}
				if body.String() != suffix {
					t.Fatalf("subtitle index %d served %q, want %q", index, body.String(), suffix)
				}
			}
		})
	}
}

func TestEmbyUserSubtitleConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, language, mode, wantLanguage, wantMode string
	}{
		{"defaults", "", "", "chi", "Always"},
		{"whitespace", "  ", "  ", "chi", "Always"},
		{"overrides", " eng ", " Default ", "eng", "Default"},
		{"empty_language", " - ", " None ", "", "None"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MEBOX_EMBY_SUBTITLE_LANGUAGE", tc.language)
			t.Setenv("MEBOX_EMBY_SUBTITLE_MODE", tc.mode)
			svc := &EmbyService{}
			user := &model.User{Username: "viewer", SubtitleChineseMode: "traditional"}
			configuration := svc.userPayload(user)["Configuration"].(map[string]any)
			if configuration["SubtitleLanguagePreference"] != tc.wantLanguage || configuration["SubtitleMode"] != tc.wantMode {
				t.Fatalf("subtitle configuration = %#v", configuration)
			}
			if user.SubtitleChineseMode != "traditional" {
				t.Fatal("Emby configuration changed the user's web subtitle preference")
			}
		})
	}
}
