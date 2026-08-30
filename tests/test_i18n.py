import ast
import re
import unittest
from pathlib import Path
from string import Formatter

import i18n


class InternationalizationTests(unittest.TestCase):
    def test_locale_normalization_and_fallback(self):
        self.assertEqual(i18n.normalize_locale("en-US"), "en")
        self.assertEqual(i18n.normalize_locale("zh"), "zh-CN")
        self.assertEqual(i18n.normalize_locale("zh_Hans"), "zh-CN")
        self.assertEqual(i18n.normalize_locale("zh-CN"), "zh-CN")
        self.assertEqual(i18n.normalize_locale("zh_Hant"), "zh-TW")
        self.assertEqual(i18n.normalize_locale("zh-TW"), "zh-TW")
        self.assertIsNone(i18n.normalize_locale("fr-FR"))
        self.assertEqual(
            i18n.select_locale("unsupported", "", ["zh-Hant", "en-US"]),
            "zh-TW",
        )
        self.assertEqual(i18n.select_locale(None, None, ["fr-FR"]), "en")

    def test_catalogs_have_valid_interpolation_placeholders(self):
        for locale, messages in i18n.CATALOGS.items():
            with self.subTest(locale=locale):
                for source, translated in messages.items():
                    source_fields = {
                        field_name for _, field_name, _, _ in Formatter().parse(source)
                        if field_name
                    }
                    translated_fields = {
                        field_name for _, field_name, _, _ in Formatter().parse(translated)
                        if field_name
                    }
                    self.assertLessEqual(translated_fields, source_fields)

    def test_every_non_english_catalog_covers_static_translation_calls(self):
        project_root = Path(__file__).resolve().parents[1]
        source_strings = set()

        tree = ast.parse((project_root / "webapp.py").read_text("utf-8"))
        for node in ast.walk(tree):
            if (isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
                    and node.func.id == "tr" and node.args
                    and isinstance(node.args[0], ast.Constant)
                    and isinstance(node.args[0].value, str)):
                source_strings.add(node.args[0].value)

        call_pattern = re.compile(r"\b(?:t|tr)\(\s*(['\"])(.*?)\1", re.DOTALL)
        for relative_path in ("static/app.js", "templates/index.html"):
            text = (project_root / relative_path).read_text("utf-8")
            source_strings.update(match[1] for match in call_pattern.findall(text))

        for locale, messages in i18n.CATALOGS.items():
            if locale == i18n.DEFAULT_LOCALE:
                continue
            with self.subTest(locale=locale):
                self.assertEqual(sorted(source_strings - messages.keys()), [])


if __name__ == "__main__":
    unittest.main()
