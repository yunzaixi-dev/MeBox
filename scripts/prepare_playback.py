#!/usr/bin/env python3
"""Prepare validated copy-video HLS or copy-only faststart MP4 without changing the source."""
import argparse
import bisect
import ctypes
import errno
import itertools
import json
import math
import os
from pathlib import Path
import re
import shutil
import signal
import struct
import subprocess
import sys
import tempfile
import threading


class PreparationError(Exception):
    pass


def run(command):
    try:
        result = subprocess.run(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                check=False)
    except OSError:
        raise PreparationError("media tool could not be started") from None
    if result.returncode:
        raise PreparationError("media tool rejected the input or output")
    return result.stdout


def probe(binary, source):
    try:
        return json.loads(run([binary, "-v", "error", "-protocol_whitelist", "file,pipe",
                               "-show_streams", "-show_format", "-show_data_hash", "sha256", "-of", "json", str(source)]))
    except (ValueError, UnicodeError):
        raise PreparationError("invalid media probe response") from None


def require_mse_aac(binary, source):
    data = run([binary, "-v", "error", "-protocol_whitelist", "file,pipe",
                "-select_streams", "a:0", "-show_entries", "stream=extradata",
                "-show_data", "-of", "json", str(source)])
    try:
        configuration = json.loads(data)["streams"][0]["extradata"]
    except (ValueError, KeyError, IndexError, TypeError):
        raise PreparationError("AAC configuration unavailable") from None
    first = re.search(r"^\s*00000000:\s*([0-9a-fA-F]{4})", configuration, re.MULTILINE)
    if not first:
        raise PreparationError("AAC configuration unavailable")
    header = int(first[1], 16)
    # MIME support for mp4a.40.2 does not imply MSE accepts an AAC PCE.
    # Preserve the original; never infer speakers or remix an unknown layout.
    if header >> 11 != 2 or (header >> 7) & 15 > 12 or not 1 <= (header >> 3) & 15 <= 7:
        raise PreparationError("AAC configuration is not supported by prepared MSE playback")


def fingerprint(source):
    info = source.stat()
    return (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns, info.st_ctime_ns)


def number(value):
    try:
        result = float(value)
    except (TypeError, ValueError):
        raise PreparationError("missing media timestamp") from None
    if not math.isfinite(result):
        raise PreparationError("non-finite media timestamp")
    return result


def packet_rows(binary, source, stream=None, feeder=None):
    """Stream packet records; never retain a movie-sized probe response in memory."""
    command = [binary, "-v", "error", "-protocol_whitelist", "file,pipe",
               "-show_packets", "-show_data_hash", "sha256",
               "-show_entries", "packet=pts_time,dts_time,duration_time,data_hash,flags,stream_index,pos:packet_side_data=",
               "-of", "compact=p=0:nk=0"]
    if stream:
        command += ["-select_streams", stream]
    command += ["-i", str(source)]
    with tempfile.TemporaryFile() as errors:
        try:
            process = subprocess.Popen(command, stdin=subprocess.PIPE if feeder else subprocess.DEVNULL,
                                       stdout=subprocess.PIPE, stderr=errors, text=False)
        except OSError:
            raise PreparationError("packet probe could not be started") from None
        worker = None
        feed_errors = []
        if feeder:
            def feed():
                try:
                    feeder(process.stdin)
                except (OSError, PreparationError) as exc:
                    feed_errors.append(exc)
                finally:
                    process.stdin.close()
            worker = threading.Thread(target=feed, daemon=True)
            worker.start()
        try:
            for raw in process.stdout:
                try:
                    line = raw.decode("ascii").strip()
                    if not line:
                        continue
                    fields = dict(part.split("=", 1) for part in line.split("|") if "=" in part)
                    if "stream_index" not in fields:
                        continue
                    if not re.fullmatch(r"SHA256:[0-9a-fA-F]{64}", fields.get("data_hash", "")):
                        raise PreparationError("packet payload hash is unavailable")
                    yield fields
                except UnicodeError:
                    raise PreparationError("invalid packet probe response") from None
            if process.wait():
                raise PreparationError("packet probe failed")
            if worker:
                worker.join()
                if feed_errors:
                    raise PreparationError("packet input failed")
        finally:
            process.stdout.close()
            if process.poll() is None:
                process.terminate()
            process.wait()
            if worker:
                worker.join()


def source_video_feed(ffmpeg, source):
    def feed(destination):
        with tempfile.TemporaryFile() as errors:
            try:
                process = subprocess.Popen(
                    [ffmpeg, "-v", "error", "-nostdin", "-protocol_whitelist", "file,pipe",
                     "-i", str(source), "-map", "0:v:0", "-c:v", "copy", "-an",
                     "-movflags", "frag_keyframe+empty_moov", "-f", "mp4", "pipe:1"],
                    stdout=destination, stderr=errors)
            except OSError:
                raise PreparationError("video demux tool could not be started") from None
            try:
                if process.wait():
                    raise PreparationError("source video normalization failed")
            finally:
                if process.poll() is None:
                    process.terminate()
                    process.wait()
    return feed


