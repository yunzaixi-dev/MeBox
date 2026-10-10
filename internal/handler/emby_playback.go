package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/truewhile/MeBox/internal/model"
	"github.com/truewhile/MeBox/internal/service"
)

func embyPlaybackInfoHandler(svc *service.Container) gin.HandlerFunc {
	return func(c *gin.Context) {
		uid := embyEffectiveUserID(c)
		request, err := embyPlaybackRequest(c)
		if err != nil {
			embyError(c, http.StatusBadRequest, "invalid playback options")
			return
		}
		if request.DeviceProfile == nil && svc.Sessions != nil {
			device := embyClientInfoFromRequest(c)
			request.DeviceProfile = svc.Sessions.DeviceProfile(uid, device.DeviceID, device.DeviceName, device.Client, c.ClientIP())
		}
		out, err := svc.Emby.PlaybackInfo(c.Request.Context(), c.Param("id"), uid, request)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if out == nil {
			embyError(c, http.StatusNotFound, "not found")
			return
		}
		preparedMP4BaseURL := ""
		if svc.Cfg != nil {
			preparedMP4BaseURL = svc.Cfg.PreparedMP4BaseURL
		}
		embyAttachRequestTokenToMediaSources(c, out, preparedMP4BaseURL)
		// 在后台把本次条目的云盘直链换好：播放器拿到 PlaybackInfo 后通常还要
		// 1–2 秒才请求 /Videos/{id}/stream，把换链开销落在这段等待里。
		embyPrewarmPlaybackTargets(svc, c, out)
		c.JSON(http.StatusOK, out)
	}
}

func embyPlaybackRequest(c *gin.Context) (model.EmbyPlaybackInfoRequest, error) {
	var request model.EmbyPlaybackInfoRequest
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20))
		if err := decoder.Decode(&request); err != nil && !errors.Is(err, io.EOF) {
			return request, err
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			return request, errors.New("invalid playback body")
		}
	}
	for key, values := range c.Request.URL.Query() {
		if len(values) != 1 {
			continue
		}
		value := values[0]
		switch strings.ToLower(key) {
		case "enabledirectplay", "enabledirectstream", "enabletranscoding":
			flag, err := strconv.ParseBool(value)
			if err != nil {
				return request, err
			}
			if strings.EqualFold(key, "EnableDirectPlay") {
				request.EnableDirectPlay = &flag
			} else if strings.EqualFold(key, "EnableTranscoding") {
				request.EnableTranscoding = &flag
			} else {
				request.EnableDirectStream = &flag
			}
		case "audiostreamindex", "subtitlestreamindex":
			index, err := strconv.Atoi(value)
			if err != nil || index < -1 {
				return request, errors.New("invalid stream index")
			}
			if strings.EqualFold(key, "AudioStreamIndex") {
				request.AudioStreamIndex = &index
			} else {
				request.SubtitleStreamIndex = &index
			}
		case "maxstreamingbitrate", "maxaudiochannels":
			limit, err := strconv.ParseInt(value, 10, 32)
			if err != nil || limit < 0 {
				return request, errors.New("invalid playback limit")
			}
			if strings.EqualFold(key, "MaxStreamingBitrate") {
				request.MaxStreamingBitrate = limit
			} else {
				request.MaxAudioChannels = int(limit)
			}
		case "mediasourceid":
			request.MediaSourceId = value
		}
	}
	if request.MaxAudioChannels < 0 || request.MaxStreamingBitrate < 0 || len(request.MediaSourceId) > 128 ||
		(request.AudioStreamIndex != nil && *request.AudioStreamIndex < -1) ||
		(request.SubtitleStreamIndex != nil && *request.SubtitleStreamIndex < -1) {
		return request, errors.New("invalid playback options")
	}
	return request, nil
}

// embyPrewarmTimeout 是单次预热的等待上限。115 开放平台在跨太平洋线路上单次
// 换链实测 0.4–1.1s，这里给足余量；超时只是没预热成功，不影响后续播放。
const embyPrewarmTimeout = 10 * time.Second

// embyPrewarmInFlight 去重同一个条目的并发预热（首页刷新会并发请求多个接口，
// 同一条目可能在短时间内被多次请求）。
var embyPrewarmInFlight sync.Map

// embyPrewarmSlots 限制同时进行的预热数量。客户端可能批量预取 PlaybackInfo
// （逐个剧集的预取请求），预热只是优化，不能反过来把 115 换链接口打出突发。
// 名额满时直接跳过：排队等待的预热往往等真正播放时已经没意义了。
var embyPrewarmSlots = make(chan struct{}, 4)

