import unittest

from srt_translate import Throttle, make_echo, rebuild_cues, segment_cue, translate_segments
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
    def test_srt_round_trip_and_translation(self):
        source = b"1\r\n00:00:01,000 --> 00:00:03,000\r\n<i>Hello</i> world\r\n\r\n"
        doc = parse_subtitle(source, ".srt")
        result = translate_echo(doc)
        self.assertIn("\r\n", result)
        self.assertIn("<i>", result)
        self.assertIn("</i>", result)
        self.assertIn("00:00:01,000 --> 00:00:03,000", result)

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

    def test_unsupported_extension_is_rejected(self):
        with self.assertRaises(SubtitleFormatError):
            parse_subtitle(b"hello", ".txt")

    def test_output_name_keeps_original_format(self):
        self.assertEqual(translated_filename("show.en.ass", ".zh.tw"), "show.zh.tw.ass")
