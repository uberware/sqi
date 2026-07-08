# SPDX-License-Identifier: AGPL-3.0-or-later
"""Package import smoke tests."""

import sqi_submitter


def test_version_is_a_string() -> None:
    assert isinstance(sqi_submitter.__version__, str)
    assert sqi_submitter.__version__


def test_core_package_imports_without_qt_or_dcc_modules() -> None:
    import sqi_submitter.core  # noqa: F401