// embyPrewarmPlaybackTargets 在后台预热本次 PlaybackInfo 涉及条目的云盘直链。
//
// 起播链路里最贵的一步是「服务端拿 pickcode 去 115 开放平台换直链」：服务器在
// 洛杉矶、115 接口在国内，冷启动实测 0.4–1.1s；之后 45 分钟内命中进程内缓存。
// 播放器在 PlaybackInfo 与真正拉流之间有几秒间隔，这里把换链放到那段间隔里，
// 起播时就只剩纯网络耗时。
//
// 只处理云盘/strm 条目，且失败一律静默忽略：预热是尽力而为的优化，不能影响
// PlaybackInfo 的正常返回。
func embyPrewarmPlaybackTargets(svc *service.Container, c *gin.Context, out map[string]any) {
	if svc == nil || svc.Strm == nil || svc.Repo == nil || svc.Repo.Media == nil || out == nil {
		return
	}
	ids := embyPrewarmMediaIDs(out)
	if len(ids) == 0 {
		return
	}
	userAgent := c.GetHeader("User-Agent")
	// 预热是给「后续请求」用的：即便本次 PlaybackInfo 的连接断开，
	// 也要把换链跑完。
	base := context.WithoutCancel(c.Request.Context())
	for _, mediaID := range ids {
		if _, loaded := embyPrewarmInFlight.LoadOrStore(mediaID, struct{}{}); loaded {
			continue
		}
		go func(id string) {
			defer embyPrewarmInFlight.Delete(id)
			select {
			case embyPrewarmSlots <- struct{}{}:
				defer func() { <-embyPrewarmSlots }()
			default:
				return
			}
			ctx, cancel := context.WithTimeout(base, embyPrewarmTimeout)
			defer cancel()
			m, err := svc.Repo.Media.FindByID(ctx, id)
			if err != nil || m == nil {
				return
			}
			// 与播放路径（StreamService.ServeFileWithCloudMode）同一套目标解析：
			// STRMURL 为空时回读 .strm 文件内容，预热才能覆盖同一批条目。
			raw := service.MediaSTRMTarget(m)
			if raw == "" || !service.IsStrmMediaRow(m) {
				return
			}
			// 解析结果由 strm 层按 pickcode+UA 缓存；已缓存时这里是空转。
			_, _ = svc.Strm.ResolvePlayTargetWithUA(ctx, raw, userAgent)
		}(mediaID)
	}
}

