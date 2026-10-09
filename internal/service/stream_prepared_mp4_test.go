package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/truewhile/MeBox/internal/config"
	"github.com/truewhile/MeBox/internal/model"
	"github.com/truewhile/MeBox/internal/repository"
)

const preparedMP4TestBytes = "0123456789abcdefghij"

func preparedMP4Fixture(t *testing.T) (*StreamService, *config.Config, *repository.Container, string, string) {
	t.Helper()
	repos := newStreamTestRepo(t)
	cfg := &config.Config{Cache: config.CacheConfig{CacheDir: t.TempDir()}}
	source := filepath.Join(t.TempDir(), "media", "source.mkv")
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("original-video"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := repos.DB.Create(&model.Media{Base: model.Base{ID: "prepared-mp4-test"}, Path: source, VideoCodec: "h264", AudioCodec: "aac", Container: "mkv"}).Error; err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.Cache.CacheDir, "prepared-mp4", "prepared-mp4-test")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(preparedMP4Metadata{
		Version: 1, SourceSize: stat.Size(), SourceMtimeNS: stat.ModTime().UnixNano(),
		VideoCodec: "h264", AudioCodec: "aac", AudioChannels: 2, AudioSampleRate: 48000,
		VideoProfile: "High", VideoLevel: 40, VideoBitDepth: 8, ColorTransfer: "bt709", Codecs: "avc1.640028,mp4a.40.2",
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"source.json": data, "stream.mp4": []byte(preparedMP4TestBytes)} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return NewStreamService(cfg, zap.NewNop(), repos, nil), cfg, repos, source, dir
}

func TestPreparedMP4RangeHEADAndConditionals(t *testing.T) {
	svc, _, _, _, _ := preparedMP4Fixture(t)
	full := httptest.NewRecorder()
	if err := svc.ServePreparedMP4(full, httptest.NewRequest(http.MethodGet, "/Videos/prepared-mp4-test/stream", nil), "prepared-mp4-test"); err != nil {
		t.Fatal(err)
	}
	etag := full.Header().Get("ETag")
	if full.Code != http.StatusOK || full.Body.String() != preparedMP4TestBytes || etag == "" ||
		full.Header().Get("Content-Type") != "video/mp4" || full.Header().Get("Cache-Control") != "private, no-cache" ||
		full.Header().Get("X-Content-Type-Options") != "nosniff" || full.Header().Get("Last-Modified") != "" {
		t.Fatalf("full MP4 response: %d %q %v", full.Code, full.Body.String(), full.Header())
	}
	for _, tc := range []struct {
		name, method, rangeValue, condition, validator, body, contentRange string
		status                                                             int
	}{
		{name: "range", method: http.MethodGet, rangeValue: "bytes=2-6", body: "23456", contentRange: "bytes 2-6/20", status: http.StatusPartialContent},
		{name: "suffix", method: http.MethodGet, rangeValue: "bytes=-3", body: "hij", contentRange: "bytes 17-19/20", status: http.StatusPartialContent},
		{name: "HEAD", method: http.MethodHead, status: http.StatusOK},
		{name: "cached", method: http.MethodGet, condition: "If-None-Match", validator: etag, status: http.StatusNotModified},
		{name: "precondition", method: http.MethodGet, condition: "If-Match", validator: `"other"`, status: http.StatusPreconditionFailed},
		{name: "IfRange current", method: http.MethodGet, rangeValue: "bytes=2-6", condition: "If-Range", validator: etag, body: "23456", contentRange: "bytes 2-6/20", status: http.StatusPartialContent},
		{name: "IfRange stale", method: http.MethodGet, rangeValue: "bytes=2-6", condition: "If-Range", validator: `"other"`, body: preparedMP4TestBytes, status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/Videos/prepared-mp4-test/stream", nil)
			if tc.rangeValue != "" {
				r.Header.Set("Range", tc.rangeValue)
			}
			if tc.condition != "" {
				r.Header.Set(tc.condition, tc.validator)
			}
			w := httptest.NewRecorder()
			if err := svc.ServePreparedMP4(w, r, "prepared-mp4-test"); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.status || w.Body.String() != tc.body || w.Header().Get("Content-Range") != tc.contentRange {
				t.Fatalf("response: %d %q %v", w.Code, w.Body.String(), w.Header())
			}
			if tc.method == http.MethodHead && w.Header().Get("Content-Length") != "20" {
				t.Fatalf("HEAD length: %v", w.Header())
			}
		})
	}
}

