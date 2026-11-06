import asyncio
import time
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

from app import protocol
from app.pulse import BrowserLogins, MAX_RESPONSE


@pytest.mark.asyncio
@pytest.mark.parametrize('body,error', [(b'bad', None), (b'x' * (MAX_RESPONSE + 1), None), (None, OSError('read failed'))], ids=['invalid', 'oversized', 'read-failure'])
async def test_profile_response_is_disposed_on_all_exit_paths(body, error):
    response = SimpleNamespace(status=200, headers={'content-type': 'application/grpc-web+proto'},
                               body=AsyncMock(return_value=body, side_effect=error), dispose=AsyncMock())
    context = SimpleNamespace(request=SimpleNamespace(post=AsyncMock(return_value=response)))
    browser = BrowserLogins(None, AsyncMock())
    if error:
        with pytest.raises(OSError):
            await browser.capture(context)
    else:
        assert await browser.capture(context) is None
    response.dispose.assert_awaited_once()


@pytest.mark.asyncio
async def test_profile_response_is_disposed_on_cancellation():
    reading = asyncio.Event()
    async def body():
        reading.set()
        await asyncio.Event().wait()
    response = SimpleNamespace(body=body, dispose=AsyncMock())
    context = SimpleNamespace(request=SimpleNamespace(post=AsyncMock(return_value=response)))
    task = asyncio.create_task(BrowserLogins(None, AsyncMock()).capture(context))
    await reading.wait()
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    response.dispose.assert_awaited_once()


@pytest.mark.asyncio
@pytest.mark.parametrize('status', [200, 503])
async def test_qr_poll_response_is_disposed(status):
    from test_renewal import qr_fixture
    browser, page, context, job, _ = qr_fixture()
    context.request.get.return_value.status = status
    await browser.qr_step(context, page, job)
    context.request.get.return_value.dispose.assert_awaited_once()


@pytest.mark.asyncio
async def test_context_is_closed_when_setup_fails():
    context = SimpleNamespace(set_default_timeout=lambda _: None, set_default_navigation_timeout=lambda _: None,
                              route=AsyncMock(side_effect=RuntimeError('setup failed')), close=AsyncMock())
    browser = BrowserLogins(None, AsyncMock())
    browser._browser = AsyncMock(return_value=SimpleNamespace(new_context=AsyncMock(return_value=context)))
    with pytest.raises(RuntimeError, match='setup failed'):
        await browser._context()
    context.close.assert_awaited_once()


@pytest.mark.asyncio
async def test_expired_jobs_are_pruned_without_a_new_login_and_tasks_are_drained():
    from app.pulse import LoginJob
    browser = BrowserLogins(None, AsyncMock())
    waiting = asyncio.create_task(asyncio.Event().wait())
    old = LoginJob('old', 'hash', 'Owner', None, time.time() - 2000)
    old.task = waiting
    browser.jobs['old'] = old
    browser._cleanup_jobs()
    assert not browser.jobs
    await browser.close()
    assert waiting.cancelled() and not browser._retired_tasks
    assert browser._cleanup_handle is None


@pytest.mark.asyncio
async def test_recording_cancellation_drains_response_tasks_and_closes_context():
    reading, released = asyncio.Event(), asyncio.Event()
    async def body():
        reading.set()
        try:
            await asyncio.Event().wait()
        finally:
            released.set()
    response = SimpleNamespace(headers={'content-type': 'application/grpc-web+proto'},
                               url=protocol.HOST + '/rpc', body=body)
    listeners = {}
    def remove(event, callback):
        if listeners.get(event) is callback:
            listeners.pop(event)
    async def goto(*args, **kwargs):
        callback = listeners.get('response')
        if callback:
            callback(response)
    page = SimpleNamespace(on=lambda event, callback: listeners.update({event: callback}),
                           remove_listener=remove, goto=goto,
                           eval_on_selector_all=AsyncMock(return_value=[]))
    context = SimpleNamespace(new_page=AsyncMock(return_value=page), close=AsyncMock())
    vault = SimpleNamespace(secret=lambda _: {'storage_state': {'cookies': []}})
    browser = BrowserLogins(vault, AsyncMock())
    browser._context = AsyncMock(return_value=context)
    task = asyncio.create_task(browser.record({}, seconds=0))
    await asyncio.wait_for(reading.wait(), 1)
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    assert released.is_set() and not listeners
    context.close.assert_awaited_once()
