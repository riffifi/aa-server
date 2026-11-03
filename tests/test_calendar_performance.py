import asyncio
import time

import pytest

from app.schedule import Calendars


@pytest.mark.asyncio
async def test_stale_calendar_does_not_wait_and_refresh_is_shared():
    started, release = asyncio.Event(), asyncio.Event()
    calls = []

    async def fetch(url):
        calls.append(url)
        started.set()
        await release.wait()
        return 'BEGIN:VCALENDAR\nnew\nEND:VCALENDAR'

    calendars = Calendars(fetch)
    old = 'BEGIN:VCALENDAR\nold\nEND:VCALENDAR'
    calendars.cache[1] = (time.time() - 901, old)
    try:
        assert await asyncio.wait_for(calendars.source(1), 0.5) == old
        await asyncio.wait_for(started.wait(), 0.5)
        assert await asyncio.wait_for(calendars.source(1), 0.5) == old
        assert len(calls) == 1
        pending = calendars.tasks[1]
        release.set()
        await pending
        assert 'new' in await calendars.source(1)
    finally:
        await calendars.close()


@pytest.mark.asyncio
async def test_cancelled_first_reader_does_not_cancel_shared_download():
    started, release = asyncio.Event(), asyncio.Event()

    async def fetch(url):
        started.set()
        await release.wait()
        return 'BEGIN:VCALENDAR\nEND:VCALENDAR'

    calendars = Calendars(fetch)
    reader = asyncio.create_task(calendars.source(1))
    try:
        await asyncio.wait_for(started.wait(), 0.5)
        reader.cancel()
        with pytest.raises(asyncio.CancelledError):
            await reader
        assert not calendars.tasks[1].done()
        release.set()
        assert await calendars.source(1) == 'BEGIN:VCALENDAR\nEND:VCALENDAR'
    finally:
        await calendars.close()


@pytest.mark.asyncio
async def test_close_cancels_background_refresh():
    async def fetch(url):
        await asyncio.Event().wait()

    calendars = Calendars(fetch)
    calendars.cache[1] = (0, 'BEGIN:VCALENDAR\nEND:VCALENDAR')
    await calendars.source(1)
    pending = calendars.tasks[1]
    await calendars.close()
    assert pending.cancelled()
    assert not calendars.tasks
