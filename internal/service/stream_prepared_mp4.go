package service

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/truewhile/MeBox/internal/config"
	"github.com/truewhile/MeBox/internal/model"
	"github.com/truewhile/MeBox/internal/perftrace"
)

// Prepared MP4 is an offline container conversion, never a request-time encode.
type preparedMP4Metadata struct {
	generation      string
	asset           os.FileInfo
	Version         int    `json:"version"`
	SourceSize      int64  `json:"source_size"`
	SourceMtimeNS   int64  `json:"source_mtime_ns"`
	VideoCodec      string `json:"video_codec"`
	AudioCodec      string `json:"audio_codec"`
	AudioChannels   int    `json:"audio_channels"`
	AudioSampleRate int    `json:"audio_sample_rate"`
	VideoProfile    string `json:"video_profile"`
	VideoLevel      int    `json:"video_level"`
	VideoBitDepth   int    `json:"video_bit_depth"`
	ColorTransfer   string `json:"color_transfer"`
	Codecs          string `json:"codecs"`
	AudioTranscoded bool   `json:"audio_transcoded"`
}

func preparedMP4(cfg *config.Config, m *model.Media) (*preparedMP4Metadata, string, error) {
	if cfg == nil || m == nil || cfg.Cache.CacheDir == "" || m.ID == "" ||
		m.ID == "." || m.ID == ".." || m.ID != filepath.Base(m.ID) ||
		strings.ContainsAny(m.ID, "/\\") || IsStrmMediaRow(m) || !filepath.IsAbs(m.Path) {
		return nil, "", ErrMediaNotFound
	}
	// Reject symlinks anywhere in source/package paths, not just the leaf.
	sourcePath, err := filepath.EvalSymlinks(m.Path)
	if err != nil || sourcePath != filepath.Clean(m.Path) {
		return nil, "", ErrMediaNotFound
	}
	dir, err := filepath.Abs(filepath.Join(cfg.Cache.CacheDir, "prepared-mp4", m.ID))
	if err != nil {
		return nil, "", ErrMediaNotFound
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || resolved != dir {
		return nil, "", ErrMediaNotFound
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", ErrMediaNotFound
	}
	defer root.Close()
	f, marker, err := preparedMP4File(root, "source.json")
	if err != nil {
		return nil, "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	f.Close()
	if err != nil || len(data) > 4096 {
		return nil, "", ErrMediaNotFound
	}
	var metadata preparedMP4Metadata
	if json.Unmarshal(data, &metadata) != nil || !metadata.valid(m) {
		return nil, "", ErrMediaNotFound
	}
	source, err := preparedRegularFile(m.Path)
	if err != nil || source.Size() != metadata.SourceSize || source.ModTime().UnixNano() != metadata.SourceMtimeNS {
		return nil, "", ErrMediaNotFound
	}
	asset, stat, err := preparedMP4File(root, "stream.mp4")
	if err != nil {
		return nil, "", err
	}
	asset.Close()
	if stat.Size() <= 0 {
		return nil, "", ErrMediaNotFound
	}
	metadata.generation = strconv.FormatInt(marker.ModTime().UnixNano(), 10)
	metadata.asset = stat
	return &metadata, dir, nil
}

func (metadata *preparedMP4Metadata) valid(m *model.Media) bool {
	if metadata.Version != 1 || metadata.AudioTranscoded ||
		metadata.VideoCodec != strings.ToLower(strings.TrimSpace(m.VideoCodec)) ||
		metadata.AudioCodec != strings.ToLower(strings.TrimSpace(m.AudioCodec)) ||
		metadata.AudioChannels < 0 || metadata.AudioChannels > 64 ||
		metadata.AudioSampleRate < 0 || metadata.AudioSampleRate > 768000 ||
		metadata.VideoLevel < 0 || metadata.VideoLevel > 255 ||
		metadata.VideoBitDepth < 0 || metadata.VideoBitDepth > 16 ||
		len(metadata.VideoProfile) > 64 || len(metadata.ColorTransfer) > 64 || len(metadata.Codecs) > 160 {
		return false
	}
	switch metadata.VideoCodec {
	case "h264", "hevc", "av1":
	default:
		return false
	}
	switch metadata.AudioCodec {
	case "":
		if metadata.AudioChannels != 0 || metadata.AudioSampleRate != 0 {
			return false
		}
	case "aac", "flac", "ac3", "eac3":
	default:
		return false
	}
	for _, text := range []string{metadata.VideoProfile, metadata.ColorTransfer, metadata.Codecs} {
		for _, c := range text {
			if c < 32 || c > 126 || c == '"' || c == '\\' {
				return false
			}
		}
	}
	// RFC 6381 is optional: never require an invented descriptor for native audio.
	if metadata.Codecs != "" {
		codecs := strings.Split(metadata.Codecs, ",")
		video := strings.TrimSpace(codecs[0])
		if (metadata.VideoCodec == "h264" && !strings.HasPrefix(video, "avc1.") && !strings.HasPrefix(video, "avc3.")) ||
			(metadata.VideoCodec == "hevc" && !strings.HasPrefix(video, "hvc1.") && !strings.HasPrefix(video, "hev1.")) ||
			(metadata.VideoCodec == "av1" && !strings.HasPrefix(video, "av01.")) {
			return false
		}
		if metadata.AudioCodec == "" {
			return len(codecs) == 1
		}
		if len(codecs) != 2 {
			return false
		}
		audio := strings.TrimSpace(codecs[1])
		return (metadata.AudioCodec == "aac" && strings.HasPrefix(audio, "mp4a.40.")) ||
			(metadata.AudioCodec == "ac3" && audio == "ac-3") ||
			(metadata.AudioCodec == "eac3" && audio == "ec-3")
	}
	return true
}

// Root.Open confines race-time symlinks; SameFile verifies the checked regular file.
func preparedMP4File(root *os.Root, name string) (*os.File, os.FileInfo, error) {
	checked, err := preparedRegularFile(filepath.Join(root.Name(), name))
	if err != nil {
		return nil, nil, err
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, nil, ErrMediaNotFound
	}
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || !os.SameFile(checked, stat) {
		f.Close()
		return nil, nil, ErrMediaNotFound
	}
	return f, stat, nil
}

// ServePreparedMP4 serves the validated offline asset; handlers retain authorization.
func (s *StreamService) ServePreparedMP4(w http.ResponseWriter, r *http.Request, mediaID string) error {
	ctx := r.Context()
	started := perftrace.Begin(ctx)
	defer perftrace.End(ctx, "stream.file", started)
	lookupStarted := perftrace.Begin(ctx)
	m, err := s.repo.Media.FindByID(ctx, mediaID)
	perftrace.End(ctx, "stream.file.lookup", lookupStarted)
	if err != nil {
		return err
	}
	metadata, dir, err := preparedMP4(s.cfg, m)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return ErrMediaNotFound
	}
	defer root.Close()
	f, stat, err := preparedMP4File(root, "stream.mp4")
	if err != nil || !os.SameFile(metadata.asset, stat) || metadata.asset.Size() != stat.Size() ||
		!metadata.asset.ModTime().Equal(stat.ModTime()) {
		if f != nil {
			f.Close()
		}
		return ErrMediaNotFound
	}
	defer f.Close()
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", `"prepared-mp4-`+metadata.generation+`"`)
	transferStarted := perftrace.Begin(ctx)
	// ETag is authoritative: date validators cannot distinguish a package rollback.
	http.ServeContent(w, r, "stream.mp4", time.Time{}, perftrace.Reader(ctx, f))
	perftrace.End(ctx, "stream.file.serve_content", transferStarted)
	return nil
}
