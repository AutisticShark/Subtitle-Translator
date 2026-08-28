import unittest
from unittest.mock import patch

from srt_translate import (
    FatalTranslationError,
    Throttle,
    TranslationError,
    make_anthropic,
    make_deepl,
    make_google,
    make_openai,
)


class LlmProviderTests(unittest.TestCase):
    @patch("srt_translate._post_json")
    def test_anthropic_builds_numbered_request_and_parses_response(self, post_json):
        post_json.return_value = {
            "content": [{"type": "text", "text": "1\tHola\n2\tAdiós"}]
        }
        throttle = Throttle()

        provider = make_anthropic("claude-test", "secret", throttle)
        translated = provider(["Hello", "Goodbye"], "English", "es")

        self.assertEqual(translated, ["Hola", "Adiós"])
        url, headers, payload = post_json.call_args.args
        self.assertEqual(url, "https://api.anthropic.com/v1/messages")
        self.assertEqual(headers["x-api-key"], "secret")
        self.assertEqual(payload["model"], "claude-test")
        self.assertEqual(payload["messages"][0]["content"], "1\tHello\n2\tGoodbye")
        self.assertIs(post_json.call_args.kwargs["throttle"], throttle)

    @patch("srt_translate._post_json")
    def test_openai_normalizes_base_url_and_rejects_bad_response(self, post_json):
        post_json.return_value = {"choices": []}
        provider = make_openai("model", "secret", Throttle(), "https://example.test/v1/")

        with self.assertRaisesRegex(TranslationError, "unexpected response"):
            provider(["Hello"], "English", "de")

        self.assertEqual(post_json.call_args.args[0],
                         "https://example.test/v1/chat/completions")
        self.assertEqual(post_json.call_args.args[1]["authorization"], "Bearer secret")

    @patch("srt_translate._post_json")
    def test_deepl_selects_free_endpoint_and_target_code(self, post_json):
        post_json.return_value = {"translations": [{"text": "Hallo"}]}
        throttle = Throttle()

        provider = make_deepl("secret:fx", throttle)
        translated = provider(["Hello"], "English", "de")

        self.assertEqual(translated, ["Hallo"])
        self.assertEqual(post_json.call_args.args[0], "https://api-free.deepl.com/v2/translate")
        self.assertEqual(post_json.call_args.args[2]["target_lang"], "DE")
        self.assertIs(post_json.call_args.kwargs["throttle"], throttle)


class GoogleCloudTranslationProviderTests(unittest.TestCase):
    @patch("srt_translate._post_json")
    def test_translates_a_batch_and_preserves_order(self, post_json):
        post_json.return_value = {
            "data": {
                "translations": [
                    {"translatedText": "你好 &amp; 再見"},
                    {"translatedText": "第二行"},
                ]
            }
        }
        throttle = Throttle()

        provider = make_google("key with /?", throttle)
        translated = provider(["Hello & goodbye", "Second line"], "English", "zh-TW")

        self.assertEqual(translated, ["你好 & 再見", "第二行"])
        post_json.assert_called_once_with(
            "https://translation.googleapis.com/language/translate/v2?key=key+with+%2F%3F",
            {"content-type": "application/json; charset=utf-8"},
            {
                "q": ["Hello & goodbye", "Second line"],
                "target": "zh-TW",
                "format": "text",
                "source": "en",
            },
            throttle=throttle,
        )

    @patch("srt_translate._post_json")
    def test_uses_detection_for_an_unrecognized_source_name(self, post_json):
        post_json.return_value = {
            "data": {"translations": [{"translatedText": "Hola"}]}
        }

        provider = make_google("key", Throttle())
        provider(["Hello"], "Detect automatically", "es")

        payload = post_json.call_args.args[2]
        self.assertNotIn("source", payload)

    @patch("srt_translate._post_json")
    def test_rejects_a_response_with_the_wrong_translation_count(self, post_json):
        post_json.return_value = {
            "data": {"translations": [{"translatedText": "Only one"}]}
        }

        provider = make_google("key", Throttle())
        with self.assertRaisesRegex(TranslationError, "unexpected response"):
            provider(["One", "Two"], "en", "de")

    def test_rejects_more_than_google_batch_limit(self):
        provider = make_google("key", Throttle())
        with self.assertRaisesRegex(FatalTranslationError, "at most 128"):
            provider(["text"] * 129, "en", "de")


if __name__ == "__main__":
    unittest.main()
