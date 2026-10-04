import hashlib
import unittest
from unittest.mock import patch

from srt_translate import (
    Cue,
    FatalTranslationError,
    Segment,
    RateLimitError,
    Throttle,
    TranslationCanceled,
    TranslationError,
    _parse_retry_after,
    display_width,
    mask_tags,
    parse_numbered,
    parse_srt,
    rebuild_cues,
    segment_cue,
    translate_segments,
    unmask_tags,
    wrap_cjk,
    wrap_latin,
)


class SrtParsingTests(unittest.TestCase):
    def test_tolerates_bom_junk_missing_indices_and_internal_blank_lines(self):
        source = (
            "\ufeffjunk\r\n"
            "00:00:01,000 --> 00:00:02,000  X1:10\r\n"
            "First line\r\n\r\ncontinued\r\n\r\n"
            "9\r\n00:00:03.000 --> 00:00:04.500\r\nLast line"
        )

        cues = parse_srt(source)

        self.assertEqual(len(cues), 2)
        self.assertEqual(cues[0].index, 1)
        self.assertEqual(cues[0].rest, "  X1:10")
        self.assertEqual(cues[0].lines, ["First line", "", "continued"])
        self.assertEqual(cues[1].index, 9)
        self.assertEqual(cues[1].start, "00:00:03.000")

class SegmentationAndWrappingTests(unittest.TestCase):
    def test_masks_and_restores_inline_tags(self):
        masked, tags = mask_tags("<i>Hello</i> {\\an8}world")

        self.assertEqual(tags, ["<i>", "</i>", "{\\an8}"])
        self.assertEqual(unmask_tags(masked.replace("⟦1⟧", "⟦ 1 ⟧"), tags),
                         "<i>Hello</i> {\\an8}world")

    def test_speaker_dialogue_is_split_and_dashes_are_restored(self):
        cue = Cue(1, "00:00:01,000", "00:00:02,000", "", [
            "- <i>Hello</i>",
            "— Goodbye",
        ])
        segments = segment_cue(cue, 0)

        self.assertEqual([segment.text for segment in segments], ["⟦0⟧Hello⟦1⟧", "Goodbye"])
        rebuilt = rebuild_cues([cue], segments, ["⟦0⟧Hola⟦1⟧", "Adiós"],
                               "es", 20, 2)
        self.assertEqual(rebuilt[0].lines, ["- <i>Hola</i>", "- Adiós"])

    def test_single_dashed_wrapped_cue_remains_one_segment(self):
        cue = Cue(1, "00:00:01,000", "00:00:02,000", "", ["- A long", "sentence"])

        segments = segment_cue(cue, 0)

        self.assertEqual(len(segments), 1)
        self.assertTrue(segments[0].dashed)
        self.assertEqual(segments[0].text, "A long sentence")

    def test_cjk_width_and_wrapping_keep_closing_punctuation_off_a_new_line(self):
        self.assertEqual(display_width("中A"), 1.5)
        lines = wrap_cjk("你好，世界！再見。", 3, 2)

        self.assertLessEqual(len(lines), 2)
        self.assertFalse(any(line.startswith(tuple("，。！")) for line in lines))
        self.assertEqual("".join(lines), "你好，世界！再見。")

    def test_latin_wrapping_preserves_all_words(self):
        lines = wrap_latin("one two three four", 7, 2)

        self.assertEqual(" ".join(lines), "one two three four")
        self.assertEqual(lines, ["one two", "three four"])

    def test_rebuild_keeps_untranslated_latin_words_intact_for_cjk_target(self):
        cue = Cue(1, "00:00:01,000", "00:00:02,000", "", ["placeholder"])
        segment = Segment(0, "placeholder", [], False)

        rebuilt = rebuild_cues([cue], [segment], ["Sherlock Holmes"], "zh-TW", 4, 2)

        self.assertEqual(rebuilt[0].lines, ["Sherlock", "Holmes"])


