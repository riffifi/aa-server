import asyncio
from unittest.mock import AsyncMock
import pytest
from app.pulse import BrowserLogins


def make(monkeypatch, **env):
    for key, value in env.items():
        monkeypatch.setenv(key, str(value))
    browser = BrowserLogins(vault=None, changed=AsyncMock())
    return browser


def test_logins_and_renewals_use_separate_budgets(monkeypatch):
    browser = make(monkeypatch, LOGIN_SLOTS=6, RENEW_SLOTS=2)
    assert browser.login_slots is not browser.renew_slots
    assert browser.login_slots._value == 6
    assert browser.renew_slots._value == 2
    # A saturated login pool must leave the renewal budget untouched.
    for _ in range(6):
        assert browser.login_slots.locked() is False
        browser.login_slots._value -= 1
    assert browser.login_slots.locked() is True
    assert browser.renew_slots._value == 2


@pytest.mark.asyncio
async def test_start_rejects_once_the_active_cap_is_reached(monkeypatch):
    browser = make(monkeypatch, MAX_ACTIVE_LOGINS=3)
    browser._login = AsyncMock()  # do not launch a real browser
    for _ in range(3):
        browser.start('Student')
    with pytest.raises(ValueError, match='busy'):
        browser.start('One too many')
    # Cancel the no-op tasks so the loop shuts down cleanly.
    for job in browser.jobs.values():
        if job.task:
            job.task.cancel()
    await asyncio.gather(*(j.task for j in browser.jobs.values() if j.task), return_exceptions=True)
