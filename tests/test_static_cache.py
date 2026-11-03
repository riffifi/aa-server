from test_server import setup


def test_static_revalidates_without_redownloading_and_api_is_not_cached(setup):
    client, _, _, _ = setup
    asset = client.get('/static/site.js')
    assert asset.status_code == 200
    assert asset.headers['cache-control'] == 'public, max-age=0, must-revalidate'
    unchanged = client.get('/static/site.js', headers={'If-None-Match': asset.headers['etag']})
    assert unchanged.status_code == 304 and not unchanged.content
    assert client.get('/api/accounts').headers['cache-control'] == 'no-store'
