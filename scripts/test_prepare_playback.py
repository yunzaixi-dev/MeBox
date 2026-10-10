#!/usr/bin/env python3
"""Real filesystem / FFmpeg checks: python3 test_prepare_playback.py."""
import argparse
import array
import json
import math
import os
from pathlib import Path
import shutil
import struct
import subprocess
import sys
import tempfile
import time
import unittest

import prepare_playback as prepare


class ValidationTests(unittest.TestCase):
    def test_segment_bound_rejects_nonfinite_and_unreasonable_values(self):
        for value in ("nan", "inf", "-inf", "0", "0.49", "30.1", "bad"):
            with self.subTest(value=value), self.assertRaises(argparse.ArgumentTypeError):
                prepare.segment_seconds(value)
        self.assertEqual(prepare.segment_seconds("4"), 4)

    def test_resume_point_rejects_nonfinite_and_negative_values(self):
        for value in ("nan", "inf", "-inf", "-0.001", "bad"):
            with self.subTest(value=value), self.assertRaises(argparse.ArgumentTypeError):
                prepare.resume_seconds(value)
        self.assertEqual(prepare.resume_seconds("1855"), 1855)
        self.assertEqual(prepare.resume_seconds("0"), 0)

    def test_unknown_initial_audio_duration_uses_next_pts_but_requires_end(self):
        rows = [{"pts_time": "-0.021333", "duration_time": "N/A"},
                {"pts_time": "0", "duration_time": "0.021333"},
                {"pts_time": "0.021333", "duration_time": "0.021333"}]
        first, end, count = prepare.stream_summary(rows)
        self.assertAlmostEqual(first, -0.021333)
        self.assertAlmostEqual(end, 0.042666)
        self.assertEqual(count, 3)
        with self.assertRaises(prepare.PreparationError):
            prepare.stream_summary([*rows[:2], {"pts_time": "0.021333", "duration_time": "N/A"}])
        for following in ("-0.021333", "-0.03"):
            with self.subTest(following=following), self.assertRaises(prepare.PreparationError):
                prepare.stream_summary([rows[0], {"pts_time": following, "duration_time": "0.021333"}])

    def test_init_configuration_is_derived_not_guessed(self):
        def box(kind, payload):
            return struct.pack(">I4s", len(payload) + 8, kind) + payload
        configuration = box(b"av1C", bytes((0x81, 8, 0x4C, 0)))
        sample = box(b"av01", bytes(78) + configuration)
        data = box(b"stsd", bytes(4) + struct.pack(">I", 1) + sample)
        for kind in (b"stbl", b"minf", b"mdia", b"trak", b"moov"):
            data = box(kind, data)
        stream = {"codec_name": "av1", "pix_fmt": "yuv420p10le"}
        self.assertEqual(prepare.video_codecs(data, stream), "av01.0.08M.10")
        with self.assertRaises(prepare.PreparationError):
            prepare.video_codecs(data[:-1], stream)
        with self.assertRaises(prepare.PreparationError):
            prepare.video_codecs(data, {**stream, "pix_fmt": "yuv420p"})

    def test_unsafe_incomplete_and_symlink_assets_are_refused(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            (directory / "init.mp4").write_bytes(b"init")
            (directory / "seg_00000.m4s").write_bytes(b"segment")
            prefix = ("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:4\n"
                      "#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n"
                      "#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-MAP:URI=\"init.mp4\"\n"
                      "#EXTINF:4,\n")
            for uri in ("../secret", "https://example.invalid/seg.m4s", "seg_00001.m4s"):
                (directory / "index.m3u8").write_text(prefix + uri + "\n#EXT-X-ENDLIST\n")
                with self.assertRaises(prepare.PreparationError):
                    prepare.read_manifest(directory)
            (directory / "index.m3u8").write_text(prefix + "seg_00000.m4s\n")
            with self.assertRaises(prepare.PreparationError):
                prepare.read_manifest(directory)
            (directory / "index.m3u8").write_text(prefix + "seg_00000.m4s\n#EXT-X-ENDLIST\n")
            self.assertEqual(len(prepare.read_manifest(directory)[0]), 1)
            (directory / "seg_00000.m4s").unlink()
            (directory / "seg_00000.m4s").symlink_to(directory / "init.mp4")
            with self.assertRaises(prepare.PreparationError):
                prepare.read_manifest(directory)

    def test_publication_never_replaces_an_existing_empty_directory(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            work, target = root / "work", root / "target"
            work.mkdir()
            target.mkdir()
            (work / "source.json").write_text("complete")
            with self.assertRaises(prepare.PreparationError):
                prepare.publish(work, target)
            self.assertTrue(work.is_dir())
            self.assertEqual(list(target.iterdir()), [])
            target.rmdir()
            prepare.publish(work, target)
            self.assertFalse(work.exists())
            self.assertEqual((target / "source.json").read_text(), "complete")


class MediaTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.ffmpeg = os.environ.get("FFMPEG", "ffmpeg")
        cls.ffprobe = os.environ.get("FFPROBE", "ffprobe")
        if not shutil.which(cls.ffmpeg) or not shutil.which(cls.ffprobe):
            raise unittest.SkipTest("FFmpeg and ffprobe are required for real media checks")

    def make_source(self, directory, audio="flac", channels="stereo", delay="0.3"):
        source = directory / (audio + ".mkv")
        audio_source = ("aevalsrc=" + "|".join(f"0.1*sin(2*PI*{frequency}*t)" for frequency in (210, 310, 410, 70, 610, 710))
                        + f":s=48000:channel_layout={channels}") if channels in ("5.1", "5.1(side)") else f"anullsrc=r=48000:cl={channels}"
        command = [self.ffmpeg, "-v", "error", "-nostdin", "-itsoffset", delay,
                   "-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10:duration=1.2",
                   "-f", "lavfi", "-i", audio_source,
                   "-t", "1.5", "-fps_mode", "passthrough", "-c:v", "libaom-av1",
                   "-cpu-used", "8", "-threads", "2", "-crf", "40", "-g", "5",
                   "-c:a", audio, str(source)]
        result = subprocess.run(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.assertEqual(result.returncode, 0, "real AV1 fixture generation failed")
        return source

    def arguments(self, source, output, ffmpeg=None, format="hls", resume_seconds=None):
        return argparse.Namespace(source=str(source), output=str(output), segment_seconds=0.5, format=format,
                                  ffmpeg=ffmpeg or self.ffmpeg, ffprobe=self.ffprobe,
                                  mkvinfo=os.environ.get("MKVINFO", "mkvinfo"), resume_seconds=resume_seconds)

    def test_av1_flac_preserves_delayed_video_and_all_source_bytes(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            source = self.make_source(directory)
            original = source.read_bytes()
            output = directory / "prepared"
            result = json.loads(prepare.run([sys.executable, str(Path(prepare.__file__).resolve()),
                                            "--source", str(source), "--output", str(output),
                                            "--ffmpeg", self.ffmpeg, "--ffprobe", self.ffprobe,
                                            "--segment-seconds", "0.5"]))
            self.assertTrue(result["audio_transcoded"])
            self.assertEqual(result["video_codec"], "av1")
            self.assertTrue(result["codecs"].endswith(",mp4a.40.2"))
            self.assertEqual(source.read_bytes(), original)
            metadata = json.loads((output / "source.json").read_text())
            self.assertEqual(metadata["source_size"], source.stat().st_size)
            self.assertEqual(metadata["source_mtime_ns"], source.stat().st_mtime_ns)
            streams = prepare.probe(self.ffprobe, output / "index.m3u8")["streams"]
            self.assertEqual([s["codec_name"] for s in streams], ["av1", "aac"])
            video_start = next(prepare.packet_rows(self.ffprobe, output / "index.m3u8", "v:0"))["pts_time"]
            audio_start = next(prepare.packet_rows(self.ffprobe, output / "index.m3u8", "a:0"))["pts_time"]
            self.assertGreater(float(video_start) - float(audio_start), 0.25)
            self.assertGreater(sum(prepare.read_manifest(output)[1]), 1.45)
            self.assertEqual(prepare.prepare(self.arguments(source, output))["status"], "unchanged")
            self.assertEqual(list(directory.glob(".prepare-hls-*")), [])
            (output / "seg_00000.m4s").write_bytes(b"damaged")
            with self.assertRaises(prepare.PreparationError):
                prepare.prepare(self.arguments(source, output))

    def test_native_mp4_is_faststart_copy_only_and_decodes_losslessly(self):
        for codec, channels in (("flac", "stereo"), ("aac", "stereo"),
                                ("ac3", "5.1(side)"), ("eac3", "5.1(side)")):
            with self.subTest(codec=codec), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                source = self.make_source(directory, codec, channels)
                original = source.read_bytes()
                output = directory / "native"
                args = self.arguments(source, output, format="mp4")
                result = json.loads(prepare.run([sys.executable, str(Path(prepare.__file__).resolve()),
                                                "--format", "mp4", "--source", str(source), "--output", str(output),
                                                "--ffmpeg", self.ffmpeg, "--ffprobe", self.ffprobe]))
                self.assertFalse(result["audio_transcoded"])
                self.assertEqual(source.read_bytes(), original)
                file = output / "stream.mp4"
                self.assertEqual(sorted(p.name for p in output.iterdir()), ["source.json", "stream.mp4"])
                self.assertLessEqual((output / "source.json").stat().st_size, 4096)
                streams = prepare.probe(self.ffprobe, file)["streams"]
                self.assertEqual([s["codec_name"] for s in streams], ["av1", codec])
                data = file.read_bytes()
                boxes = [kind for kind, _, _ in prepare.box_children(data)]
                self.assertLess(boxes.index(b"moov"), boxes.index(b"mdat"))
                # Native copyts retains the original AV axis, including negative
                # encoder preroll, instead of adding an offset at source start.
                for stream in ("v:0", "a:0"):
                    old = list(prepare.packet_rows(self.ffprobe, source, stream))
                    new = list(prepare.packet_rows(self.ffprobe, file, stream))
                    self.assertEqual([r["data_hash"] for r in old], [r["data_hash"] for r in new])
                    for before, after in zip(old, new):
                        for field in ("pts_time", "dts_time"):
                            if before.get(field) not in (None, "N/A"):
                                self.assertAlmostEqual(float(after[field]), float(before[field]), delta=0.002)
                    old_first, old_end, _ = prepare.stream_summary(iter(old))
                    new_first, new_end, _ = prepare.stream_summary(iter(new))
                    self.assertAlmostEqual(new_first, old_first, delta=0.002)
                    self.assertAlmostEqual(new_end, old_end, delta=0.002)
                    # Compare all decoded packet samples, before container edit
                    # lists trim priming/tail padding. MKV millisecond timestamps
                    # round 1024/256 priming samples to 1008/240 in MP4; AAC's
                    # short final declared duration also becomes discard padding.
                    # Packet PTS/DTS/end sync is independently checked above.
                    decode_flags = ["-flags2", "+skip_manual"] if stream == "a:0" else []
                    hashes = [prepare.run([self.ffmpeg, "-v", "error", *decode_flags, "-i", str(path),
                                           "-map", "0:" + stream, "-fps_mode", "passthrough",
                                           "-f", "hash", "-hash", "sha256", "pipe:1"])
                              for path in (source, file)]
                    self.assertEqual(hashes[0], hashes[1])
                self.assertEqual(prepare.prepare(args)["status"], "unchanged")
                self.assertEqual(list(directory.glob(".prepare-mp4-*")), [])

    def test_native_mkv_preserves_default_ac3_samples_after_keyframe_resume(self):
        mkvinfo = os.environ.get("MKVINFO", "mkvinfo")
        if not shutil.which(mkvinfo):
            self.skipTest("mkvinfo is required for native MKV checks")
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            encoded = self.make_source(directory, "ac3", "5.1(side)")
            source = directory / "original-with-extra-audio.mkv"
            prepare.run([self.ffmpeg, "-v", "error", "-i", str(encoded), "-map", "0:v:0",
                         "-map", "0:a:0", "-map", "0:a:0", "-c", "copy", str(source)])
            original = source.read_bytes()
            output = directory / "native"
            result = json.loads(prepare.run([sys.executable, str(Path(prepare.__file__).resolve()),
                                            "--format", "mkv", "--resume-seconds", "0.95",
                                            "--source", str(source), "--output", str(output),
                                            "--ffmpeg", self.ffmpeg, "--ffprobe", self.ffprobe,
                                            "--mkvinfo", mkvinfo]))
            self.assertFalse(result["audio_transcoded"])
            self.assertEqual(result["audio_channels"], 6)
            file = output / "stream.mkv"
            self.assertEqual(sorted(p.name for p in output.iterdir()), ["source.json", "stream.mkv"])
            self.assertEqual([s["codec_name"] for s in prepare.probe(self.ffprobe, file)["streams"]], ["av1", "ac3"])
            # The default consumer must decode the same samples after a seek,
            # not just the same compressed packets. AC3's persistent dither
            # state makes different demuxer preroll observable well past startup.
            for start in (None, 0.95, 1.4):
                with self.subTest(start=start):
                    seek = ["-ss", str(start)] if start is not None else []
                    samples = [prepare.run([self.ffmpeg, "-v", "error", *seek, "-i", str(path),
                                            "-t", "2", "-map", "0:a:0", "-c:a", "pcm_f64le",
                                            "-f", "f64le", "pipe:1"]) for path in (source, file)]
                    self.assertGreater(len(samples[0]), 0)
                    self.assertEqual(samples[0], samples[1])
            args = self.arguments(source, output, format="mkv", resume_seconds=0.95)
            self.assertEqual(prepare.prepare(args)["status"], "unchanged")
            self.assertEqual(source.read_bytes(), original)
            self.assertEqual(list(directory.glob(".prepare-mkv-*")), [])
            # A valid copy-only MKV with tail cues cannot be admitted merely
            # because its source marker and compressed/decoded media still match.
            replacement = directory / "tail-cues.mkv"
            prepare.run([self.ffmpeg, "-v", "error", "-i", str(file), "-map", "0", "-c", "copy", str(replacement)])
            replacement.replace(file)
            with self.assertRaisesRegex(prepare.PreparationError, "cues"):
                prepare.prepare(args)
            self.assertEqual(source.read_bytes(), original)

    def test_native_h264_b_frames_preserve_canonical_decode_timeline_and_color(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            source = directory / "reordered.mkv"
            chapters = directory / "chapters.ffmeta"
            chapters.write_text(";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=1200\ntitle=Original chapter\n")
            prepare.run([self.ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10:duration=1.2",
                         "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo",
                         "-f", "ffmetadata", "-i", str(chapters), "-t", "1.2",
                         "-map", "0:v:0", "-map", "1:a:0", "-map_chapters", "2",
                         "-c:v", "libx264", "-bf", "2", "-g", "10", "-threads", "2",
                         "-vf", "setparams=range=limited:color_primaries=bt709:color_trc=bt709:colorspace=bt709",
                         "-c:a", "aac", str(source)])
            original = source.read_bytes()
            source_chapters = json.loads(prepare.run([self.ffprobe, "-v", "error", "-show_chapters", "-of", "json", str(source)]))
            self.assertEqual(len(source_chapters["chapters"]), 1)
            output = directory / "native"
            prepare.prepare(self.arguments(source, output, format="mp4"))
            rows = list(prepare.packet_rows(self.ffprobe, output / "stream.mp4", "v:0"))
            canonical = list(prepare.packet_rows(self.ffprobe, "pipe:0", "v:0", prepare.source_video_feed(self.ffmpeg, source)))
            self.assertEqual([r["data_hash"] for r in rows], [r["data_hash"] for r in canonical])
            self.assertTrue(any(r["pts_time"] != r["dts_time"] for r in rows))
            shift = float(rows[0]["pts_time"]) - float(canonical[0]["pts_time"])
            for normalized, new in zip(canonical, rows):
                for field in ("pts_time", "dts_time"):
                    self.assertAlmostEqual(float(new[field]), float(normalized[field]) + shift, delta=0.002)
            streams = prepare.probe(self.ffprobe, output / "stream.mp4")["streams"]
            self.assertEqual([s["codec_type"] for s in streams], ["video", "audio"])
            video = streams[0]
            self.assertEqual(video["color_transfer"], "bt709")
            self.assertEqual(video["color_space"], "bt709")
            self.assertEqual(source.read_bytes(), original)

    def test_native_pce_audio_is_copied_without_guessing_speakers(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            encoded = self.make_source(directory, "aac", "5.1(side)", delay="0")
            source = directory / "pce-no-default-duration.mkv"
            prepare.run([self.ffmpeg, "-v", "error", "-i", str(encoded), "-map", "0", "-c", "copy",
                         "-write_crc32", "0", str(source)])
            # Omit the optional Matroska AAC DefaultDuration, as in real PCE
            # sources. Equal-sized EBML Void preserves every media byte/offset;
            # CRCs were disabled above so the resulting container remains valid.
            hint = bytes.fromhex("23e3838401458555")  # 1024/48000 s in nanoseconds
            data = source.read_bytes()
            self.assertEqual(data.count(hint), 1)
            source.write_bytes(data.replace(hint, bytes.fromhex("ec86000000000000"), 1))
            rows = prepare.packet_rows(self.ffprobe, source, "a:0")
            try:
                first = next(rows)
                self.assertIn(first.get("duration_time"), (None, "N/A"))
                self.assertLess(float(first["pts_time"]), 0)
            finally:
                rows.close()
            with self.assertRaises(prepare.PreparationError):
                prepare.require_mse_aac(self.ffprobe, source)
            original = source.read_bytes()
            output = directory / "native"
            prepare.prepare(self.arguments(source, output, format="mp4"))
            file = output / "stream.mp4"
            old_audio = next(s for s in prepare.probe(self.ffprobe, source)["streams"] if s["codec_type"] == "audio")
            new_audio = next(s for s in prepare.probe(self.ffprobe, file)["streams"] if s["codec_type"] == "audio")
            self.assertEqual(old_audio["extradata_hash"], new_audio["extradata_hash"])
            self.assertEqual(old_audio["channels"], new_audio["channels"])
            self.assertEqual([r["data_hash"] for r in prepare.packet_rows(self.ffprobe, source, "a:0")],
                             [r["data_hash"] for r in prepare.packet_rows(self.ffprobe, file, "a:0")])
            self.assertEqual(self.channel_frequencies(file), self.channel_frequencies(source))
            self.assertEqual(source.read_bytes(), original)
            old_first, old_end, _ = prepare.stream_summary(prepare.packet_rows(self.ffprobe, source, "a:0"))
            new_first, new_end, _ = prepare.stream_summary(prepare.packet_rows(self.ffprobe, file, "a:0"))
            self.assertAlmostEqual(new_first, old_first, delta=0.002)
            self.assertAlmostEqual(new_end, old_end, delta=0.002)

    def test_native_existing_source_and_package_mutations_are_refused(self):
        for mutation in ("source", "payload", "timeline", "marker", "oversized_marker", "symlink", "directory_symlink", "foreign"):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                source = self.make_source(directory)
                output = directory / "native"
                args = self.arguments(source, output, format="mp4")
                prepare.prepare(args)
                file = output / "stream.mp4"
                if mutation == "source":
                    info = source.stat()
                    os.utime(source, ns=(info.st_atime_ns, info.st_mtime_ns + 1))
                elif mutation in ("payload", "timeline"):
                    replacement = directory / "changed.mp4"
                    command = [self.ffmpeg, "-v", "error", "-i", str(file), "-map", "0", "-c", "copy"]
                    if mutation == "payload":
                        command += ["-bsf:v", "noise=amount=1"]
                    else:
                        baseline = list(prepare.packet_rows(self.ffprobe, file, "a:0"))
                        command += ["-bsf:a", r"setts=pts=PTS+if(gte(PTS*TB\,0.5)\,0.007/TB\,0):dts=DTS+if(gte(DTS*TB\,0.5)\,0.007/TB\,0)"]
                    prepare.run([*command, "-strict", "experimental", "-movflags", "+faststart", str(replacement)])
                    file.write_bytes(replacement.read_bytes())
                    if mutation == "timeline":
                        shifted = list(prepare.packet_rows(self.ffprobe, file, "a:0"))
                        self.assertEqual([r["data_hash"] for r in baseline], [r["data_hash"] for r in shifted])
                        for field in ("pts_time", "dts_time"):
                            self.assertTrue(any(float(after[field]) - float(before[field]) > 0.006
                                                for before, after in zip(baseline, shifted)))
                elif mutation == "marker":
                    marker = output / "source.json"
                    data = json.loads(marker.read_text())
                    data["audio_transcoded"] = True
                    marker.write_text(json.dumps(data))
                elif mutation == "oversized_marker":
                    marker = output / "source.json"
                    marker.write_text(marker.read_text() + " " * 4096)
                elif mutation == "symlink":
                    elsewhere = directory / "elsewhere.mp4"
                    file.rename(elsewhere)
                    file.symlink_to(elsewhere)
                elif mutation == "directory_symlink":
                    elsewhere = directory / "elsewhere"
                    output.rename(elsewhere)
                    output.symlink_to(elsewhere, target_is_directory=True)
                else:
                    source = self.make_source(directory, "aac")
                    args.source = str(source)
                with self.assertRaises(prepare.PreparationError):
                    prepare.prepare(args)
                self.assertEqual(list(directory.glob(".prepare-mp4-*")), [])

    def test_native_non_faststart_file_and_unpreservable_timeline_are_refused(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            original = self.make_source(directory, "aac", delay="0")
            output = directory / "native"
            args = self.arguments(original, output, format="mp4")
            prepare.prepare(args)
            replacement = directory / "tail-moov.mp4"
            prepare.run([self.ffmpeg, "-v", "error", "-i", str(output / "stream.mp4"),
                         "-map", "0", "-c", "copy", str(replacement)])
            (output / "stream.mp4").write_bytes(replacement.read_bytes())
            with self.assertRaises(prepare.PreparationError):
                prepare.prepare(args)
            source = directory / "duplicate.mkv"
            timestamp = r"if(eq(N\,5)\,PTS+0.1/TB\,PTS)"
            prepare.run([self.ffmpeg, "-v", "error", "-i", str(original), "-map", "0", "-c", "copy",
                         "-bsf:v", f"setts=pts={timestamp}:dts={timestamp}", str(source)])
            refused = directory / "refused"
            with self.assertRaises(prepare.PreparationError):
                prepare.prepare(self.arguments(source, refused, format="mp4"))
            self.assertFalse(refused.exists())
            self.assertEqual(list(directory.glob(".prepare-mp4-*")), [])

    def test_aac_program_config_is_not_published_as_mse_compatible(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            source = self.make_source(directory, "aac", "5.1(side)", delay="0")
            original = source.read_bytes()
            output = directory / "prepared"
            with self.assertRaises(prepare.PreparationError):
                prepare.prepare(self.arguments(source, output))
            self.assertFalse(output.exists())
            self.assertEqual(source.read_bytes(), original)

    def test_aac_is_copied_and_multichannel_ac3_preserves_channel_identity(self):
        for codec, channels in (("aac", "stereo"), ("aac", "5.1"), ("ac3", "5.1(side)")):
            with self.subTest(codec=codec), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                source = self.make_source(directory, codec, channels, delay="0")
                output = directory / "prepared"
                result = prepare.prepare(self.arguments(source, output))
                self.assertEqual(result["audio_transcoded"], codec != "aac")
                source_audio = next(s for s in prepare.probe(self.ffprobe, source)["streams"]
                                    if s["codec_type"] == "audio")
                output_audio = next(s for s in prepare.probe(self.ffprobe, output / "index.m3u8")["streams"]
                                    if s["codec_type"] == "audio")
                self.assertEqual(output_audio["channels"], source_audio["channels"])
                if codec == "aac":
                    self.assertEqual(output_audio.get("channel_layout"), source_audio.get("channel_layout"))
                    self.assertEqual(output_audio["extradata_hash"], source_audio["extradata_hash"])
                    if channels == "5.1(side)":
                        self.assertEqual(self.channel_frequencies(output / "index.m3u8"), [210, 310, 410, 70, 610, 710])
                else:
                    self.assertEqual((source_audio["channel_layout"], output_audio["channel_layout"]), ("5.1(side)", "5.1"))
                    expected = [210, 310, 410, 70, 610, 710]
                    self.assertEqual(self.channel_frequencies(source), expected)
                    self.assertEqual(self.channel_frequencies(output / "index.m3u8"), expected)

    def test_copied_aac_preserves_timestamp_gap_across_video_fragment(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            original = self.make_source(directory, audio="aac", delay="0")
            source = directory / "gap.mkv"
            prepare.run([self.ffmpeg, "-v", "error", "-i", str(original), "-map", "0", "-c", "copy",
                         "-bsf:a", "setts=pts=PTS+if(gte(PTS*TB\\,0.5)\\,0.007/TB\\,0)", str(source)])
            output = directory / "prepared"
            prepare.prepare(self.arguments(source, output))
            old = list(prepare.packet_rows(self.ffprobe, source, "a:0"))
            new = list(prepare.packet_rows(self.ffprobe, output / "index.m3u8", "a:0"))
            self.assertEqual([row["data_hash"] for row in old], [row["data_hash"] for row in new])
            origin = float(prepare.probe(self.ffprobe, source)["format"]["start_time"])
            for before, after in zip(old, new):
                self.assertAlmostEqual(float(after["pts_time"]), float(before["pts_time"]) - origin, delta=0.002)

    def test_duplicate_video_timestamps_at_fragment_boundary_are_not_retimed(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            original = self.make_source(directory, audio="aac", delay="0")
            source = directory / "duplicate.mkv"
            timestamp = r"if(eq(N\,5)\,PTS+0.1/TB\,PTS)"
            prepare.run([self.ffmpeg, "-v", "error", "-i", str(original), "-map", "0", "-c", "copy",
                         "-bsf:v", f"setts=pts={timestamp}:dts={timestamp}", str(source)])
            rows = list(prepare.packet_rows(self.ffprobe, source, "v:0"))
            self.assertEqual(rows[5]["pts_time"], rows[6]["pts_time"])
            self.assertIn("K", rows[5]["flags"])
            output = directory / "prepared"
            with self.assertRaises(prepare.PreparationError):
                prepare.prepare(self.arguments(source, output))
            self.assertFalse(output.exists())

    def channel_frequencies(self, path):
        data = prepare.run([self.ffmpeg, "-v", "error", "-i", str(path), "-f", "f32le", "pipe:1"])
        pcm = array.array("f")
        pcm.frombytes(data)
        frequencies = [210, 310, 410, 70, 610, 710]
        found = []
        for channel in range(6):
            values = pcm[12000 * 6 + channel:24000 * 6:6]
            powers = []
            for frequency in frequencies:
                real = sum(v * math.cos(2 * math.pi * frequency * i / 48000) for i, v in enumerate(values))
                imaginary = sum(v * math.sin(2 * math.pi * frequency * i / 48000) for i, v in enumerate(values))
                powers.append(real * real + imaginary * imaginary)
            found.append(frequencies[max(range(6), key=powers.__getitem__)])
        return found

    def test_init_media_cannot_hide_an_empty_first_fragment(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            source = self.make_source(directory)
            output = directory / "prepared"
            args = self.arguments(source, output)
            prepare.prepare(args)
            fragment = output / "seg_00000.m4s"
            data = fragment.read_bytes()
            self.assertEqual(data[4:8], b"styp")
            prefix = struct.unpack_from(">I", data)[0]
            init = output / "init.mp4"
            init.write_bytes(init.read_bytes() + data[prefix:])
            fragment.write_bytes(data[:prefix])
            with self.assertRaises(prepare.PreparationError):
                prepare.prepare(args)

    def test_complete_package_with_non_keyframe_fragments_is_refused(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            source = self.make_source(directory, audio="aac", delay="0")
            output = directory / "unsafe"
            output.mkdir()
            prepare.run([self.ffmpeg, "-v", "error", "-copyts", "-start_at_zero", "-i", str(source),
                         "-map", "0:v:0", "-map", "0:a:0", "-c", "copy", "-f", "hls",
                         "-hls_time", "0.6", "-hls_flags", "split_by_time", "-hls_playlist_type", "vod",
                         "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4",
                         "-hls_segment_filename", str(output / "seg_%05d.m4s"), str(output / "index.m3u8")])
            manifest = output / "index.m3u8"
            manifest.write_text(manifest.read_text().replace("#EXTM3U", "#EXTM3U\n#EXT-X-INDEPENDENT-SEGMENTS"))
            prepare.read_manifest(output)
            with self.assertRaises(prepare.PreparationError):
                prepare.validate_package(output, source, prepare.probe(self.ffprobe, source),
                                         self.ffmpeg, self.ffprobe, False)

    def test_sigterm_during_preparation_never_publishes_partial_package(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            source = self.make_source(directory)
            original = source.read_bytes()
            output = directory / "prepared"
            command = [sys.executable, str(Path(prepare.__file__).resolve()),
                       "--source", str(source), "--output", str(output),
                       "--ffmpeg", self.ffmpeg, "--ffprobe", self.ffprobe,
                       "--segment-seconds", "0.5"]
            process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            try:
                deadline = time.monotonic() + 10
                while not list(directory.glob(".prepare-hls-*")):
                    self.assertIsNone(process.poll(), "preparation exited before interruption")
                    self.assertLess(time.monotonic(), deadline, "preparation did not start")
                    time.sleep(0.005)
                process.terminate()
                stdout, stderr = process.communicate(timeout=10)
                self.assertEqual(process.returncode, 130)
                self.assertFalse(output.exists())
                self.assertEqual(list(directory.glob(".prepare-hls-*")), [])
                self.assertEqual(source.read_bytes(), original)
                self.assertNotIn(str(source).encode(), stdout + stderr)
            finally:
                if process.poll() is None:
                    process.kill()
                process.communicate()

    def test_source_changed_during_processing_is_not_published(self):
        for format in ("hls", "mp4"):
            with self.subTest(format=format), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                source = self.make_source(directory)
                output = directory / "prepared"
                command = [sys.executable, str(Path(prepare.__file__).resolve()),
                           "--source", str(source), "--output", str(output), "--format", format,
                           "--ffmpeg", self.ffmpeg, "--ffprobe", self.ffprobe]
                process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                try:
                    deadline = time.monotonic() + 10
                    while not list(directory.glob(f".prepare-{format}-*")):
                        self.assertIsNone(process.poll(), "preparation exited before source change")
                        self.assertLess(time.monotonic(), deadline, "preparation did not start")
                        time.sleep(0.005)
                    info = source.stat()
                    os.utime(source, ns=(info.st_atime_ns, info.st_mtime_ns + 1_000_000))
                    stdout, stderr = process.communicate(timeout=30)
                    self.assertEqual(process.returncode, 1)
                    self.assertFalse(output.exists())
                    self.assertEqual(list(directory.glob(f".prepare-{format}-*")), [])
                    self.assertNotIn(str(source).encode(), stdout + stderr)
                finally:
                    if process.poll() is None:
                        process.kill()
                    process.communicate()

    def test_failed_tool_leaves_no_published_or_temporary_package(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            source = self.make_source(directory)
            before = source.read_bytes()
            output = directory / "prepared"
            with self.assertRaises(prepare.PreparationError):
                prepare.prepare(self.arguments(source, output, ffmpeg="/bin/false"))
            self.assertFalse(output.exists())
            self.assertEqual(source.read_bytes(), before)
            self.assertEqual(list(directory.glob(".prepare-hls-*")), [])
            unrelated = directory / "unrelated"
            unrelated.mkdir()
            (unrelated / "keep").write_bytes(b"user data")
            with self.assertRaises(prepare.PreparationError):
                prepare.prepare(self.arguments(source, unrelated))
            self.assertEqual((unrelated / "keep").read_bytes(), b"user data")


if __name__ == "__main__":
    unittest.main()
