from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

from app.browser_assets import BrowserAssets


@pytest.mark.asyncio
async def test_public_asset_outlives_fifteen_minute_renewal(monkeypatch):
    now = [1000.0]
    monkeypatch.setattr('app.browser_assets.time.monotonic', lambda: now[0])
    cache = BrowserAssets()
    await cache.route(route_for(headers={'cache-control': 'public, max-age=3600'}))
    now[0] += 901
    hit = route_for()
    await cache.route(hit)
    hit.fetch.assert_not_awaited()


@pytest.mark.asyncio
async def test_large_public_framework_asset_is_shared():
    cache = BrowserAssets()
    url = 'https://pulse.mirea.ru/_framework/dotnet.native.wasm'
    await cache.route(route_for(url=url, kind='fetch', body=b'x' * (12 * 1024 * 1024)))
    hit = route_for(url=url, kind='fetch')
    await cache.route(hit)
    hit.fetch.assert_not_awaited()
    assert cache.size == 12 * 1024 * 1024


def route_for(url='https://pulse.mirea.ru/assets/app.js', kind='script', headers=None, body=b'javascript'):
    response = SimpleNamespace(status=200, headers=headers or {'cache-control': 'public, max-age=600'},
                               body=AsyncMock(return_value=body), dispose=AsyncMock())
    route = SimpleNamespace(request=SimpleNamespace(url=url, method='GET', resource_type=kind),
                            continue_=AsyncMock(), fetch=AsyncMock(return_value=response), fulfill=AsyncMock())
    return route


@pytest.mark.asyncio
async def test_public_assets_are_reused_between_browser_contexts():
    cache = BrowserAssets()
    first, second = route_for(), route_for()
    await cache.route(first)
    await cache.route(second)
    first.fetch.assert_awaited_once_with(max_redirects=0)
    first.fetch.return_value.dispose.assert_awaited_once()
    second.fetch.assert_not_awaited()
    assert second.fulfill.call_args.kwargs['body'] == b'javascript'
    assert cache.stats()['saved_bytes'] == len(b'javascript')


@pytest.mark.asyncio
@pytest.mark.parametrize('headers', [
    {'cache-control': 'private, max-age=600'},
    {'cache-control': 'public, no-store, max-age=600'},
    {'cache-control': 'public, no-cache, max-age=600'},
    {'cache-control': 'public, max-age=600', 'set-cookie': 'session=secret'},
    {'cache-control': 'public, max-age=600', 'vary': 'Cookie'},
    {'cache-control': 'max-age=600'},
])
async def test_personal_or_nonpublic_responses_are_not_shared(headers):
    cache = BrowserAssets()
    await cache.route(route_for(headers=headers))
    assert not cache.entries


@pytest.mark.asyncio
@pytest.mark.parametrize('url,kind', [
    ('https://pulse.mirea.ru/api/profile.js', 'fetch'),
    ('https://pulse.mirea.ru/services', 'document'),
    ('https://sso.mirea.ru/app.js', 'script'),
    ('https://ddos-guard.net/check.js', 'script'),
])
async def test_authentication_and_api_requests_pass_through(url, kind):
    route = route_for(url, kind)
    await BrowserAssets().route(route)
    route.continue_.assert_awaited_once()
    route.fetch.assert_not_awaited()


@pytest.mark.asyncio
async def test_memory_budget_and_expiration(monkeypatch):
    now = [1000]
    monkeypatch.setattr('app.browser_assets.time.monotonic', lambda: now[0])
    cache = BrowserAssets(budget=12, max_item=10)
    await cache.route(route_for(body=b'12345678'))
    await cache.route(route_for('https://pulse.mirea.ru/other.js', body=b'abcdefgh'))
    assert cache.size == 8 and len(cache.entries) == 1
    now[0] += 601
    route = route_for('https://pulse.mirea.ru/other.js', body=b'new')
    await cache.route(route)
    route.fetch.assert_awaited_once()
    assert cache.size == 3


@pytest.mark.asyncio
async def test_decoded_assets_do_not_replay_wire_encoding():
    cache = BrowserAssets()
    headers = {'cache-control': 'public, max-age=600', 'content-encoding': 'gzip', 'content-length': '99'}
    await cache.route(route_for(headers=headers))
    hit = route_for()
    await cache.route(hit)
    assert 'content-encoding' not in hit.fulfill.call_args.kwargs['headers']
    assert 'content-length' not in hit.fulfill.call_args.kwargs['headers']
