package service

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/truewhile/MeBox/internal/model"
	"github.com/truewhile/MeBox/internal/perftrace"
)

func (e *EmbyService) playbackMediaSources(ctx context.Context, m *model.Media, request model.EmbyPlaybackInfoRequest) []map[string]any {
	originals := e.mediaSourcesForItem(ctx, m, false, e.directPlayOnly(ctx))
	// An explicit original or other track remains an original-file request.
	if request.MediaSourceId == m.ID || (request.AudioStreamIndex != nil && *request.AudioStreamIndex != 1) {
		return originals
	}
	variants := make([]map[string]any, 0, 2)
	preferred := -1
	var nativeDetails *preparedMP4Metadata
	if metadata, _, err := preparedMP4(e.cfg, m); err == nil {
		nativeDetails = metadata
		url := embyDirectStreamURL(m.ID, "mp4") + "?MediaSourceId=" + m.ID + ":mp4"
		src := e.baseMediaSource(ctx, m, "mp4", false, url, true)
		src["Id"], src["Name"], src["DirectStreamUrl"] = m.ID+":mp4", MediaVersionLabel(*m)+" · 原画原音轨快速 MP4", url
		src["SupportsProbing"] = false
		if request.EnableDirectPlay != nil {
			src["SupportsDirectPlay"] = *request.EnableDirectPlay
		}
		if request.EnableDirectStream != nil {
			src["SupportsDirectStream"] = *request.EnableDirectStream
		}
		if request.SubtitleStreamIndex != nil {
			src["DefaultSubtitleStreamIndex"] = *request.SubtitleStreamIndex
		}
		src["Size"] = metadata.asset.Size()
		streams := src["MediaStreams"].([]map[string]any)
		for _, stream := range streams {
			if stream["Type"] == "Video" {
				stream["BitDepth"], stream["Profile"], stream["Level"] = metadata.VideoBitDepth, metadata.VideoProfile, metadata.VideoLevel
			}
			if stream["Type"] == "Audio" {
				stream["Channels"], stream["SampleRate"] = metadata.AudioChannels, metadata.AudioSampleRate
			}
		}
		if preparedClientSupports(request, m, metadata, false, streams) {
			preferred = 0
		}
		variants = append(variants, src)
	}
	if metadata, _, err := preparedHLS(e.cfg, m); err == nil {
		url := "/Videos/" + m.ID + "/master.m3u8?quality=prepared&MediaSourceId=" + m.ID + ":hls"
		prepared := *m
		prepared.AudioCodec = metadata.AudioCodec
		src := e.baseMediaSource(ctx, &prepared, "mp4", false, url, true)
		src["Id"], src["Name"], src["DirectStreamUrl"] = m.ID+":hls", MediaVersionLabel(*m)+" · 原画分片 VOD", url
		if metadata.AudioTranscoded {
			src["Name"] = src["Name"].(string) + " · AAC 兼容音频"
		}
		src["SupportsProbing"] = false
		src["SupportsDirectPlay"], src["SupportsDirectStream"], src["SupportsTranscoding"] = false, true, true
		src["TranscodingUrl"], src["TranscodingContainer"], src["TranscodingSubProtocol"] = url, "mp4", "hls"
		if request.EnableDirectStream != nil {
			src["SupportsDirectStream"] = *request.EnableDirectStream
		}
		if request.EnableTranscoding != nil {
			src["SupportsTranscoding"] = *request.EnableTranscoding
		}
		delete(src, "Size")
		if request.SubtitleStreamIndex != nil {
			src["DefaultSubtitleStreamIndex"] = *request.SubtitleStreamIndex
		}
		native := preparedMP4Metadata{VideoCodec: metadata.VideoCodec, AudioCodec: metadata.AudioCodec}
		if nativeDetails != nil {
			native.AudioChannels, native.AudioSampleRate = nativeDetails.AudioChannels, nativeDetails.AudioSampleRate
			native.VideoProfile, native.VideoLevel, native.VideoBitDepth = nativeDetails.VideoProfile, nativeDetails.VideoLevel, nativeDetails.VideoBitDepth
			native.ColorTransfer = nativeDetails.ColorTransfer
			if metadata.AudioTranscoded {
				native.AudioSampleRate = 0
			}
		}
		for _, stream := range src["MediaStreams"].([]map[string]any) {
			if stream["Type"] == "Video" && nativeDetails != nil {
				stream["BitDepth"], stream["Profile"], stream["Level"] = native.VideoBitDepth, native.VideoProfile, native.VideoLevel
			}
			if stream["Type"] == "Audio" {
				stream["Profile"] = "LC"
				if native.AudioChannels > 0 {
					stream["Channels"] = native.AudioChannels
				}
				if native.AudioSampleRate > 0 {
					stream["SampleRate"] = native.AudioSampleRate
				}
			}
		}
		if preparedClientSupports(request, m, &native, true, src["MediaStreams"].([]map[string]any)) {
			preferred = len(variants)
		}
		variants = append(variants, src)
	}
	if request.MediaSourceId != "" {
		for _, src := range variants {
			if src["Id"] == request.MediaSourceId {
				return []map[string]any{src}
			}
		}
		for _, src := range originals {
			if src["Id"] == request.MediaSourceId {
				return []map[string]any{src}
			}
		}
		return nil
	}
	// Unknown capability is not permission to change the client's default format.
	if preferred >= 0 {
		selected := variants[preferred]
		if selected["Id"] == m.ID+":hls" {
			perftrace.Count(ctx, "playback.prepared_hls.selected")
		} else {
			perftrace.Count(ctx, "playback.prepared_mp4.selected")
		}
		variants = append(variants[:preferred], variants[preferred+1:]...)
		out := make([]map[string]any, 0, 1+len(originals)+len(variants))
		out = append(out, selected)
		out = append(out, originals...)
		return append(out, variants...)
	}
	perftrace.Count(ctx, "playback.original.selected")
	return append(originals, variants...)
}

