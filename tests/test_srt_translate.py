import hashlib
import io
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from srt_translate import (
    Cue,
    FatalTranslationError,
    Segment,
    Throttle,
    TranslationCanceled,
    TranslationError,
    _parse_retry_after,
    display_width,
    load_translation_cache,
    main,
    mask_tags,
    output_path,
    parse_numbered,
    parse_srt,
    rebuild_cues,
    resolve_key,
    save_translation_cache,
    segment_cue,
    translate_segments,
    unmask_tags,
    wrap_cjk,
    wrap_latin,
    write_srt,
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

    def test_write_srt_can_preserve_bom_crlf_and_renumber(self):
        cues = [Cue(7, "00:00:01,000", "00:00:02,000", " position:10%", ["Hi"])]
        with tempfile.TemporaryDirectory() as temp_dir:
            destination = Path(temp_dir) / "out.srt"
            write_srt(cues, destination, bom=True, crlf=True, renumber=True)
            raw = destination.read_bytes()

        self.assertTrue(raw.startswith(b"\xef\xbb\xbf1\r\n"))
        self.assertIn(b"00:00:01,000 --> 00:00:02,000 position:10%\r\n", raw)
        self.assertTrue(raw.endswith(b"\r\n"))


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
        output = "2) second\nignored heading\n1： first"

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


class CliTests(unittest.TestCase):
    def test_output_path_replaces_english_suffix(self):
        self.assertEqual(
            output_path(Path("episode.en.srt"), "zh-TW", Path("translated")),
            Path("translated/episode.zh.tw.srt"),
        )

    def test_translation_cache_rejects_unexpected_untrusted_entries(self):
        valid_key = "a" * 24
        with tempfile.TemporaryDirectory() as temp_dir:
            cache_path = Path(temp_dir) / "input.srt.xlate-cache.json"
            with cache_path.open("w", encoding="utf-8") as cache_file:
                json.dump({
                    valid_key: "Hola",
                    "../outside.json": "not a cache key",
                    "b" * 24: ["not", "text"],
                }, cache_file)

            self.assertEqual(load_translation_cache(cache_path), {valid_key: "Hola"})

    def test_translation_cache_treats_path_like_values_as_json_data(self):
        cache = {"a" * 24: "../../outside.json"}
        with tempfile.TemporaryDirectory() as temp_dir:
            cache_path = Path(temp_dir) / "input.srt.xlate-cache.json"

            save_translation_cache(cache_path, cache)

            with cache_path.open("r", encoding="utf-8") as cache_file:
                self.assertEqual(json.load(cache_file), cache)
            self.assertEqual(set(Path(temp_dir).iterdir()), {cache_path})

    def test_resolve_key_supports_file_stdin_and_environment(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            key_file = Path(temp_dir) / "key.txt"
            key_file.write_text("\nfile-secret\n", "utf-8")
            self.assertEqual(resolve_key(f"@{key_file}", "UNUSED_KEY", "Test"), "file-secret")

        with patch("srt_translate.sys.stdin", io.StringIO("\nstdin-secret\n")):
            self.assertEqual(resolve_key("-", "UNUSED_KEY", "Test"), "stdin-secret")
        with patch.dict("os.environ", {"TEST_PROVIDER_KEY": " env-secret "}):
            self.assertEqual(resolve_key(None, "TEST_PROVIDER_KEY", "Test"), "env-secret")

    def test_echo_cli_translates_srt_without_credentials(self):
        source_bytes = b"1\r\n00:00:01,000 --> 00:00:02,000\r\nHello\r\n\r\n"
        with tempfile.TemporaryDirectory() as temp_dir:
            source = Path(temp_dir) / "episode.en.srt"
            source.write_bytes(source_bytes)

            result = main([
                str(source), "--provider", "echo", "--langs", "es", "--quiet",
            ])

            output = Path(temp_dir) / "episode.es.srt"
            self.assertEqual(result, 0)
            self.assertTrue(output.exists())
            self.assertIn("[es] Hello", output.read_text("utf-8"))
            self.assertIn(b"\r\n", output.read_bytes())
            self.assertTrue(source.with_suffix(".srt.xlate-cache.json").exists())


if __name__ == "__main__":
    unittest.main()