def box_children(data, start=0, end=None):
    end = len(data) if end is None else end
    while start < end:
        if end - start < 8:
            raise PreparationError("truncated initialization box")
        size, kind = struct.unpack_from(">I4s", data, start)
        header = 8
        if size == 1:
            if end - start < 16:
                raise PreparationError("truncated initialization box")
            size = struct.unpack_from(">Q", data, start + 8)[0]
            header = 16
        elif size == 0:
            size = end - start
        if size < header or start + size > end:
            raise PreparationError("invalid initialization box")
        yield kind, start + header, start + size
        start += size


def video_configuration(data):
    configurations = []
    def visit(start, end):
        for kind, body, stop in box_children(data, start, end):
            if kind in (b"moov", b"trak", b"mdia", b"minf", b"stbl"):
                visit(body, stop)
            elif kind == b"stsd":
                if stop - body < 8:
                    raise PreparationError("invalid sample description")
                entries = list(box_children(data, body + 8, stop))
                if struct.unpack_from(">I", data, body + 4)[0] != len(entries):
                    raise PreparationError("invalid sample description count")
                for entry, payload, entry_end in entries:
                    if entry not in (b"av01", b"avc1", b"hvc1", b"hev1"):
                        continue
                    if entry_end - payload < 78:
                        raise PreparationError("truncated video description")
                    for config, config_start, config_end in box_children(data, payload + 78, entry_end):
                        if config in (b"av1C", b"avcC", b"hvcC"):
                            configurations.append((entry.decode("ascii"), config,
                                                   data[config_start:config_end]))
    visit(0, len(data))
    if len(configurations) != 1:
        raise PreparationError("video codec configuration is unavailable or ambiguous")
    return configurations[0]


def video_codecs(data, stream):
    entry, kind, config = video_configuration(data)
    codec = stream.get("codec_name")
    pixels = stream.get("pix_fmt", "")
    if codec == "av1" and entry == "av01" and kind == b"av1C":
        if len(config) < 4 or config[0] != 0x81:
            raise PreparationError("unsupported AV1 configuration")
        profile, level = config[1] >> 5, config[1] & 31
        tier = "H" if config[2] & 128 else "M"
        depth = 12 if config[2] & 32 else 10 if config[2] & 64 else 8
        if profile != 0 or level > 23 or (tier == "H" and level < 8):
            raise PreparationError("unsupported AV1 profile or level")
        if pixels not in ("yuv420p", "yuv420p10le") or config[2] & 16 or config[2] & 12 != 12:
            raise PreparationError("unsupported AV1 chroma format")
        expected_depth = 10 if pixels == "yuv420p10le" else 8
        if depth != expected_depth:
            raise PreparationError("AV1 bit depth differs from its configuration")
        return f"av01.{profile}.{level:02d}{tier}.{depth:02d}"
    if codec == "h264" and entry == "avc1" and kind == b"avcC":
        if len(config) < 7 or config[0] != 1 or config[1] not in (66, 77, 88, 100):
            raise PreparationError("unsupported H264 configuration")
        if pixels != "yuv420p" or not config[3] or config[3] > 62:
            raise PreparationError("unsupported H264 depth, chroma format or level")
        return "avc1." + config[1:4].hex().upper()
    if codec == "hevc" and entry in ("hvc1", "hev1") and kind == b"hvcC":
        if len(config) < 23 or config[0] != 1:
            raise PreparationError("unsupported HEVC configuration")
        profile = config[1] & 31
        # Only Main / Main10: extended profiles need additional capability restrictions.
        if profile not in (1, 2) or config[1] >> 6 or not config[12]:
            raise PreparationError("unsupported HEVC profile or level")
        depth = 8 + (config[17] & 7)
        if (depth not in (8, 10) or depth == 10 and profile != 2 or
                config[16] & 3 != 1 or config[18] & 7 != config[17] & 7 or
                pixels != ("yuv420p10le" if depth == 10 else "yuv420p")):
            raise PreparationError("unsupported HEVC depth or chroma format")
        compatibility = int.from_bytes(config[2:6], "big")
        compatibility = int(f"{compatibility:032b}"[::-1], 2)
        constraints = config[6:12].rstrip(b"\x00")
        suffix = "".join(f".{byte:02X}" for byte in constraints)
        return f"{entry}.{profile}.{compatibility:X}.{'H' if config[1] & 32 else 'L'}{config[12]}{suffix}"
    raise PreparationError("unsupported video codec configuration")


