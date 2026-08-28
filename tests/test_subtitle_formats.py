import unittest

from srt_translate import Segment, Throttle, make_echo, rebuild_cues, segment_cue, translate_segments
from subtitle_formats import SubtitleFormatError, parse_subtitle, translated_filename


def translate_echo(document, language="zh-TW"):
    segments = []
    for cue_i, cue in enumerate(document.cues):
        segments.extend(segment_cue(cue, cue_i))
    output = translate_segments(
        segments, make_echo(), language, "English", 20, 1, 1,
        Throttle(), {}, 1, True,
    )
    cues = rebuild_cues(document.cues, segments, output, language, 40, 2)
    return document.clone_with_cues(cues).render()


class SubtitleFormatTests(unittest.TestCase):
    def test_translation_reports_each_completed_batch(self):
        segments = [Segment(index, f"Line {index}", [], False) for index in range(4)]
        updates = []

        output = translate_segments(
            segments, make_echo(), "zh-TW", "English", 1, 1, 1,
            Throttle(), {}, 2, True, lambda done, total: updates.append((done, total)),
        )

        self.assertEqual(updates, [(0, 4), (1, 4), (2, 4), (3, 4), (4, 4)])
        self.assertEqual(output, [f"[zh-TW] Line {index}" for index in range(4)])

    def test_srt_round_trip_and_translation(self):
        source = b"1\r\n00:00:01,000 --> 00:00:03,000\r\n<i>Hello</i> world\r\n\r\n"
        doc = parse_subtitle(source, ".srt")
        result = translate_echo(doc)
        self.assertIn("\r\n", result)
        self.assertIn("<i>", result)
        self.assertIn("</i>", result)
        self.assertIn("00:00:01,000 --> 00:00:03,000", result)

    def test_srt_preserves_utf8_bom_and_newline_style(self):
        source = b"\xef\xbb\xbf1\r\n00:00:01,000 --> 00:00:03,000\r\nHello\r\n\r\n"

        document = parse_subtitle(source, ".SRT")
        rendered = document.to_bytes()

        self.assertTrue(rendered.startswith(b"\xef\xbb\xbf"))
        self.assertIn(b"\r\n", rendered)
        self.assertNotIn(b"\n", rendered.replace(b"\r\n", b""))

    def test_webvtt_preserves_header_identifier_and_settings(self):
        source = (
            "WEBVTT - captions\n\nNOTE generated here\n\nintro\n"
            "00:01.000 --> 00:03.000 align:start position:10%\nHello\n\n"
        ).encode()
        doc = parse_subtitle(source, ".vtt")
        result = translate_echo(doc)
        self.assertTrue(result.startswith("WEBVTT - captions"))
        self.assertIn("NOTE generated here", result)
        self.assertIn("intro\n00:01.000 --> 00:03.000 align:start position:10%", result)
        self.assertIn("[zh-TW] Hello", result)

    def test_webvtt_requires_a_header_and_at_least_one_cue(self):
        with self.assertRaisesRegex(SubtitleFormatError, "begin with WEBVTT"):
            parse_subtitle(b"00:01.000 --> 00:03.000\nHello\n", ".vtt")
        with self.assertRaisesRegex(SubtitleFormatError, "No subtitle cues"):
            parse_subtitle(b"WEBVTT\n\nNOTE metadata only\n", ".vtt")

    def test_ass_preserves_script_and_non_text_dialogue_fields(self):
        source = (
            "[Script Info]\nTitle: Demo\n\n[Events]\n"
            "Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n"
            "Dialogue: 0,0:00:01.00,0:00:03.00,Default,Jane,0,0,0,,{\\an8}Hello\\Nworld\n"
        ).encode()
        doc = parse_subtitle(source, ".ass")
        result = translate_echo(doc)
        self.assertIn("Title: Demo", result)
        self.assertIn("Dialogue: 0,0:00:01.00,0:00:03.00,Default,Jane,0,0,0,,", result)
        self.assertIn("{\\an8}", result)
        self.assertIn("[zh-TW]", result)

    def test_ssa_round_trip_preserves_format_bom_and_crlf(self):
        source = (
            "\ufeff[Script Info]\r\nTitle: SSA Demo\r\n\r\n[Events]\r\n"
            "Format: Marked, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\r\n"
            "Dialogue: Marked=0,0:00:01.00,0:00:03.00,Default,,0,0,0,,Hello\\Nworld\r\n"
        ).encode()

        document = parse_subtitle(source, ".ssa")
        rendered = document.to_bytes()

        self.assertEqual(document.format, "ssa")
        self.assertTrue(rendered.startswith(b"\xef\xbb\xbf"))
        self.assertIn(b"Hello\\Nworld\r\n", rendered)

    def test_ass_rejects_dialogue_without_format_and_malformed_dialogue(self):
        without_format = b"[Events]\nDialogue: 0,0:00:01.00,0:00:02.00,Hello\n"
        malformed = (
            b"[Events]\nFormat: Start, End, Text\n"
            b"Dialogue: 0:00:01.00,0:00:02.00\n"
        )

        with self.assertRaisesRegex(SubtitleFormatError, "no Format line"):
            parse_subtitle(without_format, ".ass")
        with self.assertRaisesRegex(SubtitleFormatError, "Malformed"):
            parse_subtitle(malformed, ".ass")

    def test_clone_with_cues_does_not_mutate_original_metadata(self):
        source = b"WEBVTT\n\n00:01.000 --> 00:03.000\nHello\n"
        document = parse_subtitle(source, ".vtt")

        clone = document.clone_with_cues(document.cues)
        clone.metadata["header"] = "WEBVTT changed"

        self.assertEqual(document.metadata["header"], "WEBVTT")

    def test_invalid_encoding_and_empty_subtitle_are_rejected(self):
        with self.assertRaisesRegex(SubtitleFormatError, "Could not decode"):
            parse_subtitle(b"\xff", ".srt", encoding="utf-8")
        with self.assertRaisesRegex(SubtitleFormatError, "No subtitle cues"):
            parse_subtitle(b"not subtitles", ".srt")

    def test_unsupported_extension_is_rejected(self):
        with self.assertRaises(SubtitleFormatError):
            parse_subtitle(b"hello", ".txt")

    def test_output_name_keeps_original_format(self):
        self.assertEqual(translated_filename("show.en.ass", ".zh.tw"), "show.zh.tw.ass")
        self.assertEqual(translated_filename("SHOW.ENGLISH.VTT", ".ja"), "SHOW.ja.vtt")
