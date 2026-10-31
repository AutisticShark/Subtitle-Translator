"""Regression tests for the translation-engine and subtitle-format audit fixes."""

import threading
import time
import unittest
from unittest.mock import patch

from srt_translate import (
    MAX_RATE_LIMIT_WAIT,
    Cue,
    FatalTranslationError,
    RateLimitError,
    Segment,
    Throttle,
    TranslationError,
    _parse_rate_limit_reset,
    _parse_retry_after,
    display_width,
    make_anthropic,
    make_openai,
    mask_tags,
    parse_srt,
    rebuild_cues,
    segment_cue,
    translate_segments,
    wrap_latin,
)
from subtitle_formats import parse_subtitle

CUE = ("00:00:01,000", "00:00:02,000", "")


def run_translation(segments, provider, **overrides):
    options = {
        "tgt_key": "es", "src": "English", "batch_size": 20, "retries": 1,
        "rate_retries": 1, "throttle": Throttle(), "cache": {}, "workers": 1,
    }
    options.update(overrides)
    return translate_segments(segments, provider, **options)


def translate_document(document, provider, **overrides):
    segments = []
    for cue_i, cue in enumerate(document.cues):
        segments.extend(segment_cue(cue, cue_i))
    output = run_translation(segments, provider, **overrides)
    cues = rebuild_cues(document.cues, segments, output, "es", 40, 2)
    return document.clone_with_cues(cues).to_bytes()


def recording_provider(transform):
    calls = []

    def provider(texts, _source, _target):
        calls.append(list(texts))
        return [transform(text) for text in texts]

    return provider, calls


class WideCharacterWrapTests(unittest.TestCase):
    KOREAN = "안녕하세요 오늘 날씨가 정말 좋네요 그렇죠"

    def test_korean_lines_respect_the_width_and_break_at_spaces(self):
        cue = Cue(1, *CUE, ["Hello, the weather is really nice today, isn't it"])
        segments = segment_cue(cue, 0)

        lines = rebuild_cues([cue], segments, [self.KOREAN], "ko", 10, 3)[0].lines

        self.assertGreater(len(lines), 1)
        self.assertTrue(all(display_width(line) <= 10 for line in lines), lines)
        self.assertEqual(" ".join(lines), self.KOREAN)

    def test_limit_is_in_half_width_cells(self):
        # Each Hangul syllable is two cells; Latin letters are one.
        self.assertEqual(wrap_latin("가나다 라마바", 13), ["가나다 라마바"])
        self.assertEqual(wrap_latin("가나다 라마바", 12), ["가나다", "라마바"])
        self.assertEqual(wrap_latin("abcdef ghijkl", 13), ["abcdef ghijkl"])


class MalformedProviderReplyTests(unittest.TestCase):
    def assert_retryable(self, provider, response):
        with patch("srt_translate._post_json", return_value=response):
            with self.assertRaises(TranslationError) as caught:
                provider(["Hello"], "English", "es")
        self.assertNotIsInstance(caught.exception, (FatalTranslationError, RateLimitError))

    def test_anthropic_null_or_malformed_content_is_retryable(self):
        provider = make_anthropic("model", "key", Throttle())
        for response in ({"content": None}, {"content": "1\tHola"}, {"content": ["x"]},
                         {"content": [{"type": "text", "text": None}]},
                         {"content": [{"type": "text", "text": ["1\tHola"]}]}):
            with self.subTest(response=response):
                self.assert_retryable(provider, response)

    def test_openai_refusal_and_list_content_are_retryable(self):
        provider = make_openai("model", "key", Throttle())
        for content in (None, [{"type": "text", "text": "1\tHola"}], 7):
            response = {"choices": [{"message": {"content": content, "refusal": "No."}}]}
            with self.subTest(content=content):
                self.assert_retryable(provider, response)

    def test_valid_replies_still_parse(self):
        anthropic = make_anthropic("model", "key", Throttle())
        openai = make_openai("model", "key", Throttle())
        with patch("srt_translate._post_json",
                   return_value={"content": [{"type": "text", "text": "1\tHola"}]}):
            self.assertEqual(anthropic(["Hello"], "English", "es"), ["Hola"])
        with patch("srt_translate._post_json",
                   return_value={"choices": [{"message": {"content": "1\tHola"}}]}):
            self.assertEqual(openai(["Hello"], "English", "es"), ["Hola"])

    def test_refusal_falls_back_to_source_instead_of_failing_the_job(self):
        provider = make_openai("model", "key", Throttle())
        reported = []
        with patch("srt_translate._post_json",
                   return_value={"choices": [{"message": {"content": None}}]}):
            output = run_translation([Segment(0, "Hello", [], False)], provider,
                                     fallback_callback=reported.append)

        self.assertEqual(output, ["Hello"])
        self.assertEqual(reported, [1])