def read_manifest(directory):
    init = directory / "init.mp4"
    manifest = directory / "index.m3u8"
    for file in (init, manifest):
        if file.is_symlink() or not file.is_file() or not file.stat().st_size:
            raise PreparationError("missing or unsafe package asset")
    if manifest.stat().st_size > 16 * 1024 * 1024 or init.stat().st_size > 16 * 1024 * 1024:
        raise PreparationError("oversized package metadata")
    try:
        lines = manifest.read_text(encoding="utf-8").splitlines()
    except UnicodeError:
        raise PreparationError("invalid playlist encoding") from None
    if not lines or lines[0] != "#EXTM3U" or lines.count("#EXT-X-ENDLIST") != 1:
        raise PreparationError("package is not a complete HLS VOD")
    if "#EXT-X-PLAYLIST-TYPE:VOD" not in lines or "#EXT-X-MEDIA-SEQUENCE:0" not in lines:
        raise PreparationError("package is not a full-timeline VOD")
    if lines.count('#EXT-X-MAP:URI="init.mp4"') != 1 or "#EXT-X-INDEPENDENT-SEGMENTS" not in lines:
        raise PreparationError("missing independent fragment initialization")
    segments, durations = [], []
    pending = None
    allowed_tags = ("#EXT-X-VERSION:", "#EXT-X-TARGETDURATION:", "#EXTINF:")
    fixed_tags = {"#EXTM3U", "#EXT-X-ENDLIST", "#EXT-X-PLAYLIST-TYPE:VOD",
                  "#EXT-X-MEDIA-SEQUENCE:0", '#EXT-X-MAP:URI="init.mp4"',
                  "#EXT-X-INDEPENDENT-SEGMENTS"}
    ended = False
    for line in lines:
        if not line:
            continue
        if ended:
            raise PreparationError("playlist data follows end marker")
        if line == "#EXT-X-ENDLIST":
            ended = True
        if line.startswith("#EXTINF:"):
            if pending is not None:
                raise PreparationError("playlist segment is missing")
            pending = number(line[8:].split(",", 1)[0])
            if pending <= 0:
                raise PreparationError("invalid segment duration")
        elif not line.startswith("#"):
            if pending is None or not re.fullmatch(r"seg_[0-9]{5,}\.m4s", line):
                raise PreparationError("unsafe playlist segment URI")
            expected = f"seg_{len(segments):05d}.m4s"
            file = directory / line
            if line != expected or file.is_symlink() or not file.is_file() or not file.stat().st_size:
                raise PreparationError("missing or unsafe segment")
            segments.append(file)
            durations.append(pending)
            pending = None
        elif line not in fixed_tags and not line.startswith(allowed_tags):
            raise PreparationError("unsupported playlist directive")
    if not segments or pending is not None:
        raise PreparationError("incomplete playlist")
    targets = [line.split(":", 1)[1] for line in lines if line.startswith("#EXT-X-TARGETDURATION:")]
    if len(targets) != 1 or not targets[0].isdigit() or int(targets[0]) < round(max(durations)):
        raise PreparationError("invalid playlist target duration")
    return segments, durations


def packet_durations(rows):
    """Resolve missing nonterminal durations from the next strictly greater PTS."""
    pending = None
    for row in rows:
        if pending is not None:
            duration = number(row.get("pts_time")) - number(pending.get("pts_time"))
            if duration <= 0:
                raise PreparationError("unknown duration without a following timestamp")
            yield pending, duration
            pending = None
        duration = row.get("duration_time")
        if duration in (None, "N/A"):
            # Demuxers can omit initial duration. Use the next boundary, never
            # invent an FPS, codec sample count or an unknown final duration.
            pending = row
        else:
            duration = number(duration)
            if duration <= 0:
                raise PreparationError("invalid packet duration")
            yield row, duration
    if pending is not None:
        raise PreparationError("final packet duration unavailable")


def stream_summary(rows):
    first, end, count = None, None, 0
    for row, duration in packet_durations(rows):
        pts = number(row.get("pts_time"))
        end = pts + duration if end is None else max(end, pts + duration)
        first = pts if first is None else min(first, pts)
        count += 1
    if not count:
        raise PreparationError("empty media stream")
    return first, end, count


def validate_video_properties(source_video, video):
    for field in ("codec_name", "profile", "width", "height", "pix_fmt", "level"):
        if source_video.get(field) != video.get(field):
            raise PreparationError("video properties changed")
    for field in ("color_range", "color_space", "color_transfer", "color_primaries"):
        expected = source_video.get(field)
        if expected not in (None, "unknown", "unspecified") and video.get(field) != expected:
            raise PreparationError("video color metadata changed")


