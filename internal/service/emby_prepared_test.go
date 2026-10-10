package service

import (
	"github.com/truewhile/MeBox/internal/model"
	"testing"
	"time"
)

func TestEmbyPreparedNegotiationRespectsClientLimits(t *testing.T) {
	media := &model.Media{Width: 1920, Height: 1080, DurationSec: 100, SizeBytes: 10000000}
	metadata := &preparedMP4Metadata{VideoCodec: "av1", AudioCodec: "aac", AudioChannels: 6, AudioSampleRate: 48000, VideoBitDepth: 10, VideoProfile: "Main", VideoLevel: 8}
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
	req.DeviceProfile = nil
	if preparedClientSupports(req, media, metadata, false, nil) {
		t.Fatal("unknown capability was treated as permission to replace the original")
	}
}

func TestEmbyPreparedHLSRequiresFragmentedMP4AndExternalSubtitleSupport(t *testing.T) {
	media := &model.Media{DurationSec: 100, SizeBytes: 10000000}
	metadata := &preparedMP4Metadata{VideoCodec: "av1", AudioCodec: "aac", AudioChannels: 2}
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
