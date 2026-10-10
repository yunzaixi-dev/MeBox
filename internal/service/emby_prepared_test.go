package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/truewhile/MeBox/internal/model"
)

func TestEmbyPreparedNegotiationRespectsClientLimits(t *testing.T) {
	media := &model.Media{Width: 1920, Height: 1080, DurationSec: 100, SizeBytes: 10000000}
	metadata := &preparedNativeMetadata{VideoCodec: "av1", AudioCodec: "aac", AudioChannels: 6, AudioSampleRate: 48000, VideoBitDepth: 10, VideoProfile: "Main", VideoLevel: 8}
	profile := &model.EmbyDeviceProfile{DirectPlayProfiles: []model.EmbyDirectPlayProfile{{Type: "Video", Container: "mp4", VideoCodec: "av1", AudioCodec: "aac"}}}
	req := model.EmbyPlaybackInfoRequest{DeviceProfile: profile}
	if !preparedClientSupports(req, media, metadata, false, nil) {
		t.Fatal("declared native MP4 capability was ignored")
	}
	req.MaxAudioChannels = 2
	if preparedClientSupports(req, media, metadata, false, nil) {
		t.Fatal("six-channel audio exceeded the client limit")
	}
	req.MaxAudioChannels = 0
	profile.CodecProfiles = []model.EmbyCodecProfile{{Type: "Video", Codec: "av1", Conditions: []model.EmbyProfileCondition{{Property: "VideoBitDepth", Condition: "LessThanEqual", Value: "8", IsRequired: true}}}}
	if preparedClientSupports(req, media, metadata, false, nil) {
		t.Fatal("10-bit video was selected for an 8-bit decoder")
	}
	metadata.VideoBitDepth = 0
	if preparedClientSupports(req, media, metadata, false, nil) {
		t.Fatal("missing required decoder metadata was treated as support")
	}
	metadata.VideoBitDepth = 10
	profile.CodecProfiles[0].Conditions = []model.EmbyProfileCondition{{Property: "VideoBitDepth", Condition: "GreaterThanEqual", Value: "NaN"}}
	if preparedClientSupports(req, media, metadata, false, nil) {
		t.Fatal("non-finite decoder condition was accepted")
	}
	metadata.ColorTransfer = "smpte2084"
	profile.CodecProfiles[0].Conditions = []model.EmbyProfileCondition{{Property: "VideoRangeType", Condition: "Equals", Value: "SDR", IsRequired: true}}
	if preparedClientSupports(req, media, metadata, false, nil) {
		t.Fatal("HDR source was selected for an SDR-only decoder")
	}
	profile.CodecProfiles = nil
	req.MaxStreamingBitrate = 100000
	if preparedClientSupports(req, media, metadata, false, nil) {
		t.Fatal("bitrate ceiling was ignored")
	}
	req.MaxStreamingBitrate = 0
	profile.DirectPlayProfiles[0].VideoCodec = "h264"
	if preparedClientSupports(req, media, metadata, false, nil) {
		t.Fatal("AV1 was selected for an H264-only device")
	}
	req.DeviceProfile = &model.EmbyDeviceProfile{DirectPlayProfiles: []model.EmbyDirectPlayProfile{{Type: "Video"}}}
	for _, container := range []string{"mp4", "mkv"} {
		metadata.container = container
		if !preparedClientSupports(req, media, metadata, false, nil) {
			t.Fatalf("declared unrestricted native capability rejected %s", container)
		}
	}
	req.DeviceProfile = nil
	if preparedClientSupports(req, media, metadata, false, nil) {
		t.Fatal("unknown capability was treated as permission to replace the original")
	}
}

func TestEmbyPreparedHLSRequiresFragmentedMP4AndExternalSubtitleSupport(t *testing.T) {
	media := &model.Media{DurationSec: 100, SizeBytes: 10000000}
	metadata := &preparedNativeMetadata{VideoCodec: "av1", AudioCodec: "aac", AudioChannels: 2}
	profile := &model.EmbyDeviceProfile{TranscodingProfiles: []model.EmbyTranscodingProfile{{Type: "Video", Protocol: "hls", Container: "ts", VideoCodec: "av1", AudioCodec: "aac"}}}
	request := model.EmbyPlaybackInfoRequest{DeviceProfile: profile}
	if preparedClientSupports(request, media, metadata, true, nil) {
		t.Fatal("MPEG-TS capability was mistaken for fMP4 HLS support")
	}
	profile.TranscodingProfiles[0].Container = "mp4"
	if !preparedClientSupports(request, media, metadata, true, nil) {
		t.Fatal("fMP4 HLS capability was ignored")
	}
	for _, tc := range []struct {
		limit     string
		supported bool
	}{{"2", true}, {"1", false}, {"", true}, {"invalid", false}, {"-1", false}, {"0", false}} {
		profile.TranscodingProfiles[0].MaxAudioChannels = tc.limit
		if got := preparedClientSupports(request, media, metadata, true, nil); got != tc.supported {
			t.Fatalf("standard MaxAudioChannels %q: compatible=%v, want %v", tc.limit, got, tc.supported)
		}
	}
	profile.TranscodingProfiles[0].MaxAudioChannels = ""
	no := false
	request.EnableDirectPlay, request.EnableDirectStream = &no, &no
	if preparedClientSupports(request, media, metadata, false, nil) {
		t.Fatal("client's disabled direct paths were ignored")
	}
	if !preparedClientSupports(request, media, metadata, true, nil) {
		t.Fatal("prebuilt HLS was excluded for a streaming-only client")
	}
	index := 2
	request.SubtitleStreamIndex = &index
	subtitles := []map[string]any{{"Type": "Subtitle", "Index": 2, "Codec": "ass", "IsExternal": true}}
	profile.SubtitleProfiles = []model.EmbySubtitleProfile{{Format: "ass", Method: "Encode"}}
	if preparedClientSupports(request, media, metadata, true, subtitles) {
		t.Fatal("burn-in-only subtitles were offered on a copy-only stream")
	}
	profile.SubtitleProfiles[0].Method = "External"
	if !preparedClientSupports(request, media, metadata, true, subtitles) {
		t.Fatal("supported external subtitles prevented optimized playback")
	}
	request.SubtitleStreamIndex = nil
	subtitles[0]["IsDefault"] = true
	profile.SubtitleProfiles[0].Method = "Encode"
	if preparedClientSupports(request, media, metadata, true, subtitles) {
		t.Fatal("default Chinese subtitle was silently lost for a burn-in-only client")
	}
	request.SubtitleStreamIndex = new(-1)
	if !preparedClientSupports(request, media, metadata, true, subtitles) {
		t.Fatal("explicitly disabled subtitles prevented prebuilt playback")
	}
}