class ProviderOutputParsingTests(unittest.TestCase):
    def test_parse_numbered_accepts_common_separators_and_reorders(self):
        # Unnumbered text before the first entry is preamble; after an entry
        # it would be a continuation of that entry.
        output = "ignored heading\n2) second\n1： first"

        self.assertEqual(parse_numbered(output, 2), ["first", "second"])

    def test_parse_numbered_rejects_missing_lines(self):
        with self.assertRaisesRegex(TranslationError, r"missing \[2\]"):
            parse_numbered("1\tone", 2)

    def test_retry_after_parses_seconds_and_rejects_garbage(self):
        self.assertEqual(_parse_retry_after(" 2.5 "), 2.5)
        self.assertEqual(_parse_retry_after("-1"), 0.0)
        self.assertIsNone(_parse_retry_after("not a date"))


class TranslationDriverTests(unittest.TestCase):
    @staticmethod
    def _translate(segments, provider, **overrides):
        options = {
            "tgt_key": "es",
            "src": "English",
            "batch_size": 20,
            "retries": 1,
            "rate_retries": 1,
            "throttle": Throttle(),
            "cache": {},
            "workers": 1,
            "quiet": True,
        }
        options.update(overrides)
        return translate_segments(segments, provider, **options)

    def test_cache_hits_skip_provider_and_are_reported_as_complete(self):
        segment = Segment(0, "Hello", [], False)
        key = hashlib.sha256("es\0Hello".encode()).hexdigest()[:24]
        progress = []

        result = self._translate(
            [segment],
            lambda *_args: self.fail("provider should not be called"),
            cache={key: "Hola"},
            progress_callback=lambda done, total: progress.append((done, total)),
        )

        self.assertEqual(result, ["Hola"])
        self.assertEqual(progress, [(1, 1)])

    @patch("srt_translate.time.sleep")
    @patch("srt_translate.random.uniform", return_value=0)
    def test_transient_failure_is_retried(self, _random, _sleep):
        calls = []

        def provider(texts, _source, _target):
            calls.append(texts)
            if len(calls) == 1:
                raise TranslationError("temporary")
            return ["Hola"]

        result = self._translate(
            [Segment(0, "Hello", [], False)], provider, retries=2,
        )

        self.assertEqual(result, ["Hola"])
        self.assertEqual(len(calls), 2)

    def test_batch_failure_falls_back_per_line_and_passes_through_a_bad_line(self):
        def provider(texts, _source, _target):
            if len(texts) > 1 or texts == ["bad"]:
                raise TranslationError("cannot translate")
            return [texts[0].upper()]

        segments = [Segment(0, "good", [], False), Segment(1, "bad", [], False)]

        result = self._translate(segments, provider)

        self.assertEqual(result, ["GOOD", "bad"])

    def test_pass_through_lines_are_reported_and_never_cached(self):
        def provider(texts, _source, _target):
            if len(texts) > 1 or texts == ["bad"]:
                raise TranslationError("cannot translate")
            return [texts[0].upper()]

        segments = [Segment(0, "good", [], False), Segment(1, "bad", [], False)]
        reported = []
        cache = {}

        self._translate(
            segments, provider, cache=cache, fallback_callback=reported.append,
        )

        self.assertEqual(reported, [1])
        self.assertEqual(list(cache.values()), ["GOOD"])

    @patch("srt_translate.time.sleep")
    def test_exhausted_rate_limit_aborts_without_per_line_fallback(self, _sleep):
        calls = []

        def provider(texts, _source, _target):
            calls.append(texts)
            raise RateLimitError("429", retry_after=0)

        segments = [Segment(i, f"line {i}", [], False) for i in range(5)]

        with self.assertRaises(RateLimitError):
            self._translate(segments, provider)

        # One batch attempt plus its single rate-limit retry; no per-line requests.
        self.assertEqual(len(calls), 2)
        self.assertTrue(all(len(texts) == 5 for texts in calls))

    def test_fatal_failure_aborts_without_per_line_fallback(self):
        def provider(_texts, _source, _target):
            raise FatalTranslationError("bad credentials")

        with self.assertRaisesRegex(FatalTranslationError, "bad credentials"):
            self._translate([Segment(0, "Hello", [], False)], provider)

    def test_cancellation_is_checked_before_work_starts(self):
        with self.assertRaises(TranslationCanceled):
            self._translate(
                [Segment(0, "Hello", [], False)],
                lambda *_args: ["Hola"],
                cancel_callback=lambda: True,
            )


if __name__ == "__main__":
    unittest.main()
