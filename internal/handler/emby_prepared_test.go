package handler

import (
	"encoding/json"
	"github.com/truewhile/MeBox/internal/model"
	"github.com/truewhile/MeBox/internal/service"
	"go.uber.org/zap"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbyPreparedMP4NegotiatesAndStreamsThroughAllRouteShapes(t *testing.T) {
	router, svc, secret := newPlaybackScopeTestRouter(t)
	svc.Cfg.Cache.CacheDir = t.TempDir()
	svc.Emby = service.NewEmbyService(svc.Cfg, zap.NewNop(), svc.Repo)
	svc.Sessions = service.NewSessionTrackerService(zap.NewNop())
	source := filepath.Join(t.TempDir(), "original.mkv")
	if err := os.WriteFile(source, []byte("original-file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := svc.Repo.DB.Model(&model.Media{}).Where("id = ?", "media-1").Updates(map[string]any{"path": source, "strm_url": "", "video_codec": "h264", "audio_codec": "aac", "container": "mkv"}).Error; err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(svc.Cfg.Cache.CacheDir, "prepared-mp4", "media-1")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal(map[string]any{"version": 1, "source_size": info.Size(), "source_mtime_ns": info.ModTime().UnixNano(), "video_codec": "h264", "audio_codec": "aac", "audio_channels": 2, "audio_sample_rate": 48000, "video_bit_depth": 8})
	if err := os.WriteFile(filepath.Join(directory, "source.json"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "stream.mp4"), []byte("faststart-asset"), 0600); err != nil {
		t.Fatal(err)
	}
	registerEmbyRoutes(router, secret, svc)
	token := signedTestToken(t, secret)
	capabilities := `{"DeviceProfile":{"DirectPlayProfiles":[{"Type":"Video","Container":"mp4","VideoCodec":"h264","AudioCodec":"aac"}]}}`
	request := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Capabilities/Full?DeviceId=test-device", strings.NewReader(capabilities))
	request.Header.Set("X-Emby-Token", token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("capabilities: %d %s", response.Code, response.Body.String())
	}
	for _, path := range []string{"/Items/media-1/PlaybackInfo", "/emby/items/media-1/playbackinfo", "/emby/Users/user-1/Items/media-1/PlaybackInfo"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(method+path, func(t *testing.T) {
				request := httptest.NewRequest(method, path+"?DeviceId=test-device", nil)
				request.Header.Set("X-Emby-Token", token)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				var result struct{ MediaSources []map[string]any }
				if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil {
					t.Fatalf("playback: %d %s", response.Code, response.Body.String())
				}
				if len(result.MediaSources) != 2 || result.MediaSources[0]["Id"] != "media-1:mp4" || result.MediaSources[1]["Id"] != "media-1" {
					t.Fatalf("optimized/default/original negotiation: %#v", result.MediaSources)
				}
				direct := result.MediaSources[0]["DirectStreamUrl"].(string)
				uri, err := url.Parse(direct)
				if err != nil {
					t.Fatal(err)
				}
				if uri.Query().Get("api_key") != token {
					t.Fatal("external-player media lost authentication")
				}
				get := httptest.NewRequest(http.MethodGet, "/emby"+direct, nil)
				get.Header.Set("Range", "bytes=0-3")
				body := httptest.NewRecorder()
				router.ServeHTTP(body, get)
				if body.Code != 206 || body.Body.String() != "fast" {
					t.Fatalf("MP4 variant did not serve exact prepared range: %d %q", body.Code, body.Body.String())
				}
			})
		}
	}
	for _, query := range []string{"DeviceId=unknown", "DeviceId=test-device&MediaSourceId=media-1", "DeviceId=test-device&AudioStreamIndex=2"} {
		request = httptest.NewRequest(http.MethodGet, "/emby/Items/media-1/PlaybackInfo?"+query, nil)
		request.Header.Set("X-Emby-Token", token)
		response = httptest.NewRecorder()
		router.ServeHTTP(response, request)
		var result struct{ MediaSources []map[string]any }
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.MediaSources[0]["Id"] != "media-1" {
			t.Fatalf("original default/explicit choice changed: %d %s", response.Code, response.Body.String())
		}
	}
	// Explicit original selection must still serve the original bytes, not an alias.
	request = httptest.NewRequest(http.MethodGet, "/emby/Videos/media-1/stream.mkv?MediaSourceId=media-1&api_key="+token, nil)
	request.Header.Set("Range", "bytes=0-7")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 206 || response.Body.String() != "original" {
		t.Fatalf("original track access changed: %d %q", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/emby/Videos/media-1/stream.mp4?MediaSourceId=media-1:mp4", nil)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatalf("prepared variant bypassed auth: %d", response.Code)
	}
	for _, path := range []string{"/emby/Items/media-1/PlaybackInfo?MediaSourceId=retired:mp4", "/emby/Videos/media-1/stream.mp4?MediaSourceId=retired:mp4"} {
		request = httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("X-Emby-Token", token)
		response = httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != 404 {
			t.Fatalf("missing explicit source was silently replaced: %d", response.Code)
		}
	}
}
