"""Regression tests for translation-engine and subtitle-format fixes."""

import datetime
import hashlib
import http.server
import threading
import time
import unittest
from unittest.mock import patch

from srt_translate import (
    Cue,
    FatalTranslationError,
    RateLimitError,
    Segment,
    Throttle,
    TranslationError,
    _parse_rate_limit_reset,
    _post_json,
    make_deepl,
    make_echo,
    mask_tags,
    needs_translation,
    parse_numbered,
    parse_srt,
    placeholders_match,
    rebuild_cues,
    segment_cue,
    translate_segments,
    wrap_latin,
)
from subtitle_formats import parse_subtitle

CUE = ("00:00:01,000", "00:00:02,000", "")


def segments_for(cues):
    segments = []
    for cue_i, cue in enumerate(cues):
        segments.extend(segment_cue(cue, cue_i))
    return segments


def run_translation(segments, provider, **overrides):
    options = {
        "tgt_key": "es", "src": "English", "batch_size": 20, "retries": 1,
        "rate_retries": 1, "throttle": Throttle(), "cache": {}, "workers": 1,
        "quiet": True,
    }
    options.update(overrides)
    return translate_segments(segments, provider, **options)


def translate_document(document, provider, language="es", width=40, max_lines=2):
    segments = segments_for(document.cues)
    output = run_translation(segments, provider, tgt_key=language)
    cues = rebuild_cues(document.cues, segments, output, language, width, max_lines)
    return document.clone_with_cues(cues).render()


def recording_provider(transform=lambda text: f"T:{text}"):
    calls = []

    def provider(texts, _source, _target):
        calls.append(list(texts))
        return [transform(text) for text in texts]

    return provider, calls


ASS_HEADER = (
    "[Script Info]\nTitle: Demo\n\n[Events]\n"
    "Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n"
)


def ass_line(text):
    return f"Dialogue: 0,0:00:01.00,0:00:03.00,Default,,0,0,0,,{text}\n"


class EmptySegmentTests(unittest.TestCase):
    def test_empty_segments_are_not_sent_and_pass_through(self):
        source = (ASS_HEADER + ass_line("") + ass_line("Hello") + ass_line("{\\an8}")).encode()
        document = parse_subtitle(source, ".ass")
        provider, calls = recording_provider()
        reported = []
        segments = segments_for(document.cues)

        output = run_translation(segments, provider, fallback_callback=reported.append)
        rendered = document.clone_with_cues(
            rebuild_cues(document.cues, segments, output, "es", 40, 2)
        ).render()

        self.assertEqual(calls, [["Hello"]])
        self.assertEqual(reported, [])
        self.assertIn(ass_line("").rstrip("\n"), rendered)
        self.assertIn(ass_line("T:Hello").rstrip("\n"), rendered)
        self.assertIn(ass_line("{\\an8}").rstrip("\n"), rendered)

    def test_srt_cue_without_text_is_not_sent(self):
        source = (b"1\n00:00:01,000 --> 00:00:02,000\n\n"
                  b"2\n00:00:03,000 --> 00:00:04,000\nHi\n")
        document = parse_subtitle(source, ".srt")
        provider, calls = recording_provider()

        rendered = translate_document(document, provider)

        self.assertEqual(calls, [["Hi"]])
        self.assertEqual(rendered, document.render().replace("Hi", "T:Hi"))


class _LoopbackHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length") or 0))
        route = self.path.strip("/")
        if route == "disconnect":
            self.close_connection = True
            return
        if route == "slow-headers":
            time.sleep(1.0)
            return
        if route == "slow-body":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", "100")
            self.end_headers()
            self.wfile.write(b'{"partial"')
            self.wfile.flush()
            time.sleep(1.0)
            return
        if route == "rate-limited":
            body = b'{"error": "slow down"}'
            self.send_response(429)
            self.send_header("x-ratelimit-reset-requests", "6m0s")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        body = {"html": b"<html>Bad gateway</html>", "list": b"[]",
                "ok": b'{"ok": true}'}[route]
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


class PostJsonErrorTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _LoopbackHandler)
        cls.server.daemon_threads = True
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()
        cls.base = f"http://127.0.0.1:{cls.server.server_address[1]}"

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def post(self, route):
        return _post_json(f"{self.base}/{route}", {"content-type": "application/json"},
                          {"q": "x"}, timeout=0.3)

    def assert_retryable(self, route):
        with self.assertRaises(TranslationError) as caught:
            self.post(route)
        self.assertNotIsInstance(caught.exception, (FatalTranslationError, RateLimitError))
        return caught.exception

    def test_success_still_returns_the_json_object(self):
        self.assertEqual(self.post("ok"), {"ok": True})

    def test_dropped_connection_is_retryable(self):
        self.assert_retryable("disconnect")

    def test_timeout_waiting_for_headers_is_retryable(self):
        self.assert_retryable("slow-headers")

    def test_timeout_while_reading_the_body_is_retryable(self):
        self.assert_retryable("slow-body")

    def test_non_json_and_non_object_responses_are_retryable(self):
        self.assertIn("not JSON", str(self.assert_retryable("html")))
        self.assert_retryable("list")

    def test_go_duration_reset_header_sets_retry_after(self):
        with self.assertRaises(RateLimitError) as caught:
            self.post("rate-limited")
        self.assertEqual(caught.exception.retry_after, 360.0)


class TagSafeWrappingTests(unittest.TestCase):
    def rebuild_single(self, line, language="es", width=5, translated=None, dialect=""):
        cue = Cue(1, *CUE, [line], dialect)
        segments = segment_cue(cue, 0)
        output = [translated or segment.text for segment in segments]
        return rebuild_cues([cue], segments, output, language, width, 4)[0].lines

    def assert_tags_intact(self, lines, *tags):
        for line in lines:
            self.assertEqual(line.count("<"), line.count(">"), lines)
            self.assertEqual(line.count("{"), line.count("}"), lines)
        joined = "".join(lines)
        for tag in tags:
            self.assertIn(tag, joined)

    def test_latin_wrap_never_splits_a_font_tag(self):
        lines = self.rebuild_single('<font color="#ff0000">Hello there my friend</font>')

        self.assertGreater(len(lines), 1)
        self.assert_tags_intact(lines, '<font color="#ff0000">', "</font>")
        self.assertTrue(lines[0].startswith('<font color="#ff0000">Hello'))

    def test_latin_wrap_never_splits_an_ass_override_with_spaces(self):
        lines = self.rebuild_single("{\\fnArial Black}Hello there my friend", dialect="ass")

        self.assert_tags_intact(lines, "{\\fnArial Black}")
        self.assertTrue(lines[0].startswith("{\\fnArial Black}Hello"))

    def test_cjk_wrap_never_splits_an_ass_override(self):
        lines = self.rebuild_single(
            "{\\pos(100,200)}Hello", language="zh-TW", width=3,
            translated="⟦0⟧你好世界你好世界", dialect="ass",
        )

        self.assertGreater(len(lines), 1)
        self.assert_tags_intact(lines, "{\\pos(100,200)}")
        self.assertTrue(lines[0].startswith("{\\pos(100,200)}你"))

    def test_tag_between_spaces_never_forms_its_own_line(self):
        lines = self.rebuild_single("aaaa <i> bbbb cccc</i>", width=3)

        for line in lines:
            self.assertNotIn(line, {"<i>", "</i>"})
        self.assertEqual(" ".join(lines), "aaaa <i> bbbb cccc</i>")