class SingleDashSpeakerPairTests(unittest.TestCase):
    def test_undashed_line_answered_by_a_dashed_line_is_a_pair(self):
        cue = Cue(1, *CUE, ["Where are you going?", "-To the store."])

        segments = segment_cue(cue, 0)

        self.assertEqual([(s.text, s.dashed) for s in segments],
                         [("Where are you going?", False), ("To the store.", True)])
        rebuilt = rebuild_cues([cue], segments, ["¿Adónde vas?", "A la tienda."],
                               "es", 40, 2)
        self.assertEqual(rebuilt[0].lines, ["¿Adónde vas?", "- A la tienda."])

    def test_other_single_dash_shapes_stay_one_segment(self):
        for lines in (["-Hello there"], ["- A long", "sentence"],
                      ["I was going", "to say", "- really?"], ["Where are", "you going?"]):
            with self.subTest(lines=lines):
                self.assertEqual(len(segment_cue(Cue(1, *CUE, lines), 0)), 1)


class WebVttEntityTests(unittest.TestCase):
    SOURCE = (
        "WEBVTT\n\n"
        "00:01.000 --> 00:02.000\nTom &amp; Jerry &lt;3\n\n"
        "00:03.000 --> 00:04.000\n<c.loud>Fish &amp; chips &gt; all</c>\n\n"
        "00:05.000 --> 00:06.000\n&nbsp;\n"
    ).encode()

    def test_entities_are_decoded_for_the_provider(self):
        document = parse_subtitle(self.SOURCE, ".vtt")
        provider, calls = recording_provider(lambda text: text)

        translate_document(document, provider)

        self.assertEqual(calls, [["Tom & Jerry <3", "⟦0⟧Fish & chips > all⟦1⟧"]])

    def test_unchanged_text_round_trips_byte_for_byte(self):
        document = parse_subtitle(self.SOURCE, ".vtt")
        provider, _calls = recording_provider(lambda text: text)

        self.assertEqual(translate_document(document, provider), self.SOURCE)

    def test_translated_text_is_escaped_but_tags_are_not(self):
        document = parse_subtitle(self.SOURCE, ".vtt")
        provider, _calls = recording_provider(lambda text: text + " <&>")

        rendered = translate_document(document, provider).decode()

        self.assertIn("\nTom &amp; Jerry &lt;3 &lt;&amp;&gt;\n", rendered)
        self.assertIn("\n<c.loud>Fish &amp; chips &gt; all</c> &lt;&amp;&gt;\n", rendered)
        # The markup-only cue is not translated and keeps its original bytes.
        self.assertIn("\n00:05.000 --> 00:06.000\n&nbsp;\n", rendered)

    def test_passed_through_cue_keeps_its_escaping(self):
        document = parse_subtitle(self.SOURCE, ".vtt")

        def failing(_texts, _source, _target):
            raise TranslationError("down")

        self.assertEqual(translate_document(document, failing), self.SOURCE)

    def test_escaped_markup_stays_literal_text(self):
        masked, tags = mask_tags("&lt;i&gt;hi&lt;/i&gt; <i>x</i>", entities=True)

        self.assertEqual(masked, "<i>hi</i> ⟦0⟧x⟦1⟧")
        self.assertEqual(tags, ["<i>", "</i>"])

    def test_reference_to_a_reserved_character_is_masked(self):
        masked, tags = mask_tags("a &#x27e6;0&#x27e7; b", entities=True)

        self.assertEqual(masked, "a ⟦0⟧0⟦1⟧ b")
        self.assertEqual(tags, ["⟦", "⟧"])

    def test_srt_text_is_not_decoded(self):
        segments = segment_cue(Cue(1, *CUE, ["Tom &amp; Jerry"]), 0)

        self.assertEqual(segments[0].text, "Tom &amp; Jerry")