func TestPreparedMP4RejectsChangedSourceAndSymlinks(t *testing.T) {
	for _, target := range []string{"source changed", "source symlink", "source parent", "package", "package parent", "cache", "source.json", "stream.mp4", "empty stream"} {
		t.Run(target, func(t *testing.T) {
			svc, cfg, _, source, dir := preparedMP4Fixture(t)
			switch target {
			case "source changed":
				stat, err := os.Stat(source)
				if err != nil {
					t.Fatal(err)
				}
				changed := stat.ModTime().Add(time.Second)
				if err := os.Chtimes(source, changed, changed); err != nil {
					t.Fatal(err)
				}
			case "empty stream":
				if err := os.WriteFile(filepath.Join(dir, "stream.mp4"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			default:
				path := filepath.Join(dir, target)
				switch target {
				case "source symlink":
					path = source
				case "source parent":
					path = filepath.Dir(source)
				case "package":
					path = dir
				case "package parent":
					path = filepath.Dir(dir)
				case "cache":
					// Keep the real directory inside the test's cleanup root.
					cfg.Cache.CacheDir = filepath.Join(cfg.Cache.CacheDir, "alias")
					if err := os.Symlink(filepath.Dir(cfg.Cache.CacheDir), cfg.Cache.CacheDir); err != nil {
						t.Fatal(err)
					}
				}
				if target != "cache" {
					if err := os.Rename(path, path+".real"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(path+".real", path); err != nil {
						t.Fatal(err)
					}
				}
			}
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/Videos/prepared-mp4-test/stream", nil)
			if err := svc.ServePreparedMP4(w, r, "prepared-mp4-test"); !errors.Is(err, ErrMediaNotFound) || w.Body.Len() != 0 {
				t.Fatalf("untrusted package served: %v %q", err, w.Body.String())
			}
		})
	}
}

func TestPreparedMP4RejectsUntrustedMetadata(t *testing.T) {
	svc, _, repos, _, dir := preparedMP4Fixture(t)
	path := filepath.Join(dir, "source.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"version", 2}, {"video_codec", "vp9"}, {"audio_codec", "mp3"}, {"audio_codec", ""},
		{"audio_transcoded", true}, {"audio_channels", -1}, {"audio_sample_rate", -1},
		{"video_bit_depth", -1}, {"video_level", -1}, {"video_profile", "High\n"},
		{"color_transfer", "bt709\r"}, {"codecs", "vp09.00.40.08,mp4a.40.2"},
		{"codecs", "avc1.640028,ec-3"}, {"codecs", "avc1.640028\r\n,mp4a.40.2"}, {"source_size", 1},
	} {
		t.Run(tc.key+"="+stringMustJSON(t, tc.value), func(t *testing.T) {
			var metadata map[string]json.RawMessage
			if err := json.Unmarshal(original, &metadata); err != nil {
				t.Fatal(err)
			}
			metadata[tc.key] = json.RawMessage(stringMustJSON(t, tc.value))
			if err := os.WriteFile(path, []byte(stringMustJSON(t, metadata)), 0600); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			if err := svc.ServePreparedMP4(w, httptest.NewRequest(http.MethodGet, "/stream", nil), "prepared-mp4-test"); !errors.Is(err, ErrMediaNotFound) || w.Body.Len() != 0 {
				t.Fatalf("untrusted metadata served: %v %q", err, w.Body.String())
			}
		})
	}
	if err := os.WriteFile(path, append(original, []byte(strings.Repeat(" ", 4096))...), 0600); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	if err := svc.ServePreparedMP4(w, httptest.NewRequest(http.MethodGet, "/stream", nil), "prepared-mp4-test"); !errors.Is(err, ErrMediaNotFound) {
		t.Fatalf("oversized metadata accepted: %v", err)
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := repos.DB.Model(&model.Media{}).Where("id = ?", "prepared-mp4-test").Update("audio_codec", "").Error; err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	if err := svc.ServePreparedMP4(w, httptest.NewRequest(http.MethodGet, "/stream", nil), "prepared-mp4-test"); !errors.Is(err, ErrMediaNotFound) {
		t.Fatalf("request did not reload source metadata: %v", err)
	}
}

func stringMustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPreparedMP4RollbackDoesNotReturnFalse304(t *testing.T) {
	svc, _, _, _, dir := preparedMP4Fixture(t)
	w := httptest.NewRecorder()
	if err := svc.ServePreparedMP4(w, httptest.NewRequest(http.MethodGet, "/stream", nil), "prepared-mp4-test"); err != nil {
		t.Fatal(err)
	}
	etag := w.Header().Get("ETag")
	marker, err := os.Stat(filepath.Join(dir, "source.json"))
	if err != nil {
		t.Fatal(err)
	}
	older := marker.ModTime().Add(-time.Hour)
	if err := os.WriteFile(filepath.Join(dir, "stream.mp4"), []byte("rolled-back-package"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "source.json"), filepath.Join(dir, "stream.mp4")} {
		if err := os.Chtimes(path, older, older); err != nil {
			t.Fatal(err)
		}
	}
	for _, withETag := range []bool{false, true} {
		r := httptest.NewRequest(http.MethodGet, "/stream", nil)
		r.Header.Set("If-Modified-Since", marker.ModTime().UTC().Format(http.TimeFormat))
		if withETag {
			r.Header.Set("If-None-Match", etag)
		}
		w = httptest.NewRecorder()
		if err := svc.ServePreparedMP4(w, r, "prepared-mp4-test"); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusOK || w.Body.String() != "rolled-back-package" || w.Header().Get("ETag") == etag {
			t.Fatalf("rollback reused cached bytes: %d %q %v", w.Code, w.Body.String(), w.Header())
		}
	}
}

func TestPreparedMP4NativeCodecsAndLocalSourceBinding(t *testing.T) {
	for _, tc := range []struct {
		video, audio string
		allowed      bool
	}{
		{"h264", "aac", true}, {"hevc", "ac3", true}, {"av1", "eac3", true},
		{"hevc", "flac", true}, {"av1", "", true}, {"vp9", "aac", false}, {"h264", "opus", false},
	} {
		t.Run(tc.video+"/"+tc.audio, func(t *testing.T) {
			svc, _, repos, _, dir := preparedMP4Fixture(t)
			path := filepath.Join(dir, "source.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var metadata preparedMP4Metadata
			if err := json.Unmarshal(data, &metadata); err != nil {
				t.Fatal(err)
			}
			metadata.VideoCodec, metadata.AudioCodec, metadata.Codecs = tc.video, tc.audio, ""
			if tc.audio == "" {
				metadata.AudioChannels, metadata.AudioSampleRate = 0, 0
			}
			if err := os.WriteFile(path, []byte(stringMustJSON(t, metadata)), 0600); err != nil {
				t.Fatal(err)
			}
			if err := repos.DB.Model(&model.Media{}).Where("id = ?", "prepared-mp4-test").Updates(map[string]any{"video_codec": tc.video, "audio_codec": tc.audio}).Error; err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			err = svc.ServePreparedMP4(w, httptest.NewRequest(http.MethodGet, "/stream", nil), "prepared-mp4-test")
			if tc.allowed {
				if err != nil || w.Code != http.StatusOK || w.Body.String() != preparedMP4TestBytes {
					t.Fatalf("native copied track rejected: %v %d %q", err, w.Code, w.Body.String())
				}
			} else if !errors.Is(err, ErrMediaNotFound) || w.Body.Len() != 0 {
				t.Fatalf("unsupported MP4 codec accepted: %v %q", err, w.Body.String())
			}
		})
	}
	for _, column := range []string{"strm_url", "container", "path"} {
		t.Run(column, func(t *testing.T) {
			svc, _, repos, source, _ := preparedMP4Fixture(t)
			value := "strm"
			if column == "strm_url" {
				value = "https://remote.invalid/video.mkv"
			}
			if column == "path" {
				value = source + ".strm"
				if err := os.Rename(source, value); err != nil {
					t.Fatal(err)
				}
			}
			if err := repos.DB.Model(&model.Media{}).Where("id = ?", "prepared-mp4-test").Update(column, value).Error; err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			if err := svc.ServePreparedMP4(w, httptest.NewRequest(http.MethodGet, "/stream", nil), "prepared-mp4-test"); !errors.Is(err, ErrMediaNotFound) || w.Body.Len() != 0 {
				t.Fatalf("STRM source accepted: %v %q", err, w.Body.String())
			}
		})
	}
}