def read_faststart(file):
    if file.is_symlink() or not file.is_file() or not file.stat().st_size:
        raise PreparationError("missing or unsafe MP4 asset")
    size = file.stat().st_size
    moov, media = None, False
    with file.open("rb") as stream:
        while stream.tell() < size:
            start = stream.tell()
            header = stream.read(8)
            if len(header) != 8:
                raise PreparationError("truncated MP4 box")
            length, kind = struct.unpack(">I4s", header)
            if length == 1:
                extension = stream.read(8)
                if len(extension) != 8:
                    raise PreparationError("truncated MP4 box")
                length = struct.unpack(">Q", extension)[0]
            elif length == 0:
                length = size - start
            if length < stream.tell() - start or start + length > size:
                raise PreparationError("invalid MP4 box")
            if kind == b"moof":
                raise PreparationError("MP4 must be non-fragmented")
            if kind == b"moov":
                if moov is not None or media or length > 16 * 1024 * 1024:
                    raise PreparationError("missing, oversized or non-faststart MP4 metadata")
                stream.seek(start)
                moov = stream.read(length)
            if kind == b"mdat":
                if moov is None:
                    raise PreparationError("MP4 metadata must precede media")
                media = True
            stream.seek(start + length)
    if moov is None or not media:
        raise PreparationError("incomplete MP4")
    return moov


def validate_mp4(directory, source, source_info, ffmpeg, ffprobe):
    file = directory / "stream.mp4"
    moov = read_faststart(file)
    info = probe(ffprobe, file)
    source_video = next(s for s in source_info["streams"] if s.get("codec_type") == "video")
    source_audio = next((s for s in source_info["streams"] if s.get("codec_type") == "audio"), None)
    videos = [s for s in info["streams"] if s.get("codec_type") == "video"]
    audios = [s for s in info["streams"] if s.get("codec_type") == "audio"]
    if len(videos) != 1 or len(audios) != bool(source_audio) or len(info["streams"]) != 1 + bool(source_audio):
        raise PreparationError("unexpected prepared stream selection")
    video = videos[0]
    validate_video_properties(source_video, video)
    video_configuration(moov)
    try:
        codecs = video_codecs(moov, video)
    except PreparationError:
        # Native clients negotiate the explicit codec/profile/depth fields;
        # never guess an RFC6381 string for profiles outside the HLS subset.
        codecs = ""
    origin = number(source_info.get("format", {}).get("start_time", 0))
    original = packet_rows(ffprobe, source, "v:0")
    canonical = packet_rows(ffprobe, "pipe:0", "v:0", source_video_feed(ffmpeg, source))
    prepared = packet_rows(ffprobe, file, "v:0")
    shift, canonical_shift, count = None, None, 0
    previous_dts = None
    try:
        for old, normalized, new in itertools.zip_longest(original, canonical, prepared):
            if old is None or normalized is None or new is None:
                raise PreparationError("video packet count changed")
            if old.get("dts_time") not in (None, "N/A"):
                dts = number(old["dts_time"])
                if previous_dts is not None and dts <= previous_dts:
                    raise PreparationError("source video decode timestamps are not strictly increasing")
                previous_dts = dts
            if old["data_hash"] != normalized["data_hash"] or normalized["data_hash"] != new["data_hash"]:
                raise PreparationError("compressed video payload changed")
            pts = number(new.get("pts_time"))
            if shift is None:
                shift = pts - (number(old.get("pts_time")) - origin)
                canonical_shift = pts - number(normalized.get("pts_time"))
                if abs(shift) > 0.25:
                    raise PreparationError("video timestamp origin changed")
            if abs(pts - (number(old.get("pts_time")) - origin + shift)) > 0.002:
                raise PreparationError("video packet timing changed")
            for field in ("pts_time", "dts_time"):
                if abs(number(new.get(field)) - (number(normalized.get(field)) + canonical_shift)) > 0.002:
                    raise PreparationError("canonical video packet timing changed")
            duration = number(new.get("duration_time"))
            if duration <= 0 or abs(duration - number(old.get("duration_time"))) > 0.002:
                raise PreparationError("copied video duration changed")
            count += 1
    finally:
        original.close()
        canonical.close()
        prepared.close()
    if not count:
        raise PreparationError("empty video stream")
    _, old_end, _ = stream_summary(packet_rows(ffprobe, source, "v:0"))
    _, new_end, _ = stream_summary(packet_rows(ffprobe, file, "v:0"))
    if abs(new_end - (old_end - origin + shift)) > 0.002:
        raise PreparationError("video end changed")
    if source_audio:
        audio = audios[0]
        for field in ("codec_name", "profile", "channels", "sample_rate", "extradata_hash"):
            if source_audio.get(field) != audio.get(field):
                raise PreparationError("copied audio properties or configuration changed")
        layout = source_audio.get("channel_layout")
        if layout not in (None, "unknown", "unspecified") and layout != audio.get("channel_layout"):
            raise PreparationError("audio channel layout changed")
        old_rows, new_rows = packet_rows(ffprobe, source, "a:0"), packet_rows(ffprobe, file, "a:0")
        try:
            for old_packet, new_packet in itertools.zip_longest(packet_durations(old_rows), packet_durations(new_rows)):
                if old_packet is None or new_packet is None:
                    raise PreparationError("copied audio packet count changed")
                old, old_duration = old_packet
                new, new_duration = new_packet
                if old["data_hash"] != new["data_hash"]:
                    raise PreparationError("copied audio payload changed")
                for field in ("pts_time", "dts_time"):
                    if abs(number(new.get(field)) - (number(old.get(field)) - origin + shift)) > 0.002:
                        raise PreparationError("copied audio packet timing changed")
                if abs(new_duration - old_duration) > 0.002:
                    raise PreparationError("copied audio duration changed")
        finally:
            old_rows.close()
            new_rows.close()
        old_first, old_end, _ = stream_summary(packet_rows(ffprobe, source, "a:0"))
        new_first, new_end, _ = stream_summary(packet_rows(ffprobe, file, "a:0"))
        if (abs(new_first - (old_first - origin + shift)) > 0.002 or
                abs(new_end - (old_end - origin + shift)) > 0.002):
            raise PreparationError("audio/video synchronization changed")
        if codecs and audio.get("codec_name") == "aac" and audio.get("profile") == "LC":
            codecs += ",mp4a.40.2"
        else:
            codecs = ""
    pixels = video.get("pix_fmt", "")
    depth = re.search(r"(?:p|gray)(10|12|14|16)(?:le|be)?$", pixels)
    bit_depth = int(depth[1]) if depth else int(video.get("bits_per_raw_sample", 0) or 0)
    if not bit_depth and pixels in ("yuv420p", "yuv422p", "yuv444p", "yuvj420p", "yuvj422p", "yuvj444p", "gbrp", "gray", "nv12", "nv21"):
        bit_depth = 8
    return {"video_codec": video["codec_name"], "audio_codec": audios[0]["codec_name"] if audios else "",
            "audio_channels": int(audios[0]["channels"]) if audios else 0,
            "audio_sample_rate": int(audios[0]["sample_rate"]) if audios else 0,
            "video_profile": video.get("profile", ""), "video_level": int(video.get("level", 0)),
            "video_bit_depth": bit_depth,
            "color_transfer": video.get("color_transfer", ""),
            "codecs": codecs, "audio_transcoded": False}, 1


