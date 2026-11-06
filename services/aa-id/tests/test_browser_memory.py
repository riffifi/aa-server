import asyncio
import time
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

from aaid.mirea_login import MireaLogins, LoginJob
from aaid import pulse


@pytest.mark.asyncio
@pytest.mark.parametrize('failed', [False, True])
async def test_rpc_response_disposed_on_success_and_read_failure(failed):
    response = SimpleNamespace(status=200, headers={},
                               body=AsyncMock(return_value=b'data', side_effect=OSError() if failed else None),
                               dispose=AsyncMock())
    context = SimpleNamespace(request=SimpleNamespace(post=AsyncMock(return_value=response)))
    logins = MireaLogins()
    if failed:
        with pytest.raises(OSError):
            await logins._call(context, '/rpc', b'')
    else:
        assert await logins._call(context, '/rpc', b'') == (200, {}, b'data')
    response.dispose.assert_awaited_once()


@pytest.mark.asyncio
async def test_context_setup_failure_closes_allocated_context():
    context = SimpleNamespace(set_default_timeout=lambda _: None, set_default_navigation_timeout=lambda _: None,
                              route=AsyncMock(side_effect=RuntimeError()), close=AsyncMock())
    logins = MireaLogins()
    logins._browser = AsyncMock(return_value=SimpleNamespace(new_context=AsyncMock(return_value=context)))
    with pytest.raises(RuntimeError):
        await logins._context()
    context.close.assert_awaited_once()


@pytest.mark.asyncio
async def test_unclaimed_expired_credentials_are_pruned_without_a_new_login():
    logins = MireaLogins()
    old = LoginJob('old', 'hash', time.time() - 2000, status='complete',
                   session={'storage_state': {'origins': ['synthetic']}})
    logins.jobs['old'] = old
    logins._cleanup_jobs()
    assert not logins.jobs and logins._cleanup_handle is None
    await logins.close()