class RateLimitWaitTests(unittest.TestCase):
    def test_non_finite_header_values_are_rejected(self):
        for value in ("inf", "Infinity", "1e400", "nan"):
            with self.subTest(value=value):
                self.assertIsNone(_parse_retry_after(value))
                self.assertIsNone(_parse_rate_limit_reset(value))
        self.assertIsNone(_parse_rate_limit_reset("9" * 400 + "h"))

    def test_too_long_wait_gives_up_without_parking_workers(self):
        for retry_after in (MAX_RATE_LIMIT_WAIT + 1, 1e300, float("inf"), float("nan")):
            calls = []
            throttle = Throttle()

            def provider(texts, _source, _target, calls=calls, retry_after=retry_after):
                calls.append(texts)
                raise RateLimitError("429", retry_after=retry_after)

            segments = [Segment(0, "Hello", [], False)]
            with self.subTest(retry_after=retry_after):
                with self.assertRaises(RateLimitError):
                    run_translation(segments, provider, rate_retries=5, throttle=throttle)
                self.assertEqual(len(calls), 1)
                self.assertEqual(throttle.penalise(0), 0)


class EmptyTranslationTests(unittest.TestCase):
    def test_empty_reply_for_text_is_retried_and_never_cached(self):
        def provider(texts, _source, _target):
            return ["" if text == "bad" else text.upper() for text in texts]

        segments = [Segment(0, "good", [], False), Segment(1, "bad", [], False),
                    Segment(2, "⟦0⟧bad too", ["<i>"], False)]
        reported = []
        cache = {}

        output = run_translation(
            [*segments[:2]], provider, cache=cache, fallback_callback=reported.append,
        )

        self.assertEqual(output, ["GOOD", "bad"])
        self.assertEqual(reported, [1])
        self.assertEqual(list(cache.values()), ["GOOD"])
        # A reply that keeps only the markup is empty too.
        output = run_translation([segments[2]], lambda texts, *_: ["⟦0⟧ "],
                                 fallback_callback=reported.append)
        self.assertEqual(output, ["⟦0⟧bad too"])
        self.assertEqual(reported, [1, 1])


class SingleSegmentFallbackTests(unittest.TestCase):
    @patch("srt_translate._retry_delay", return_value=0)
    def test_failed_single_segment_batch_is_not_retried_again(self, _delay):
        calls = []

        def provider(texts, _source, _target):
            calls.append(list(texts))
            raise TranslationError("broken")

        reported = []

        output = run_translation([Segment(0, "Hello", [], False)], provider, retries=2,
                                 fallback_callback=reported.append)

        self.assertEqual(output, ["Hello"])
        self.assertEqual(len(calls), 2)
        self.assertEqual(reported, [1])


class SrtIndexTests(unittest.TestCase):
    def test_non_ascii_digit_lines_are_not_indices(self):
        source = ("1\n00:00:01,000 --> 00:00:02,000\nHello\n\n"
                  "①\n00:00:03,000 --> 00:00:04,000\nWorld\n\n"
                  "²\n00:00:05,000 --> 00:00:06,000\nAgain\n")

        cues = parse_srt(source)

        self.assertEqual(len(cues), 3)
        self.assertEqual([cue.index for cue in cues], [1, 2, 3])
        self.assertEqual(cues[1].lines[0], "World")
        self.assertEqual(len(parse_subtitle(source.encode(), ".srt").cues), 3)


class AbortStopsInFlightWorkersTests(unittest.TestCase):
    def test_workers_stop_calling_the_provider_after_a_fatal_error(self):
        other_started = threading.Event()
        release = threading.Event()
        other_calls = []

        def provider(texts, _source, _target):
            if texts == ["fatal"]:
                other_started.wait(5)
                raise FatalTranslationError("bad key")
            other_calls.append(texts)
            other_started.set()
            release.wait(5)
            raise RateLimitError("429", retry_after=0)

        segments = [Segment(0, "fatal", [], False), Segment(1, "other", [], False)]

        with self.assertRaises(FatalTranslationError):
            run_translation(segments, provider, batch_size=1, workers=2, rate_retries=5)
        release.set()
        time.sleep(0.5)

        # Without the stop signal the other worker retries straight away.
        self.assertEqual(len(other_calls), 1)


class VttTimestampTests(unittest.TestCase):
    def test_three_digit_hours_round_trip(self):
        source = b"WEBVTT\n\n100:00:01.000 --> 100:00:02.500\nHello\n"

        document = parse_subtitle(source, ".vtt")

        self.assertEqual(document.cues[0].start, "100:00:01.000")
        self.assertEqual(document.to_bytes(), source)


if __name__ == "__main__":
    unittest.main()