def validate_package(directory, source, source_info, ffmpeg, ffprobe, transcoded, rewrite=False):
    segments, durations = read_manifest(directory)
    info = probe(ffprobe, directory / "index.m3u8")
    videos = [s for s in info["streams"] if s.get("codec_type") == "video"]
    audios = [s for s in info["streams"] if s.get("codec_type") == "audio"]
    source_video = next(s for s in source_info["streams"] if s.get("codec_type") == "video")
    source_audio = next((s for s in source_info["streams"] if s.get("codec_type") == "audio"), None)
    if len(videos) != 1 or len(audios) != bool(source_audio):
        raise PreparationError("unexpected prepared stream selection")
    video = videos[0]
    validate_video_properties(source_video, video)
    codecs = video_codecs((directory / "init.mp4").read_bytes(), video)
    origin = number(source_info.get("format", {}).get("start_time", 0))
    original = packet_rows(ffprobe, source, "v:0")
    canonical = packet_rows(ffprobe, "pipe:0", "v:0", source_video_feed(ffmpeg, source))
    def prepared_feed(destination):
        for asset in (directory / "init.mp4", *segments):
            with asset.open("rb") as file:
                shutil.copyfileobj(file, destination)
    # Read all fragments once. Packet positions locate each independent segment
    # in this virtual MP4, avoiding one decoder/process startup per fragment.
    init_size = (directory / "init.mp4").stat().st_size
    ends, offset = [], init_size
    for segment in segments:
        offset += segment.stat().st_size
        ends.append(offset)
    prepared = packet_rows(ffprobe, "pipe:0", "v:0", prepared_feed)
    starts, current_segment = [], -1
    shift, video_end, count = None, 0, 0
    try:
        for old, normalized, new in itertools.zip_longest(original, canonical, prepared):
            if old is None or normalized is None or new is None:
                raise PreparationError("video packet count changed")
            if normalized["data_hash"] != new["data_hash"]:
                raise PreparationError("compressed video payload changed")
            position = new.get("pos", "")
            if not position.isdigit():
                raise PreparationError("fragment packet position unavailable")
            position = int(position)
            if position < init_size:
                raise PreparationError("media payload in initialization file")
            index = bisect.bisect_right(ends, position)
            if index >= len(segments):
                raise PreparationError("fragment packet outside package")
            if index != current_segment:
                if index != current_segment + 1 or "K" not in new.get("flags", ""):
                    raise PreparationError("segment does not begin with an independent keyframe")
                starts.append(number(new.get("pts_time")))
                current_segment = index
            old_pts = number(old.get("pts_time")) - origin
            new_pts = number(new.get("pts_time"))
            if shift is None:
                shift = new_pts - old_pts
                if abs(shift) > 0.25:
                    raise PreparationError("video timestamp origin changed")
            for field in ("pts_time", "dts_time"):
                if abs(number(new.get(field)) - (number(old.get(field)) - origin + shift)) > 0.002:
                    raise PreparationError("video packet timing changed")
            # HLS demuxers can omit first-fragment AV1 durations. Every copied
            # PTS/DTS above is verified; retain the source duration when omitted.
            duration = new.get("duration_time")
            duration = old.get("duration_time") if duration in (None, "N/A") else duration
            duration = number(duration)
            if duration <= 0 or abs(duration - number(old.get("duration_time"))) > 0.002:
                raise PreparationError("copied video duration changed")
            video_end = max(video_end, new_pts + duration)
            count += 1
    finally:
        original.close()
        canonical.close()
        prepared.close()
    if not count:
        raise PreparationError("empty video stream")
    end = video_end
    if source_audio:
        audio = audios[0]
        if audio.get("codec_name") != "aac" or audio.get("profile") != "LC":
            raise PreparationError("unsupported prepared AAC profile")
        for field in ("channels", "sample_rate"):
            if source_audio.get(field) != audio.get(field):
                raise PreparationError("audio channels or sample rate changed")
        source_layout, output_layout = source_audio.get("channel_layout"), audio.get("channel_layout")
        # AAC back-surround labels differ from AC3 side-surround labels. The
        # explicit identity pan preserves signals, not identical speaker labels.
        if source_layout != output_layout and not (
                transcoded and (source_layout, output_layout) == ("5.1(side)", "5.1")):
            raise PreparationError("audio channel layout changed")
        old_first, old_end, _ = stream_summary(packet_rows(ffprobe, source, "a:0"))
        new_first, new_end, _ = stream_summary(packet_rows(ffprobe, directory / "index.m3u8", "a:0"))
        tolerance = max(0.025, 2048 / int(audio["sample_rate"])) if transcoded else 0.002
        if (abs(new_first - (old_first - origin + shift)) > tolerance or
                abs(new_end - (old_end - origin + shift)) > tolerance):
            raise PreparationError("audio/video synchronization changed")
        if not transcoded:
            configuration = source_audio.get("extradata_hash", "")
            if (not re.fullmatch(r"SHA256:[0-9a-fA-F]{64}", configuration) or
                    configuration != audio.get("extradata_hash")):
                raise PreparationError("copied AAC configuration changed or unavailable")
            old_rows = packet_rows(ffprobe, source, "a:0")
            new_rows = packet_rows(ffprobe, directory / "index.m3u8", "a:0")
            try:
                for old, new in itertools.zip_longest(old_rows, new_rows):
                    if old is None or new is None or old["data_hash"] != new["data_hash"]:
                        raise PreparationError("copied audio payload changed")
                    if abs(number(new["pts_time"]) - (number(old["pts_time"]) - origin + shift)) > 0.002:
                        raise PreparationError("copied audio packet timing changed")
            finally:
                old_rows.close()
                new_rows.close()
        end = max(end, new_end)
        codecs += ",mp4a.40.2"
    if len(starts) != len(segments):
        raise PreparationError("fragment missing independent video")
    # FFmpeg measures the first EXTINF from the first video packet, which can be
    # delayed relative to source time zero. Preserve that delay in the VOD timeline.
    boundaries = [0.0] + starts[1:] + [end]
    actual = [right - left for left, right in zip(boundaries, boundaries[1:])]
    if any(duration <= 0 or not math.isfinite(duration) for duration in actual):
        raise PreparationError("invalid full-timeline segment boundaries")
    if rewrite:
        lines = ["#EXTM3U", "#EXT-X-VERSION:7", f"#EXT-X-TARGETDURATION:{math.ceil(max(actual))}",
                 "#EXT-X-MEDIA-SEQUENCE:0", "#EXT-X-PLAYLIST-TYPE:VOD",
                 "#EXT-X-INDEPENDENT-SEGMENTS", '#EXT-X-MAP:URI="init.mp4"']
        for segment, duration in zip(segments, actual):
            lines.extend((f"#EXTINF:{duration:.6f},", segment.name))
        lines.append("#EXT-X-ENDLIST")
        (directory / "index.m3u8").write_text("\n".join(lines) + "\n", encoding="utf-8")
    elif any(abs(a - b) > 0.002 for a, b in zip(actual, durations)):
        raise PreparationError("playlist does not describe the full source timeline")
    return {"video_codec": source_video["codec_name"], "audio_codec": "aac" if source_audio else "",
            "codecs": codecs, "audio_transcoded": transcoded,
            "source_audio_layout": source_audio.get("channel_layout", "") if source_audio else "",
            "audio_layout": audios[0].get("channel_layout", "") if source_audio else ""}, len(segments)


