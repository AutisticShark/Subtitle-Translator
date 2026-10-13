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

        tree = ast.parse("\n".join(
            path.read_text("utf-8") for path in project_root.glob("*.py")
        ))
        def constants(node):
            if isinstance(node, ast.Constant) and isinstance(node.value, str):
                return [node.value]
            if isinstance(node, ast.IfExp):  # tr("A" if condition else "B")
                return constants(node.body) + constants(node.orelse)
            return []

        for node in ast.walk(tree):
            if (isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
                    and node.func.id == "tr" and node.args):
                source_strings.update(constants(node.args[0]))

        call_pattern = re.compile(r"\b(?:t|tr)\(\s*(['\"])(.*?)\1", re.DOTALL)
        # t(condition ? 'A' : 'B') selects between two static keys.
        ternary_pattern = re.compile(
            r"\bt\(\s*[^()'\"?]+\?\s*(['\"])(.*?)\1\s*:\s*(['\"])(.*?)\3", re.DOTALL,
        )
        for relative_path in ("static/app.js", "templates/index.html"):
            text = (project_root / relative_path).read_text("utf-8")
            source_strings.update(match[1] for match in call_pattern.findall(text))
            for match in ternary_pattern.findall(text):
                source_strings.update((match[1], match[3]))
        # Heading pairs that showView translates through a variable.
        script = (project_root / "static/app.js").read_text("utf-8")
        labels = re.search(r"const viewLabels = \{(.*?)\n\};", script, re.DOTALL)
        self.assertIsNotNone(labels)
        source_strings.update(match[1] for match in re.findall(r"(['\"])(.*?)\1", labels[1]))

        for locale, messages in i18n.CATALOGS.items():
            if locale == i18n.DEFAULT_LOCALE:
                continue
            with self.subTest(locale=locale):
                self.assertEqual(sorted(source_strings - messages.keys()), [])


if __name__ == "__main__":
    unittest.main()
