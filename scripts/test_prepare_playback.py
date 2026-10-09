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

    def arguments(self, source, output, ffmpeg=None):
        return argparse.Namespace(source=str(source), output=str(output), segment_seconds=0.5,
                                  ffmpeg=ffmpeg or self.ffmpeg, ffprobe=self.ffprobe)

    def test_av1_flac_preserves_delayed_video_and_all_source_bytes(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            source = self.make_source(directory)
            original = source.read_bytes()
            output = directory / "prepared"
            result = prepare.prepare(self.arguments(source, output))
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
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            source = self.make_source(directory)
            output = directory / "prepared"
            command = [sys.executable, str(Path(prepare.__file__).resolve()),
                       "--source", str(source), "--output", str(output),
                       "--ffmpeg", self.ffmpeg, "--ffprobe", self.ffprobe]
            process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            try:
                deadline = time.monotonic() + 10
                while not list(directory.glob(".prepare-hls-*")):
                    self.assertIsNone(process.poll(), "preparation exited before source change")
                    self.assertLess(time.monotonic(), deadline, "preparation did not start")
                    time.sleep(0.005)
                info = source.stat()
                os.utime(source, ns=(info.st_atime_ns, info.st_mtime_ns + 1_000_000))
                stdout, stderr = process.communicate(timeout=30)
                self.assertEqual(process.returncode, 1)
                self.assertFalse(output.exists())
                self.assertEqual(list(directory.glob(".prepare-hls-*")), [])
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