func codecListContains(list, codec string) bool {
	if list == "" {
		return true
	}
	for value := range strings.SplitSeq(list, ",") {
		if strings.EqualFold(strings.TrimSpace(value), codec) {
			return true
		}
	}
	return false
}

func preparedClientSupports(request model.EmbyPlaybackInfoRequest, m *model.Media, metadata *preparedMP4Metadata, hls bool, streams []map[string]any) bool {
	profile := request.DeviceProfile
	if profile == nil {
		return false
	}
	if hls && request.EnableDirectStream != nil && !*request.EnableDirectStream && request.EnableTranscoding != nil && !*request.EnableTranscoding {
		return false
	}
	if !hls && request.EnableDirectPlay != nil && !*request.EnableDirectPlay && request.EnableDirectStream != nil && !*request.EnableDirectStream {
		return false
	}
	if request.SubtitleStreamIndex != nil && *request.SubtitleStreamIndex >= 0 {
		compatible := false
		for _, stream := range streams {
			if stream["Type"] != "Subtitle" || stream["Index"] != *request.SubtitleStreamIndex || stream["IsExternal"] != true {
				continue
			}
			codec, _ := stream["Codec"].(string)
			for _, p := range profile.SubtitleProfiles {
				if strings.EqualFold(p.Method, "External") && strings.EqualFold(p.Format, codec) {
					compatible = true
					break
				}
			}
		}
		if !compatible {
			return false
		}
	}
	if request.MaxAudioChannels > 0 && (metadata.AudioChannels == 0 || metadata.AudioChannels > request.MaxAudioChannels) {
		return false
	}
	if request.MaxStreamingBitrate > 0 && (m.DurationSec <= 0 || m.SizeBytes*8/int64(m.DurationSec) > request.MaxStreamingBitrate) {
		return false
	}
	matched := false
	if hls {
		for _, p := range profile.TranscodingProfiles {
			if strings.EqualFold(p.Type, "Video") && strings.EqualFold(p.Protocol, "hls") && codecListContains(p.Container, "mp4") && p.Container != "" &&
				codecListContains(p.VideoCodec, metadata.VideoCodec) && (metadata.AudioCodec == "" || codecListContains(p.AudioCodec, metadata.AudioCodec)) &&
				(p.MaxAudioChannels == 0 || metadata.AudioChannels > 0 && metadata.AudioChannels <= p.MaxAudioChannels) {
				matched = true
				break
			}
		}
	} else {
		for _, p := range profile.DirectPlayProfiles {
			if strings.EqualFold(p.Type, "Video") && codecListContains(p.Container, "mp4") &&
				codecListContains(p.VideoCodec, metadata.VideoCodec) && (metadata.AudioCodec == "" || codecListContains(p.AudioCodec, metadata.AudioCodec)) {
				matched = true
				break
			}
		}
	}
	if !matched {
		return false
	}
	container := "mp4"
	for _, p := range profile.ContainerProfiles {
		if strings.EqualFold(p.Type, "Video") && codecListContains(p.Container, container) && !preparedConditionsMatch(p.Conditions, m, metadata) {
			return false
		}
	}
	for _, p := range profile.CodecProfiles {
		codec := metadata.VideoCodec
		if strings.EqualFold(p.Type, "VideoAudio") || strings.EqualFold(p.Type, "Audio") {
			codec = metadata.AudioCodec
		} else if !strings.EqualFold(p.Type, "Video") {
			continue
		}
		if codec != "" && codecListContains(p.Codec, codec) && codecListContains(p.Container, container) && !preparedConditionsMatch(p.Conditions, m, metadata) {
			return false
		}
	}
	limit := profile.MaxStaticBitrate
	if hls {
		limit = profile.MaxStreamingBitrate
	}
	return limit <= 0 || m.DurationSec > 0 && m.SizeBytes*8/int64(m.DurationSec) <= int64(limit)
}

