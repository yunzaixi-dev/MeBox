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
	// An explicit original or other track remains an original-file request.
	if request.MediaSourceId == m.ID || (request.AudioStreamIndex != nil && *request.AudioStreamIndex != 1) {
		return e.mediaSourcesForItem(ctx, m, false, e.directPlayOnly(ctx))
	}
	automatic := request.MediaSourceId == ""
	wantMP4 := request.MediaSourceId == m.ID+":mp4" || automatic && request.DeviceProfile != nil && len(request.DeviceProfile.DirectPlayProfiles) > 0
	wantMKV := request.MediaSourceId == m.ID+":mkv" || automatic && request.DeviceProfile != nil && len(request.DeviceProfile.DirectPlayProfiles) > 0
	wantHLS := request.MediaSourceId == m.ID+":hls" || automatic && request.DeviceProfile != nil && len(request.DeviceProfile.TranscodingProfiles) > 0
	if automatic && request.EnableDirectPlay != nil && !*request.EnableDirectPlay && request.EnableDirectStream != nil && !*request.EnableDirectStream {
		wantMP4 = false
		wantMKV = false
	}
	if automatic && request.EnableDirectStream != nil && !*request.EnableDirectStream && request.EnableTranscoding != nil && !*request.EnableTranscoding {
		wantHLS = false
	}
	var mp4Details, mkvDetails *preparedNativeMetadata
	if wantMP4 || wantHLS {
		mp4Details, _, _ = preparedNative(e.cfg, m, "mp4")
	}
	if wantMKV || wantHLS && mp4Details == nil {
		mkvDetails, _, _ = preparedNative(e.cfg, m, "mkv")
	}
	// HLS uses an admitted native package's decoder metadata.
	nativeDetails := mp4Details
	if nativeDetails == nil {
		nativeDetails = mkvDetails
	}
	// Try HLS first: a compatible HLS source always wins automatic negotiation.
	if wantHLS {
		if metadata, _, err := preparedHLS(e.cfg, m); err == nil {
			url := "/Videos/" + m.ID + "/master.m3u8?quality=prepared&MediaSourceId=" + m.ID + ":hls"
			prepared := *m
			prepared.AudioCodec = metadata.AudioCodec
			src := e.baseMediaSource(ctx, &prepared, "mp4", false, url, true)
			src["Id"], src["Name"], src["DirectStreamUrl"] = m.ID+":hls", MediaVersionLabel(*m)+" · 原画分片 VOD", url
			src["Path"], src["IsRemote"] = url, true
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
			native := preparedNativeMetadata{VideoCodec: metadata.VideoCodec, AudioCodec: metadata.AudioCodec}
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
			if !automatic {
				return []map[string]any{src}
			}
			if preparedClientSupports(request, m, &native, true, src["MediaStreams"].([]map[string]any)) {
				perftrace.Count(ctx, "playback.prepared_hls.selected")
				// Automatic negotiation keeps the item identity, not an alternative-source ID.
				src["Id"] = m.ID
				return []map[string]any{src}
			}
		}
	}
	for _, container := range []string{"mkv", "mp4"} {
		if container == "mkv" && !wantMKV || container == "mp4" && !wantMP4 {
			continue
		}
		nativeDetails := mp4Details
		if container == "mkv" {
			nativeDetails = mkvDetails
		}
		if nativeDetails == nil {
			continue
		}
		url := embyDirectStreamURL(m.ID, container) + "?MediaSourceId=" + m.ID + ":" + container
		src := e.baseMediaSource(ctx, m, container, false, url, true)
		src["Id"], src["Name"], src["DirectStreamUrl"] = m.ID, MediaVersionLabel(*m)+" · 原画原音轨快速 "+strings.ToUpper(container), url
		src["Path"], src["IsRemote"] = url, true
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
		src["Size"] = nativeDetails.asset.Size()
		streams := src["MediaStreams"].([]map[string]any)
		for _, stream := range streams {
			if stream["Type"] == "Video" {
				stream["BitDepth"], stream["Profile"], stream["Level"] = nativeDetails.VideoBitDepth, nativeDetails.VideoProfile, nativeDetails.VideoLevel
			}
			if stream["Type"] == "Audio" {
				stream["Channels"], stream["SampleRate"] = nativeDetails.AudioChannels, nativeDetails.AudioSampleRate
			}
		}
		if preparedClientSupports(request, m, nativeDetails, false, streams) {
			perftrace.Count(ctx, "playback.prepared_"+container+".selected")
			src["Id"] = m.ID
			return []map[string]any{src}
		}
	}
	// Enumerate original versions only when no prepared source was selected.
	originals := e.mediaSourcesForItem(ctx, m, false, e.directPlayOnly(ctx))
	if !automatic {
		for _, src := range originals {
			if src["Id"] == request.MediaSourceId {
				return []map[string]any{src}
			}
		}
		return nil
	}
	perftrace.Count(ctx, "playback.original.selected")
	return originals
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

func preparedClientSupports(request model.EmbyPlaybackInfoRequest, m *model.Media, metadata *preparedNativeMetadata, hls bool, streams []map[string]any) bool {
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
	subtitleIndex := -1
	if request.SubtitleStreamIndex != nil {
		subtitleIndex = *request.SubtitleStreamIndex
	} else {
		for _, stream := range streams {
			if stream["Type"] == "Subtitle" && stream["IsDefault"] == true {
				subtitleIndex, _ = stream["Index"].(int)
				break
			}
		}
	}
	if subtitleIndex >= 0 {
		compatible := false
		for _, stream := range streams {
			if stream["Type"] != "Subtitle" || stream["Index"] != subtitleIndex || stream["IsExternal"] != true {
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
	container := metadata.container
	if container == "" || hls {
		container = "mp4"
	}
	if hls {
		for _, p := range profile.TranscodingProfiles {
			if p.MaxAudioChannels != "" {
				limit, err := strconv.Atoi(p.MaxAudioChannels)
				if err != nil || limit <= 0 || metadata.AudioChannels == 0 || metadata.AudioChannels > limit {
					continue
				}
			}
			if strings.EqualFold(p.Type, "Video") && strings.EqualFold(p.Protocol, "hls") && codecListContains(p.Container, "mp4") && p.Container != "" &&
				codecListContains(p.VideoCodec, metadata.VideoCodec) && (metadata.AudioCodec == "" || codecListContains(p.AudioCodec, metadata.AudioCodec)) {
				matched = true
				break
			}
		}
	} else {
		for _, p := range profile.DirectPlayProfiles {
			if strings.EqualFold(p.Type, "Video") && codecListContains(p.Container, container) &&
				codecListContains(p.VideoCodec, metadata.VideoCodec) && (metadata.AudioCodec == "" || codecListContains(p.AudioCodec, metadata.AudioCodec)) {
				matched = true
				break
			}
		}
	}
	if !matched {
		return false
	}
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

func preparedConditionsMatch(conditions []model.EmbyProfileCondition, m *model.Media, metadata *preparedNativeMetadata) bool {
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
