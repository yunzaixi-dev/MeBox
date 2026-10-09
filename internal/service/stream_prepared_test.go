package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/truewhile/MeBox/internal/config"
	"github.com/truewhile/MeBox/internal/model"
	"github.com/truewhile/MeBox/internal/repository"
)

func preparedHLSFixture(t *testing.T) (*StreamService, *config.Config, *repository.Container, string, string) {
	t.Helper()
	repos := newStreamTestRepo(t)
	cfg := &config.Config{Cache: config.CacheConfig{CacheDir: t.TempDir()}}
	source := filepath.Join(t.TempDir(), "source.mkv")
	if err := os.WriteFile(source, []byte("original-video"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := repos.DB.Create(&model.Media{Base: model.Base{ID: "prepared-test"}, Path: source, VideoCodec: "av1"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := repos.Setting.Set(t.Context(), PlaybackDirectOnlySettingKey, "true"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.Cache.CacheDir, "prepared-hls", "prepared-test")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := json.Marshal(map[string]any{
		"version": 1, "source_size": stat.Size(), "source_mtime_ns": stat.ModTime().UnixNano(),
		"video_codec": "av1", "audio_codec": "aac", "codecs": "av01.0.08M.08,mp4a.40.2", "audio_transcoded": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"source.json": meta,
		"index.m3u8":  []byte("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:4\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:4.000,\nseg_00000.m4s\n#EXT-X-ENDLIST\n"),
		"init.mp4":    []byte("initialization"), "seg_00000.m4s": []byte("fragment-data"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return NewStreamService(cfg, zap.NewNop(), repos, nil), cfg, repos, source, dir
}

func TestPreparedHLSServesFullTimelineWithoutTranscoding(t *testing.T) {
	svc, cfg, repos, _, _ := preparedHLSFixture(t)
	info, err := NewCloud115PlaybackService(cfg, zap.NewNop(), repos, nil).PlaybackInfo(t.Context(), "prepared-test", 0)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload["prepared_hls"]) == 0 {
		t.Fatal("prepared source was not advertised while live transcoding is disabled")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/hls/prepared-test/index.m3u8?quality=prepared&token=fixture&profile_id=p&start=4800&_seek=42", nil)
	w := httptest.NewRecorder()
	if err := svc.ServeHLSPlaylist(w, req, "prepared-test"); err != nil {
		t.Fatal(err)
	}
	body := w.Body.String()
	if !strings.Contains(body, "#EXT-X-ENDLIST") || !strings.Contains(body, "#EXT-X-MAP:URI=\"init.mp4?") {
		t.Fatalf("full VOD and authenticated initialization missing: %s", body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "seg_") || strings.HasPrefix(line, "#EXT-X-MAP:") {
			if !strings.Contains(line, "token=fixture") || !strings.Contains(line, "quality=prepared") || !strings.Contains(line, "profile_id=p") || strings.Contains(line, "start=") {
				t.Fatalf("asset lost playback authorization or inherited live seek: %s", line)
			}
		}
	}
	req.Header.Set("Authorization", "Bearer current-session")
	w = httptest.NewRecorder()
	if err := svc.ServeHLSPlaylist(w, req, "prepared-test"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(w.Body.String(), "token=") || strings.Contains(w.Body.String(), "api_key=") ||
		!strings.Contains(w.Body.String(), "profile_id=p") {
		t.Fatal("header-authenticated index repeats credentials or loses profile scope")
	}
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.HasPrefix(line, "seg_") {
			req = httptest.NewRequest(http.MethodGet, "/api/hls/prepared-test/"+line, nil)
			break
		}
	}
	req.Header.Set("Range", "bytes=2-6")
	w = httptest.NewRecorder()
	if err := svc.ServeHLSSegment(w, req, "prepared-test", "seg_00000.m4s"); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusPartialContent || w.Body.String() != "agmen" || w.Header().Get("Content-Range") != "bytes 2-6/13" {
		t.Fatalf("fragment Range mismatch: %d %q %q", w.Code, w.Body.String(), w.Header().Get("Content-Range"))
	}
	if !strings.Contains(w.Header().Get("Cache-Control"), "private") {
		t.Fatal("authenticated media became shared-cacheable")
	}
}

func TestPreparedHLSRejectsChangedSourceAndUntrustedAssets(t *testing.T) {
	svc, cfg, repos, source, dir := preparedHLSFixture(t)
	marker, err := os.Stat(filepath.Join(dir, "source.json"))
	if err != nil {
		t.Fatal(err)
	}
	query := "?quality=prepared&prepared_v=" + strconv.FormatInt(marker.ModTime().UnixNano(), 10)
	for _, asset := range []string{"../source.json", "source.json", "init.mp4/../source.json", "seg_bad.m4s"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/hls/prepared-test/asset"+query, nil)
		if err := svc.ServeHLSSegment(w, r, "prepared-test", asset); err == nil || w.Body.Len() != 0 {
			t.Fatalf("untrusted asset served: %q", asset)
		}
	}
	if err := os.Remove(filepath.Join(dir, "seg_00000.m4s")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, filepath.Join(dir, "seg_00000.m4s")); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/hls/prepared-test/seg_00000.m4s"+query, nil)
	if err := svc.ServeHLSSegment(w, r, "prepared-test", "seg_00000.m4s"); err == nil || w.Body.Len() != 0 {
		t.Fatal("symlink escaped prepared package")
	}
	if err := os.WriteFile(source, []byte("replaced-original-video"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := NewCloud115PlaybackService(cfg, zap.NewNop(), repos, nil).PlaybackInfo(t.Context(), "prepared-test", 0)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(info)
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload["prepared_hls"]) != 0 {
		t.Fatal("changed source still advertised stale package")
	}
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodGet, "/api/hls/prepared-test/index.m3u8?quality=prepared", nil)
	if err := svc.ServeHLSPlaylist(w, r, "prepared-test"); err == nil || w.Body.Len() != 0 {
		t.Fatal("stale prepared timeline served after source replacement")
	}
}

func TestPreparedHLSRejectsExternalManifestURIs(t *testing.T) {
	svc, _, _, _, dir := preparedHLSFixture(t)
	for _, body := range []string{
		"#EXTM3U\n#EXT-X-MAP:URI=\"https://outside.invalid/init.mp4\"\n#EXTINF:4,\nseg_00000.m4s\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:4,\nhttps://outside.invalid/seg_00000.m4s\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXT-X-KEY:METHOD=AES-128,URI=\"https://outside.invalid/key\"\n#EXTINF:4,\nseg_00000.m4s\n#EXT-X-ENDLIST\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/hls/prepared-test/index.m3u8?quality=prepared&token=private-fixture", nil)
		if err := svc.ServeHLSPlaylist(w, r, "prepared-test"); err == nil || w.Body.Len() != 0 {
			t.Fatal("untrusted manifest served or credentials exposed to an external asset")
		}
	}
}

func TestPreparedHLSPackageRollbackDoesNotReuseCachedFragments(t *testing.T) {
	svc, _, _, _, dir := preparedHLSFixture(t)
	var cachedURL, cachedBody, lastModified string
	readFragment := func() (string, string) {
		t.Helper()
		playlist := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/hls/prepared-test/index.m3u8?quality=prepared", nil)
		if err := svc.ServeHLSPlaylist(playlist, r, "prepared-test"); err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(playlist.Body.String(), "\n") {
			if !strings.HasPrefix(line, "seg_") {
				continue
			}
			r = httptest.NewRequest(http.MethodGet, "/api/hls/prepared-test/"+line, nil)
			if line == cachedURL {
				r.Header.Set("If-Modified-Since", lastModified)
			}
			w := httptest.NewRecorder()
			if err := svc.ServeHLSSegment(w, r, "prepared-test", "seg_00000.m4s"); err != nil {
				t.Fatal(err)
			}
			if w.Code == http.StatusNotModified {
				return cachedBody, line
			}
			lastModified = w.Header().Get("Last-Modified")
			return w.Body.String(), line
		}
		t.Fatal("playlist contains no media fragment")
		return "", ""
	}
	cachedBody, cachedURL = readFragment()
	asset := filepath.Join(dir, "seg_00000.m4s")
	older := time.Now().Add(-time.Hour)
	if err := os.WriteFile(asset, []byte("rolled-back-fragment"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{asset, filepath.Join(dir, "source.json")} {
		if err := os.Chtimes(path, older, older); err != nil {
			t.Fatal(err)
		}
	}
	body, _ := readFragment()
	if body != "rolled-back-fragment" {
		t.Fatalf("rollback reused a cached fragment from another package: %q", body)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/hls/prepared-test/"+cachedURL, nil)
	w := httptest.NewRecorder()
	if err := svc.ServeHLSSegment(w, r, "prepared-test", "seg_00000.m4s"); err == nil || w.Body.Len() != 0 {
		t.Fatal("an old playlist mixed fragments from the replacement package")
	}
}