func preparedConditionsMatch(conditions []model.EmbyProfileCondition, m *model.Media, metadata *preparedMP4Metadata) bool {
	for _, condition := range conditions {
		var value string
		switch strings.ToLower(condition.Property) {
		case "width":
			if m.Width > 0 {
				value = strconv.Itoa(m.Width)
			}
		case "height":
			if m.Height > 0 {
				value = strconv.Itoa(m.Height)
			}
		case "videobitdepth":
			if metadata.VideoBitDepth > 0 {
				value = strconv.Itoa(metadata.VideoBitDepth)
			}
		case "videolevel":
			if metadata.VideoLevel > 0 {
				value = strconv.Itoa(metadata.VideoLevel)
			}
		case "videoprofile":
			value = metadata.VideoProfile
		case "audiochannels":
			if metadata.AudioChannels > 0 {
				value = strconv.Itoa(metadata.AudioChannels)
			}
		case "audiosamplerate":
			if metadata.AudioSampleRate > 0 {
				value = strconv.Itoa(metadata.AudioSampleRate)
			}
		case "videorangetype":
			switch metadata.ColorTransfer {
			case "smpte2084":
				value = "HDR10"
			case "arib-std-b67":
				value = "HLG"
			case "bt709", "smpte170m", "bt470bg":
				value = "SDR"
			}
		}
		if value == "" {
			if condition.IsRequired {
				return false
			}
			continue
		}
		switch strings.ToLower(condition.Condition) {
		case "equals":
			if !strings.EqualFold(condition.Value, value) {
				return false
			}
		case "notequals":
			if strings.EqualFold(condition.Value, value) {
				return false
			}
		case "equalsany":
			if condition.Value == "" || !codecListContains(condition.Value, value) {
				return false
			}
		case "lessthanequal", "greaterthanequal":
			actual, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return false
			}
			limit, err := strconv.ParseFloat(condition.Value, 64)
			if err != nil || math.IsNaN(limit) || math.IsInf(limit, 0) {
				return false
			}
			if strings.EqualFold(condition.Condition, "LessThanEqual") && actual > limit || strings.EqualFold(condition.Condition, "GreaterThanEqual") && actual < limit {
				return false
			}
		default:
			return false
		}
	}
	return true
}