class SpeakerDashTests(unittest.TestCase):
    def test_dash_inside_leading_tags_is_detected_and_restored(self):
        cue = Cue(1, *CUE, ["<i>- Where?</i>", "<i>- Home.</i>"])

        segments = segment_cue(cue, 0)

        self.assertEqual([segment.text for segment in segments],
                         ["⟦0⟧Where?⟦1⟧", "⟦0⟧Home.⟦1⟧"])
        rebuilt = rebuild_cues([cue], segments, ["⟦0⟧¿Dónde?⟦1⟧", "⟦0⟧A casa.⟦1⟧"],
                               "es", 20, 2)
        self.assertEqual(rebuilt[0].lines, ["<i>- ¿Dónde?</i>", "<i>- A casa.</i>"])

    def test_dash_after_ass_override_is_detected(self):
        cue = Cue(1, *CUE, ["{\\an8}- Hi", "- Bye"], "ass")

        segments = segment_cue(cue, 0)

        self.assertEqual(len(segments), 2)
        rebuilt = rebuild_cues([cue], segments, ["⟦0⟧Hola", "Adiós"], "es", 20, 2)
        self.assertEqual(rebuilt[0].lines, ["{\\an8}- Hola", "- Adiós"])

    def test_undashed_continuation_joins_the_previous_speaker(self):
        cue = Cue(1, *CUE, ["- Where are you going", "right now?", "- Home."])

        segments = segment_cue(cue, 0)

        self.assertEqual([segment.text for segment in segments],
                         ["Where are you going right now?", "Home."])
        self.assertTrue(all(segment.dashed for segment in segments))
        rebuilt = rebuild_cues([cue], segments, ["¿Adónde vas ahora?", "A casa."],
                               "es", 20, 2)
        self.assertEqual(rebuilt[0].lines, ["- ¿Adónde vas ahora?", "- A casa."])

    def test_minus_sign_before_a_digit_is_not_a_dash(self):
        cue = Cue(1, *CUE, ["-10 degrees outside"])

        segments = segment_cue(cue, 0)

        self.assertFalse(segments[0].dashed)
        self.assertEqual(segments[0].text, "-10 degrees outside")
        rebuilt = rebuild_cues([cue], segments, [segments[0].text], "es", 40, 2)
        self.assertEqual(rebuilt[0].lines, ["-10 degrees outside"])
        speakers = segment_cue(Cue(1, *CUE, ["-10 degrees?", "- Yes."]), 0)
        self.assertEqual(len(speakers), 1)


class AssDrawingTests(unittest.TestCase):
    DRAWING = "{\\p1}m 0 0 l 100 0 100 100 0 100{\\p0}"

    def test_drawing_is_never_sent_or_wrapped(self):
        source = (ASS_HEADER + ass_line(self.DRAWING)
                  + ass_line(self.DRAWING + "Hello there my good friend")).encode()
        document = parse_subtitle(source, ".ass")
        provider, calls = recording_provider()

        rendered = translate_document(document, provider, width=5, max_lines=4)

        sent = [text for batch in calls for text in batch]
        self.assertEqual(len(sent), 1)
        self.assertNotIn("m 0 0", sent[0])
        self.assertIn(ass_line(self.DRAWING).rstrip("\n"), rendered)
        # The text after the drawing is wrapped; the drawing itself is not.
        self.assertIn("T:" + self.DRAWING + "Hello\\N", rendered)

    def test_unterminated_drawing_runs_to_the_end(self):
        masked, tags = mask_tags("{\\p2}m 0 0 l 1 1", ass=True)

        self.assertEqual(masked, "⟦0⟧")
        self.assertEqual(tags, ["{\\p2}m 0 0 l 1 1"])
        self.assertFalse(needs_translation(Segment(0, masked, tags, False)))


class SrtParsingTests(unittest.TestCase):
    def test_srt_without_blank_separator_starts_a_new_cue(self):
        cues = parse_srt("1\n00:00:01,000 --> 00:00:02,000\nHello\n"
                         "2\n00:00:03,000 --> 00:00:04,000\nWorld\n"
                         "00:00:05,000 --> 00:00:06,000\nAgain\n")

        self.assertEqual([cue.lines for cue in cues], [["Hello"], ["World"], ["Again"]])
        self.assertEqual([cue.index for cue in cues], [1, 2, 3])