// embyPrewarmMediaIDs 取出 PlaybackInfo 载荷里 MediaSources 的条目 ID。
func embyPrewarmMediaIDs(out map[string]any) []string {
	sources, ok := out["MediaSources"].([]map[string]any)
	if !ok || len(sources) == 0 {
		return nil
	}
	ids := make([]string, 0, len(sources))
	seen := make(map[string]struct{}, len(sources))
	for _, src := range sources {
		id, _ := src["Id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// embySubtitleStreamHandler serves an external subtitle track advertised in a
// MediaSource's MediaStreams via its Emby index
// (/Videos/:id/Subtitles/:index/Stream). The index maps to a discovered
// sideloaded subtitle track next to the video (SRT/ASS/SSA/VTT, local or
// cloud://), following the same layout appended by mediaStreams. 远程 Emby
// 条目的字幕直接反向代理远程。
func embySubtitleStreamHandler(svc *service.Container) gin.HandlerFunc {
	return func(c *gin.Context) {
		encodedID := c.Param("id")
		if accountID, remoteID, ok := service.DecodeEmbyRemoteID(encodedID); ok {
			if err := svc.Emby.ProxyRemoteSubtitle(c.Request.Context(), c.Writer, c.Request, accountID, remoteID, c.Param("index")); err != nil {
				embyError(c, http.StatusNotFound, "subtitle not found")
				return
			}
			return
		}
		uid := embyEffectiveUserID(c)
		ctx := c.Request.Context()
		// The official-format route carries a :format suffix (Stream.ass /
		// Stream.vtt); prefer it for the Content-Type when present, otherwise
		// fall back to the discovered source codec. :mediaSourceId is ignored —
		// the item is located by :id and subtitles by :index (1:1 in this shim).
		format := strings.TrimSpace(c.Param("format"))
		codec := svc.Emby.SubtitleStreamCodec(ctx, c.Param("id"), c.Param("index"), uid)
		if codec != "" {
			if format != "" {
				codec = service.SubtitleCodecFromFormat(format)
			}
			c.Header("Content-Type", service.SubtitleContentType(codec))
		}
		c.Header("Cache-Control", "public, max-age=3600")
		if err := svc.Emby.ServeSubtitleStream(ctx, c.Writer, c.Param("id"), c.Param("index"), uid); err != nil {
			embyError(c, http.StatusNotFound, "subtitle not found")
			return
		}
	}
}

func embyAttachRequestTokenToMediaSources(c *gin.Context, out any, preparedMP4BaseURL string) {
	token := embyRequestToken(c)
	if token == "" || out == nil {
		return
	}
	embyAttachTokenToMediaSourcesValue(out, token, c, preparedMP4BaseURL)
}

func embyAttachTokenToMediaSourcesValue(value any, token string, c *gin.Context, preparedMP4BaseURL string) {
	switch typed := value.(type) {
	case map[string]any:
		embyAttachTokenToMediaSourcesMap(typed, token, c, preparedMP4BaseURL)
	case gin.H:
		embyAttachTokenToMediaSourcesMap(map[string]any(typed), token, c, preparedMP4BaseURL)
	case []map[string]any:
		for _, item := range typed {
			embyAttachTokenToMediaSourcesMap(item, token, c, preparedMP4BaseURL)
		}
	case []any:
		for _, item := range typed {
			embyAttachTokenToMediaSourcesValue(item, token, c, preparedMP4BaseURL)
		}
	}
}

func embyAttachTokenToMediaSourcesMap(out map[string]any, token string, c *gin.Context, preparedMP4BaseURL string) {
	if out == nil {
		return
	}
	if sources, ok := out["MediaSources"].([]map[string]any); ok {
		embyAttachTokenToMediaSources(sources, token, c, preparedMP4BaseURL)
	} else if sources, ok := out["MediaSources"].([]any); ok {
		for _, source := range sources {
			if sourceMap, ok := source.(map[string]any); ok {
				embyAttachTokenToMediaSources([]map[string]any{sourceMap}, token, c, preparedMP4BaseURL)
			}
		}
	}
	if items, ok := out["Items"]; ok {
		embyAttachTokenToMediaSourcesValue(items, token, c, preparedMP4BaseURL)
	}
}

func embyAttachTokenToMediaSources(sources []map[string]any, token string, c *gin.Context, preparedMP4BaseURL string) {
	for _, source := range sources {
		if raw, ok := source["Path"].(string); ok && source["IsRemote"] == true && source["Protocol"] == "Http" && strings.HasPrefix(raw, "/Videos/") {
			source["Path"] = absoluteRequestURL(c, embyAppendAPIKey(raw, token))
		}
		for _, key := range []string{"DirectStreamUrl", "TranscodingUrl"} {
			raw, ok := source[key].(string)
			if !ok {
				continue
			}
			raw = embyAppendAPIKey(raw, token)
			if key == "DirectStreamUrl" && preparedMP4BaseURL != "" && source["IsRemote"] == true && source["Protocol"] == "Http" {
				// Only the local prepared selector is eligible; never send this token to a remote source.
				id := c.Param("id")
				u, err := url.Parse(raw)
				if err == nil && !u.IsAbs() && u.Host == "" && u.Path == "/Videos/"+id+"/stream.mp4" && u.Query().Get("MediaSourceId") == id+":mp4" {
					u.RawQuery += externalProfileQuery(c)
					u.Path = "/prepared-mp4/" + id + "/stream.mp4"
					raw = preparedMP4BaseURL + u.String()
					source["Path"] = raw
				}
			}
			source[key] = raw
		}
		// Subtitle streams advertise a DeliveryUrl; the official Emby client
		// fetches it directly, so it must carry the auth token too.
		if streams, ok := source["MediaStreams"].([]map[string]any); ok {
			for _, stream := range streams {
				if stream["Type"] != "Subtitle" {
					continue
				}
				raw, ok := stream["DeliveryUrl"].(string)
				if !ok {
					continue
				}
				stream["DeliveryUrl"] = embyAppendAPIKey(raw, token)
			}
		}
	}
}

func embyRequestToken(c *gin.Context) string {
	if c == nil {
		return ""
	}
	for _, key := range []string{"api_key", "apiKey", "ApiKey", "token", "X-Emby-Token", "X-MediaBrowser-Token"} {
		if value := strings.TrimSpace(c.Query(key)); value != "" {
			return value
		}
	}
	for _, header := range []string{"X-Emby-Token", "X-MediaBrowser-Token"} {
		if value := strings.TrimSpace(c.GetHeader(header)); value != "" {
			return value
		}
	}
	for _, header := range []string{"Authorization", "X-Emby-Authorization", "X-MediaBrowser-Authorization"} {
		if token := embyTokenFromAuthHeader(c.GetHeader(header)); token != "" {
			return token
		}
	}
	return ""
}

func embyTokenFromAuthHeader(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	// 优先提取 Token="..." 引号内的纯 token。RodelPlayer 等客户端会把
	// UserId 和 Token 一起放进同一个 Emby/MediaBrowser 头里，例如
	// `Emby UserId="..", Client="..", Token="<jwt>"`。此时必须取 Token 引号内的
	// 纯 JWT，不能取整个头，否则 JWT 解析会因多余杂质失败。
	if strings.Contains(value, "Token=") {
		return embyTokenFromAuthHeaderTokenPart(value)
	}
	for _, prefix := range []string{"Bearer ", "Emby "} {
		if strings.HasPrefix(value, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(value, prefix))
		}
	}
	// 其它格式（例如只带 Client/Device 信息的 MediaBrowser 头）不是令牌，
	// 不能整串返回，否则会被当作 JWT 解析导致 "Invalid token"。
	if strings.HasPrefix(value, "MediaBrowser ") || strings.HasPrefix(value, "Emby ") {
		return ""
	}
	return value
}

// embyTokenFromAuthHeaderTokenPart 从 "Token=..." 形如的字段中取出引号内的纯 token。
func embyTokenFromAuthHeaderTokenPart(value string) string {
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "MediaBrowser "))
		if !strings.HasPrefix(part, "Token=") {
			continue
		}
		token := strings.TrimSpace(strings.TrimPrefix(part, "Token="))
		return strings.Trim(token, `"`)
	}
	return ""
}