def publish(directory, output):
    # rename(2) can replace an unrelated empty directory. Linux NOREPLACE is the
    # atomic guarantee we need, rather than a racy exists-check before rename.
    libc = ctypes.CDLL(None, use_errno=True)
    try:
        rename = libc.renameat2
    except AttributeError:
        raise PreparationError("atomic no-replace publication is unavailable") from None
    rename.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
    rename.restype = ctypes.c_int
    if rename(-100, os.fsencode(directory), -100, os.fsencode(output), 1):
        code = ctypes.get_errno()
        if code == errno.EEXIST:
            raise PreparationError("output already exists; nothing was replaced")
        raise PreparationError("atomic publication failed")


def prepare(args):
    raw_source, raw_output = Path(args.source), Path(args.output)
    native = getattr(args, "format", "hls") == "mp4"
    for path in (raw_source, raw_output):
        path = path.absolute()
        if any(part.is_symlink() for part in (path, *path.parents)):
            raise PreparationError("source and output paths must not contain symlinks")
    if not raw_source.is_file() or raw_source.is_symlink():
        raise PreparationError("source must be a local regular file, not a symlink")
    source = raw_source.resolve()
    if raw_output.name in ("", ".", ".."):
        raise PreparationError("output must name an isolated media directory")
    if raw_output.is_symlink():
        raise PreparationError("output must not be a symlink")
    if not raw_output.parent.is_dir():
        raise PreparationError("output parent directory must already exist")
    output = raw_output.parent.resolve() / raw_output.name
    if output == source or output in source.parents:
        raise PreparationError("output must be isolated from the source")
    before = fingerprint(source)
    info = probe(args.ffprobe, source)
    formats = set(info.get("format", {}).get("format_name", "").split(","))
    if not formats.intersection({"matroska", "webm", "mov", "mp4", "mpegts"}):
        raise PreparationError("unsupported or non-self-contained source container")
    video = next((s for s in info.get("streams", []) if s.get("codec_type") == "video"), None)
    audio = next((s for s in info.get("streams", []) if s.get("codec_type") == "audio"), None)
    if not video or video.get("codec_name") not in ("av1", "h264", "hevc") or video.get("disposition", {}).get("attached_pic"):
        raise PreparationError("source requires a supported primary video stream")
    transcoded = bool(not native and audio and not (audio.get("codec_name") == "aac" and audio.get("profile") == "LC"))
    if audio:
        admitted = {"aac", "flac", "ac3", "eac3"}
        if audio.get("codec_name") not in admitted:
            raise PreparationError("unsupported source audio codec")
        if not 1 <= audio.get("channels", 0) <= 8 or (transcoded and not audio.get("channel_layout")):
            raise PreparationError("unsupported or unknown source audio channel layout")
        sample_rate = int(audio.get("sample_rate", 0))
        if sample_rate <= 0 or (not native and sample_rate not in (8000, 11025, 12000, 16000, 22050, 24000, 32000, 44100, 48000, 64000, 88200, 96000)):
            raise PreparationError("unsupported audio sample rate; refusing resampling")
        if not native and not transcoded:
            require_mse_aac(args.ffprobe, source)
    base = {"version": 1, "source_size": before[2], "source_mtime_ns": before[3]}
    if output.exists():
        metadata = output / "source.json"
        if not output.is_dir() or metadata.is_symlink() or not metadata.is_file() or metadata.stat().st_size > (4096 if native else 65536):
            raise PreparationError("refusing unrelated existing output")
        try:
            recorded = json.loads(metadata.read_text(encoding="utf-8"))
        except (ValueError, UnicodeError):
            raise PreparationError("invalid existing package metadata") from None
        if not isinstance(recorded, dict) or any(recorded.get(k) != v for k, v in base.items()):
            raise PreparationError("existing package belongs to a different or changed source")
        if native:
            details, count = validate_mp4(output, source, info, args.ffmpeg, args.ffprobe)
        else:
            details, count = validate_package(output, source, info, args.ffmpeg, args.ffprobe, transcoded)
        expected = {**base, **details}
        if (recorded != expected or any(type(recorded.get(k)) is not type(v) for k, v in expected.items()) or
                fingerprint(source) != before):
            raise PreparationError("existing package or source changed")
        return {"status": "unchanged", **({} if native else {"segments": count}), **details}
    temporary = Path(tempfile.mkdtemp(prefix=".prepare-mp4-" if native else ".prepare-hls-", dir=output.parent))
    try:
        command = [args.ffmpeg, "-v", "error", "-nostdin", "-protocol_whitelist", "file,pipe", "-copyts"]
        if not native:
            command += ["-start_at_zero"]
        # Native MP4 edit lists can represent original negative audio preroll.
        # Shifting it to zero can clip the final copied video sample's duration.
        command += ["-i", str(source), "-map", "0:v:0", "-map", "0:a:0?", "-c:v", "copy"]
        if video["codec_name"] == "hevc":
            command += ["-tag:v", "hvc1"]
        if audio:
            command += ["-c:a", "aac" if transcoded else "copy"]
            if transcoded:
                command += ["-profile:a", "aac_low", "-b:a", "192k" if audio["channels"] <= 2 else "384k"]
                if audio.get("channel_layout") == "5.1(side)":
                    command += ["-af", "pan=5.1|FL=FL|FR=FR|FC=FC|LFE=LFE|BL=SL|BR=SR"]
        command += ["-avoid_negative_ts", "disabled"]
        if native:
            # Native FLAC-in-MP4 is supported by FFmpeg behind this muxer flag;
            # every stream remains copy-only, including AAC with a PCE.
            # Default chapter copying synthesizes a bin_data/text track even
            # with explicit AV maps. Chapters remain intact in the original.
            command += ["-map_chapters", "-1", "-strict", "experimental", "-max_interleave_delta", "0",
                        "-chunk_duration", "500000", "-movflags", "+faststart", "-f", "mp4", str(temporary / "stream.mp4")]
        else:
            # DASH sidx continuity rewrites AAC PTS at HLS boundaries. HLS uses
            # its playlist instead; omit sidx without changing the timing gate.
            command += ["-f", "hls", "-hls_time", str(args.segment_seconds),
                        "-hls_playlist_type", "vod", "-hls_segment_type", "fmp4",
                        "-hls_fmp4_init_filename", "init.mp4", "-hls_flags", "independent_segments",
                        "-hls_segment_options", "movflags=+skip_sidx",
                        "-hls_segment_filename", str(temporary / "seg_%05d.m4s"),
                        str(temporary / "index.m3u8")]
        run(command)
        if native:
            details, count = validate_mp4(temporary, source, info, args.ffmpeg, args.ffprobe)
        else:
            details, count = validate_package(temporary, source, info, args.ffmpeg, args.ffprobe, transcoded, rewrite=True)
            read_manifest(temporary)
        if fingerprint(source) != before:
            raise PreparationError("source changed during preparation")
        # Commit marker is last: every package asset and the source fingerprint
        # have already passed validation before this metadata becomes visible.
        marker = json.dumps({**base, **details}, sort_keys=True) + "\n"
        if native and len(marker.encode("utf-8")) > 4096:
            raise PreparationError("oversized package metadata")
        (temporary / "source.json").write_text(marker, encoding="utf-8")
        for asset in temporary.iterdir():
            with asset.open("rb") as file:
                os.fsync(file.fileno())
        with_directory = os.open(temporary, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(with_directory)
        finally:
            os.close(with_directory)
        if fingerprint(source) != before:
            raise PreparationError("source changed before publication")
        publish(temporary, output)
        parent = os.open(output.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(parent)
        finally:
            os.close(parent)
        return {"status": "prepared", **({} if native else {"segments": count}), **details}
    finally:
        # Only our own mkdtemp directory is ever recursively removed.
        if temporary.exists():
            shutil.rmtree(temporary)


def segment_seconds(value):
    try:
        seconds = float(value)
    except ValueError:
        raise argparse.ArgumentTypeError("segment seconds must be a finite number") from None
    if not math.isfinite(seconds) or not 0.5 <= seconds <= 30:
        raise argparse.ArgumentTypeError("segment seconds must be between 0.5 and 30")
    return seconds


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--format", choices=("hls", "mp4"), default="hls")
    parser.add_argument("--segment-seconds", type=segment_seconds, default=2.0)
    parser.add_argument("--ffmpeg", default="ffmpeg")
    parser.add_argument("--ffprobe", default="ffprobe")
    args = parser.parse_args(argv)
    def interrupted(signum, frame):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupted)
    try:
        print(json.dumps(prepare(args), sort_keys=True))
        return 0
    except KeyboardInterrupt:
        print("preparation interrupted; temporary assets are never published", file=sys.stderr)
        return 130
    except PreparationError as exc:
        print(f"preparation failed: {exc}", file=sys.stderr)
        return 1
    except (OSError, KeyError, TypeError, ValueError, StopIteration):
        print("preparation failed: invalid media data or filesystem operation", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
