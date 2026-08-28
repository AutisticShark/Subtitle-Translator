import unittest
from unittest.mock import patch

from srt_translate import (
    FatalTranslationError,
    Throttle,
    TranslationError,
    make_google,
)


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