func embyAppendAPIKey(raw, token string) string {
	raw = strings.TrimSpace(raw)
	token = strings.TrimSpace(token)
	if raw == "" || token == "" {
		return raw
	}
	if strings.HasPrefix(raw, "//") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() {
		return raw
	}
	q := u.Query()
	if q.Get("api_key") == "" && q.Get("apiKey") == "" && q.Get("token") == "" {
		q.Set("api_key", token)
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// embyVideoStreamHandler 是 GET /Videos/{id}/stream 的入口。
// 远程 Emby 条目（embyremote~ 前缀）走反向代理；本地条目直接代理到
// /api/stream/{id}（同一个 ServeFile）。
func embyVideoStreamHandler(svc *service.Container, cloudMode string) gin.HandlerFunc {
	return func(c *gin.Context) {
		encodedID := c.Param("id")
		if accountID, remoteID, ok := service.DecodeEmbyRemoteID(encodedID); ok {
			if err := svc.Emby.ProxyRemoteVideoStream(c.Request.Context(), c.Writer, c.Request, accountID, remoteID); err != nil {
				if errors.Is(err, service.ErrEmbyRemoteNotFound) {
					c.Status(http.StatusNotFound)
					return
				}
				if !c.Writer.Written() {
					c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
				}
			}
			return
		}
		uid := embyUserID(c)
		item, err := svc.Emby.Item(c.Request.Context(), encodedID, uid)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if item == nil {
			c.Status(http.StatusNotFound)
			return
		}
		if sourceID := firstQueryValue(c, "MediaSourceId", "mediaSourceId", "mediasourceid"); strings.Contains(sourceID, ":") && sourceID != encodedID+":mp4" && sourceID != encodedID+":hls" {
			c.Status(http.StatusNotFound)
			return
		}
		if sourceID := firstQueryValue(c, "MediaSourceId", "mediaSourceId", "mediasourceid"); sourceID == encodedID+":hls" {
			if !enforceScopedPlaybackToken(c, encodedID) {
				return
			}
			query := c.Request.URL.Query()
			query.Set("quality", "prepared")
			if token := embyRequestToken(c); token != "" {
				query.Set("api_key", token)
			}
			setRedirectNoStoreHeaders(c)
			c.Redirect(http.StatusFound, absoluteRequestURL(c, "/Videos/"+url.PathEscape(encodedID)+"/master.m3u8?"+query.Encode()))
			return
		}
		if source := firstQueryValue(c, "MediaSourceId", "mediaSourceId", "mediasourceid"); source == encodedID+":mp4" {
			if !enforceScopedPlaybackToken(c, encodedID) {
				return
			}
			media, err := svc.Repo.Media.FindByID(c.Request.Context(), encodedID)
			if err != nil || media == nil || !mediaVisibleForRequest(c, svc, media) {
				c.Status(http.StatusNotFound)
				return
			}
			if svc.Stream == nil || svc.Stream.ServePreparedMP4(c.Writer, c.Request, encodedID) != nil {
				c.Status(http.StatusNotFound)
			}
			return
		}
		if embyShouldRedirectVideoStreamToSTRM(c, svc, c.Param("id"), cloudMode) {
			target := "/api/stream/" + url.PathEscape(strings.TrimSpace(c.Param("id")))
			if token := embyPlaybackRedirectToken(c, svc); token != "" {
				target = embyAppendAPIKey(target, token)
			}
			setRedirectNoStoreHeaders(c)
			c.Redirect(http.StatusFound, absoluteRequestURL(c, target))
			return
		}
		// 直接调用 Stream service 写入 response。
		// 此前这里把所有错误一律吞成 404：云盘 Cookie 过期、直链解析失败、
		// STRM 播放被关闭……在第三方播放器上全部表现为「404 不存在」，
		// 无法排查。现在区分：行不存在→404；云盘播放不可用/上游故障→502+原因。
		err = svc.Stream.ServeFileWithCloudMode(c.Writer, c.Request, c.Param("id"), cloudMode)
		switch {
		case err == nil:
		case errors.Is(err, service.ErrMediaNotFound):
			c.Status(http.StatusNotFound)
		case errors.Is(err, service.ErrCloudPlaybackDisabled):
			if !c.Writer.Written() {
				c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			}
		default:
			if !c.Writer.Written() {
				c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			}
		}
	}
}

func embyPlaybackRedirectToken(c *gin.Context, svc *service.Container) string {
	if token := embyRequestToken(c); token != "" {
		return token
	}
	if c == nil || svc == nil || svc.Auth == nil || svc.Repo == nil || svc.Repo.User == nil {
		return ""
	}
	uid := embyUserID(c)
	if uid == "" {
		return ""
	}
	u, err := svc.Repo.User.FindByID(c.Request.Context(), uid)
	if err != nil || u == nil {
		return ""
	}
	token, err := svc.Auth.IssueEmbyToken(u)
	if err != nil {
		return ""
	}
	return token
}

func embyShouldRedirectVideoStreamToSTRM(c *gin.Context, svc *service.Container, mediaID, cloudMode string) bool {
	if c == nil || svc == nil || svc.Repo == nil || svc.Repo.Media == nil || cloudMode != service.CloudPlaybackModeRedirectProxy {
		return false
	}
	settings := service.CloudPlaybackSettings(c.Request.Context(), svc.Repo)
	if settings.PreferredMode != service.CloudPlaybackModeSTRM || !settings.STRMEnabled {
		return false
	}
	m, err := svc.Repo.Media.FindByID(c.Request.Context(), mediaID)
	if err != nil || m == nil {
		return false
	}
	return strings.TrimSpace(m.STRMURL) != ""
}

func embyVideoHLSPlaylistHandler(svc *service.Container) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 远程 Emby 条目不做本地转码（播放地址已由 PlaybackInfo 指向远程/代理直连）。
		if service.IsEmbyRemoteID(c.Param("id")) {
			c.Status(http.StatusNotFound)
			return
		}
		// 只需确认媒体行存在且对当前用户可见；Emby.Item 会构建完整条目载荷，
		// 转码播放下每个分片请求都跑一遍太浪费。
		m, err := svc.Repo.Media.FindByID(c.Request.Context(), c.Param("id"))
		if err != nil || m == nil || !mediaVisibleForRequest(c, svc, m) || svc.Stream == nil {
			c.Status(http.StatusNotFound)
			return
		}
		if !enforceScopedPlaybackToken(c, m.ID) {
			return
		}
		err = svc.Stream.ServeHLSPlaylist(c.Writer, c.Request, c.Param("id"))
		if errors.Is(err, service.ErrTranscodeDisabled) {
			c.JSON(http.StatusConflict, gin.H{"error": "transcode disabled"})
			return
		}
		if errors.Is(err, service.ErrTranscodeBusy) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "transcode busy"})
			return
		}
		if err != nil {
			c.Status(http.StatusNotFound)
		}
	}
}

func embyVideoHLSSegmentHandler(svc *service.Container) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service.IsEmbyRemoteID(c.Param("id")) {
			c.Status(http.StatusNotFound)
			return
		}
		m, err := svc.Repo.Media.FindByID(c.Request.Context(), c.Param("id"))
		if err != nil || m == nil || !mediaVisibleForRequest(c, svc, m) || svc.Stream == nil {
			c.Status(http.StatusNotFound)
			return
		}
		if !enforceScopedPlaybackToken(c, m.ID) {
			return
		}
		if err := svc.Stream.ServeHLSSegment(c.Writer, c.Request, c.Param("id"), c.Param("seg")); err != nil {
			c.Status(http.StatusNotFound)
		}
	}
}