class MaskingTests(unittest.TestCase):
    def test_vtt_karaoke_timestamps_are_masked(self):
        masked, tags = mask_tags("<c>Never <00:00:01.500>gonna</c>")

        self.assertEqual(masked, "⟦0⟧Never ⟦1⟧gonna⟦2⟧")
        self.assertEqual(tags[1], "<00:00:01.500>")

    def test_ass_hard_space_and_soft_break_are_masked_only_for_ass(self):
        self.assertEqual(mask_tags("a\\hb\\nc", ass=True), ("a⟦0⟧b⟦1⟧c", ["\\h", "\\n"]))
        self.assertEqual(mask_tags("C:\\new\\home"), ("C:\\new\\home", []))


class PlaceholderValidationTests(unittest.TestCase):
    def test_placeholder_multiset_must_match(self):
        source = "⟦0⟧Hello⟦1⟧ world"
        self.assertTrue(placeholders_match(source, "mundo ⟦0⟧Hola⟦ 1 ⟧"))
        self.assertFalse(placeholders_match(source, "⟦0⟧Hola mundo"))
        self.assertFalse(placeholders_match(source, "⟦0⟧Hola⟦1⟧⟦1⟧ mundo"))
        self.assertFalse(placeholders_match(source, "⟦0⟧Hola⟦1⟧ ⟦2⟧mundo"))
        self.assertFalse(placeholders_match(source, "⟦0⟧Hola⟦1⟧ ⟦mundo"))

    @patch("srt_translate.time.sleep")
    def test_dropped_placeholder_is_retried_then_passed_through(self, _sleep):
        calls = []

        def provider(texts, _source, _target):
            calls.append(list(texts))
            return [text.replace("⟦1⟧", "") for text in texts]

        segments = segments_for([Cue(1, *CUE, ["<i>Hello</i>"]), Cue(2, *CUE, ["Plain"])])
        reported = []
        cache = {}

        output = run_translation(segments, provider, retries=2, cache=cache,
                                 fallback_callback=reported.append)

        self.assertEqual(output, ["⟦0⟧Hello⟦1⟧", "Plain"])
        self.assertEqual(reported, [1])
        self.assertEqual(list(cache.values()), ["Plain"])
        self.assertIn([segments[0].text], calls)

    def test_invalid_cached_translation_is_not_reused(self):
        segment = segment_cue(Cue(1, *CUE, ["<i>Hello</i>"]), 0)[0]
        key = hashlib.sha256(f"es\0{segment.text}".encode()).hexdigest()[:24]
        provider, calls = recording_provider()

        output = run_translation([segment], provider, cache={key: "⟦0⟧Hola"})

        self.assertEqual(calls, [[segment.text]])
        self.assertEqual(output, ["T:⟦0⟧Hello⟦1⟧"])

    def test_wrong_translation_count_is_a_retryable_format_error(self):
        def provider(texts, _source, _target):
            return ["only one"] if len(texts) > 1 else [texts[0].upper()]

        segments = [Segment(0, "a", [], False), Segment(1, "b", [], False)]

        self.assertEqual(run_translation(segments, provider), ["A", "B"])

    def test_literal_sentinels_in_source_round_trip(self):
        line = "Press ⟦1⟧ then <i>go</i>"
        cue = Cue(1, *CUE, [line])
        segments = segment_cue(cue, 0)

        self.assertEqual(segments[0].text.count("⟦"), 4)
        output = run_translation(segments, make_echo())
        rebuilt = rebuild_cues([cue], segments, output, "es", 40, 2)

        self.assertEqual(rebuilt[0].lines, ["[es] " + line])

    def test_rebuild_tolerates_unvalidated_output(self):
        cue = Cue(1, *CUE, ["<i>Hello</i>"])
        segments = segment_cue(cue, 0)

        rebuilt = rebuild_cues([cue], segments, ["⟦0⟧Hola⟦0⟧ ⟦7⟧ ⟦"], "es", 40, 2)

        self.assertEqual(rebuilt[0].lines, ["<i>Hola</i>"])


