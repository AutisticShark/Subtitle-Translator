import pytest

from settings_schema import (
    INTEGER_SETTINGS,
    validate_choice_and_flag_settings,
    validate_numeric_settings,
)


@pytest.mark.parametrize("value", ["4.5", "4.0", "abc", "", 2.5, None, "1e1"])
def test_integer_settings_reject_anything_that_int_cannot_round_trip(value):
    assert validate_numeric_settings({"batch_size": value})


@pytest.mark.parametrize("key,value", [("batch_size", 0), ("batch_size", 101), ("workers", 17),
                                       ("max_lines", 6), ("user_job_limit", -1)])
def test_integer_settings_enforce_bounds(key, value):
    assert validate_numeric_settings({key: value})


def test_every_integer_setting_accepts_its_bounds_as_int_or_string():
    for key, (minimum, maximum) in INTEGER_SETTINGS.items():
        for value in (minimum, maximum, str(minimum), f" {maximum} "):
            assert validate_numeric_settings({key: value}) is None, (key, value)


def test_number_settings_accept_decimals_within_bounds_only():
    assert validate_numeric_settings({"width": "16.5", "rpm": 60}) is None
    assert validate_numeric_settings({"width": 3})
    assert validate_numeric_settings({"rpm": "fast"})


def test_hostname_is_normalized_and_flags_are_validated():
    payload = {"captcha_hostname": " Example.COM. "}
    assert validate_choice_and_flag_settings(payload, ("anthropic",)) is None
    assert payload["captcha_hostname"] == "example.com"
    assert validate_choice_and_flag_settings({"captcha_hostname": "https://x.test"}, ())
    assert validate_choice_and_flag_settings({"registration_enabled": "yes"}, ())
    assert validate_choice_and_flag_settings({"default_provider": "nope"}, ("anthropic",))
    assert validate_choice_and_flag_settings({"captcha_provider": "none"}, ()) is None
