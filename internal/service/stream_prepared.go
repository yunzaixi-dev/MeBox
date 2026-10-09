package service

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/truewhile/MeBox/internal/config"
	"github.com/truewhile/MeBox/internal/model"
	"github.com/truewhile/MeBox/internal/perftrace"
)

// A prepared package never launches FFmpeg or changes the media's identity.
// Its original video is copied; optional compatible audio is disclosed.
type preparedHLSInfo struct {
	Codecs          string `json:"codecs"`
	AudioTranscoded bool   `json:"audio_transcoded"`
}

type preparedHLSMetadata struct {
	generation    string
	Version       int    `json:"version"`
	SourceSize    int64  `json:"source_size"`
	SourceMtimeNS int64  `json:"source_mtime_ns"`
	VideoCodec    string `json:"video_codec"`
	AudioCodec    string `json:"audio_codec"`
	preparedHLSInfo
}

func preparedHLS(cfg *config.Config, m *model.Media) (*preparedHLSMetadata, string, error) {
	if cfg == nil || m == nil || cfg.Cache.CacheDir == "" || m.ID == "" ||
		m.ID == "." || m.ID == ".." || m.ID != filepath.Base(m.ID) ||
		m.STRMURL != "" || !filepath.IsAbs(m.Path) {
		return nil, "", ErrMediaNotFound
	}
	dir := filepath.Join(cfg.Cache.CacheDir, "prepared-hls", m.ID)
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, "", ErrMediaNotFound
	}
	marker, err := preparedRegularFile(filepath.Join(dir, "source.json"))
	if err != nil {
		return nil, "", ErrMediaNotFound
	}
	f, err := os.Open(filepath.Join(dir, "source.json"))
	if err != nil {
		return nil, "", ErrMediaNotFound
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(data) > 4096 {
		return nil, "", ErrMediaNotFound
	}
	var metadata preparedHLSMetadata
	if json.Unmarshal(data, &metadata) != nil || metadata.Version != 1 ||
		metadata.VideoCodec != strings.ToLower(strings.TrimSpace(m.VideoCodec)) ||
		(metadata.AudioCodec != "" && metadata.AudioCodec != "aac") ||
		metadata.Codecs == "" || len(metadata.Codecs) > 160 ||
		strings.ContainsAny(metadata.Codecs, "\r\n\"\\") {
		return nil, "", ErrMediaNotFound
	}
	metadata.generation = strconv.FormatInt(marker.ModTime().UnixNano(), 10)
	source, err := os.Stat(m.Path)
	if err != nil || !source.Mode().IsRegular() || source.Size() != metadata.SourceSize ||
		source.ModTime().UnixNano() != metadata.SourceMtimeNS {
		return nil, "", ErrMediaNotFound
	}
	for _, name := range []string{"index.m3u8", "init.mp4"} {
		if _, err := preparedRegularFile(filepath.Join(dir, name)); err != nil {
			return nil, "", ErrMediaNotFound
		}
	}
	return &metadata, dir, nil
}

func preparedRegularFile(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, ErrMediaNotFound
	}
	return info, nil
}

func preparedSegmentName(name string) bool {
	if !strings.HasPrefix(name, "seg_") || !strings.HasSuffix(name, ".m4s") {
		return false
	}
	digits := name[4 : len(name)-4]
	if len(digits) < 5 {
		return false
	}
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func preparedPlaylist(data []byte, rawQuery, generation string) (string, error) {
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", errors.New("invalid playback query")
	}
	q.Del("start")
	q.Del("_seek")
	q.Set("quality", "prepared")
	q.Set("prepared_v", generation)
	query := "?" + q.Encode()
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "#EXTM3U" {
		return "", errors.New("invalid prepared playlist")
	}
	hasMap, hasEnd, hasSegment := false, false, false
	for i, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			if line != "#EXT-X-MAP:URI=\"init.mp4\"" {
				return "", errors.New("invalid prepared initialization")
			}
			lines[i] = "#EXT-X-MAP:URI=\"init.mp4" + query + "\""
			hasMap = true
		case strings.Contains(line, "URI="):
			return "", errors.New("unexpected prepared asset URI")
		case line == "#EXT-X-ENDLIST":
			hasEnd = true
		case line == "" || strings.HasPrefix(line, "#"):
		default:
			if !preparedSegmentName(line) {
				return "", errors.New("invalid prepared fragment")
			}
			lines[i] = line + query
			hasSegment = true
		}
	}
	if !hasMap || !hasEnd || !hasSegment {
		return "", errors.New("incomplete prepared playlist")
	}
	return strings.Join(lines, "\n"), nil
}

func (s *StreamService) servePreparedHLS(w http.ResponseWriter, r *http.Request, mediaID, asset string) error {
	if asset != "index.m3u8" && asset != "init.mp4" && !preparedSegmentName(asset) {
		return ErrMediaNotFound
	}
	m, err := s.repo.Media.FindByID(r.Context(), mediaID)
	if err != nil {
		return err
	}
	metadata, dir, err := preparedHLS(s.cfg, m)
	if err != nil {
		return err
	}
	if asset != "index.m3u8" && r.URL.Query().Get("prepared_v") != metadata.generation {
		return ErrMediaNotFound
	}
	path := filepath.Join(dir, asset)
	stat, err := preparedRegularFile(path)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return ErrMediaNotFound
	}
	defer f.Close()
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if asset == "index.m3u8" {
		data, err := io.ReadAll(io.LimitReader(perftrace.Reader(r.Context(), f), 4*1024*1024+1))
		if err != nil || len(data) > 4*1024*1024 {
			return errors.New("prepared playlist too large or unreadable")
		}
		rawQuery := r.URL.RawQuery
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			q, err := url.ParseQuery(rawQuery)
			if err != nil {
				return errors.New("invalid playback query")
			}
			// Browser HLS sends current shared-client credentials in headers.
			// Do not repeat a long JWT thousands of times in the VOD index.
			q.Del("token")
			q.Del("api_key")
			rawQuery = q.Encode()
		}
		playlist, err := preparedPlaylist(data, rawQuery, metadata.generation)
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, err = io.WriteString(w, playlist)
		return err
	}
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(w, r, stat.Name(), stat.ModTime(), perftrace.Reader(r.Context(), f))
	return nil
}