class ParseNumberedTests(unittest.TestCase):
    def test_accepts_an_empty_translation(self):
        self.assertEqual(parse_numbered("1\tHola\n2\t\n3\tAdiós", 3), ["Hola", "", "Adiós"])
        self.assertEqual(parse_numbered("1\tHola\n2\n3\tAdiós", 3), ["Hola", "", "Adiós"])
        self.assertEqual(parse_numbered("1. Hola\n2.\n3. Adiós", 3), ["Hola", "", "Adiós"])

    def test_unnumbered_lines_continue_the_previous_entry(self):
        output = "1\tHe said that he would\ncome back tomorrow.\n2\tBye"

        self.assertEqual(parse_numbered(output, 2),
                         ["He said that he would come back tomorrow.", "Bye"])

    def test_cjk_continuation_is_joined_without_a_space(self):
        self.assertEqual(parse_numbered("1\t他說他明天\n會回來。", 1), ["他說他明天會回來。"])

    def test_out_of_range_number_is_continuation_text(self):
        self.assertEqual(parse_numbered("1\tThe year was\n1999. Then\n2\tOk", 2),
                         ["The year was 1999. Then", "Ok"])


class PerLineFallbackRateLimitTests(unittest.TestCase):
    @patch("srt_translate.time.sleep")
    def test_rate_limit_during_per_line_fallback_aborts(self, _sleep):
        calls = []

        def provider(texts, _source, _target):
            calls.append(list(texts))
            if len(texts) > 1:
                raise TranslationError("malformed batch")
            raise RateLimitError("429", retry_after=0)

        segments = [Segment(i, f"line {i}", [], False) for i in range(5)]

        with self.assertRaises(RateLimitError):
            run_translation(segments, provider, rate_retries=0)

        self.assertEqual(len(calls), 2)


class WrapLatinTests(unittest.TestCase):
    def test_never_emits_an_empty_line(self):
        text = "xxxxxxxx xxxxxxxx xxxxxxxx xxxxxxxx xx " + "x" * 40

        for max_lines in range(1, 7):
            lines = wrap_latin(text, 10, max_lines)
            self.assertTrue(all(line.strip() for line in lines), (max_lines, lines))
            self.assertLessEqual(len(lines), max_lines)
            self.assertEqual(" ".join(lines), text)


class DeeplValidationTests(unittest.TestCase):
    @patch("srt_translate._post_json")
    def test_malformed_responses_are_retryable_errors(self, post_json):
        provider = make_deepl("secret", Throttle())
        for response in ({}, {"translations": [{"text": "Eins"}]},
                         {"translations": [{"text": "Eins"}, {"text": None}]},
                         {"translations": "nope"}):
            post_json.return_value = response
            with self.subTest(response=response):
                with self.assertRaisesRegex(TranslationError, "unexpected response"):
                    provider(["One", "Two"], "English", "de")


class RateLimitResetParsingTests(unittest.TestCase):
    def test_rfc3339_reset_is_relative_to_now(self):
        moment = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=30)
        stamp = moment.strftime("%Y-%m-%dT%H:%M:%SZ")

        self.assertAlmostEqual(_parse_rate_limit_reset(stamp), 30, delta=2)
        self.assertEqual(_parse_rate_limit_reset("2020-01-01T00:00:00Z"), 0.0)

    def test_go_durations(self):
        self.assertEqual(_parse_rate_limit_reset("6m0s"), 360.0)
        self.assertEqual(_parse_rate_limit_reset("1s"), 1.0)
        self.assertEqual(_parse_rate_limit_reset("250ms"), 0.25)
        self.assertEqual(_parse_rate_limit_reset("1h2m3.5s"), 3723.5)

    def test_plain_seconds_and_garbage(self):
        self.assertEqual(_parse_rate_limit_reset("12"), 12.0)
        self.assertIsNone(_parse_rate_limit_reset("soon"))
        self.assertIsNone(_parse_rate_limit_reset(None))


if __name__ == "__main__":
    unittest.main()