func TestEmbyPreparedHLSFallbackKeepsNativeAudioMetadata(t *testing.T) {
	_, cfg, repos, _, directory := preparedMP4Fixture(t)
	media, err := repos.Media.FindByID(t.Context(), "prepared-mp4-test")
	if err != nil || media == nil {
		t.Fatalf("media: %v", err)
	}
	native, _, err := preparedNative(cfg, media, "mp4")
	if err != nil {
		t.Fatal(err)
	}
	media.AudioCodec = "flac"
	native.AudioCodec, native.Codecs = "flac", ""
	data, err := json.Marshal(native)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "source.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	hls := preparedHLSMetadata{
		Version: 1, SourceSize: native.SourceSize, SourceMtimeNS: native.SourceMtimeNS,
		VideoCodec: "h264", AudioCodec: "aac",
		preparedHLSInfo: preparedHLSInfo{Codecs: "avc1.640028,mp4a.40.2", AudioTranscoded: true},
	}
	data, err = json.Marshal(hls)
	if err != nil {
		t.Fatal(err)
	}
	directory = filepath.Join(cfg.Cache.CacheDir, "prepared-hls", media.ID)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"source.json": data, "index.m3u8": []byte("#EXTM3U\n"), "init.mp4": []byte("init")} {
		if err := os.WriteFile(filepath.Join(directory, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	svc := NewEmbyService(cfg, nil, repos)
	for _, tc := range []struct {
		container, source, codec string
	}{{"ts", ":mp4", "flac"}, {"mp4", ":hls", "aac"}} {
		t.Run(tc.container, func(t *testing.T) {
			request := model.EmbyPlaybackInfoRequest{DeviceProfile: &model.EmbyDeviceProfile{
				DirectPlayProfiles:  []model.EmbyDirectPlayProfile{{Type: "Video", Container: "mp4", VideoCodec: "h264", AudioCodec: "flac"}},
				TranscodingProfiles: []model.EmbyTranscodingProfile{{Type: "Video", Protocol: "hls", Container: tc.container, VideoCodec: "h264", AudioCodec: "aac"}},
			}}
			sources := svc.playbackMediaSources(t.Context(), media, request)
			if len(sources) != 1 || sources[0]["Id"] != media.ID || !strings.Contains(sources[0]["DirectStreamUrl"].(string), "MediaSourceId="+media.ID+tc.source) {
				t.Fatalf("selected source: %#v", sources)
			}
			audio := sources[0]["MediaStreams"].([]map[string]any)[1]
			if audio["Codec"] != tc.codec || audio["Channels"] != 2 || media.AudioCodec != "flac" {
				t.Fatalf("audio metadata changed: %#v, original=%s", audio, media.AudioCodec)
			}
			if tc.source == ":mp4" {
				if audio["SampleRate"] != 48000 || audio["Profile"] != nil {
					t.Fatalf("native audio inherited HLS metadata: %#v", audio)
				}
			} else if audio["SampleRate"] != nil || !strings.Contains(sources[0]["Name"].(string), "AAC 兼容音频") {
				t.Fatalf("converted audio metadata/disclosure: %#v", sources[0])
			}
		})
	}
}

func TestSessionDeviceProfileIsIsolatedPreservedAndExpires(t *testing.T) {
	tracker := NewSessionTrackerService(nil)
	profile := &model.EmbyDeviceProfile{Name: "device-a"}
	tracker.RecordActivity(t.Context(), "u", "", "a", "Phone", "client", "127.0.0.1")
	tracker.SetDeviceProfile("u", "a", "Phone", "client", "127.0.0.1", profile)
	tracker.RecordActivity(t.Context(), "u", "", "a", "Phone", "client", "127.0.0.1")
	if tracker.DeviceProfile("u", "a", "Phone", "client", "127.0.0.1") != profile {
		t.Fatal("activity erased negotiated capability")
	}
	if tracker.DeviceProfile("other", "a", "Phone", "client", "127.0.0.1") != nil {
		t.Fatal("capability leaked between users")
	}
	tracker.RecordActivity(t.Context(), "u", "", "b", "Phone", "client", "127.0.0.1")
	if tracker.DeviceProfile("u", "b", "Phone", "client", "127.0.0.1") != nil {
		t.Fatal("same device name inherited a different device's capabilities")
	}
	tracker.SetDeviceProfile("u", "b", "Phone", "client", "127.0.0.1", profile)
	now := tracker.now()
	tracker.now = func() time.Time { return now.Add(realtimeSessionTTL + time.Second) }
	if tracker.DeviceProfile("u", "b", "Phone", "client", "127.0.0.1") != nil {
		t.Fatal("expired capability continued selecting prepared media")
	}
}
