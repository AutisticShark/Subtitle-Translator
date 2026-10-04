"""Regression tests for translation-engine and subtitle-format fixes."""

import datetime
import http.server
import threading
import time
import unittest
from unittest.mock import patch

from srt_translate import (
    FatalTranslationError,
    RateLimitError,
    Segment,
    Throttle,
    TranslationError,
    _parse_rate_limit_reset,
    _post_json,
    make_deepl,
    rebuild_cues,
    segment_cue,
    translate_segments,
)

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
