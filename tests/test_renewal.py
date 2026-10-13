import json
import time
from types import SimpleNamespace
from unittest.mock import AsyncMock
import pytest
from app.main import Service
from app.pulse import BrowserLogins, LoginJob
from app.vault import Vault


@pytest.mark.asyncio
async def test_proactive_renewal_preserves_pass_and_emits_update(tmp_path):
    vault = Vault(tmp_path, 'a-long-test-password')
    transport = SimpleNamespace(identity=AsyncMock(return_value=('valid', 'owner')))
    service = Service(vault, transport)
    row = vault.upsert('owner', 'Owner', {'cookie': 'session=old', 'storage_state': {'cookies': [], 'origins': []}, 'expires_at': time.time() + 120})
    vault.save_pass(row['id'], '9876')
    async def renew(account):
        vault.upsert('owner', 'Owner', {'cookie': 'session=new', 'renewed_at': time.time()}, account_id=account['id'], session_expected=account['session_version'])
        await service.changed()
        return True
    service.browser.refresh = AsyncMock(side_effect=renew)
    result = await service.refresh_account(row['id'])
    assert result['state'] == 'valid'
    assert result['session_version'] == row['session_version'] + 1
    assert vault.secret(result)['pass_number'] == '9876'
    assert service.browser.refresh.await_count == 1
    vault.close()


@pytest.mark.asyncio
async def test_failed_renewal_keeps_valid_session_and_backs_off(tmp_path):
    vault = Vault(tmp_path, 'a-long-test-password')
    service = Service(vault, SimpleNamespace(identity=AsyncMock(return_value=('valid', 'owner'))))
    row = vault.upsert('owner', 'Owner', {'cookie': 'session=old', 'storage_state': {'cookies': [], 'origins': []}})
    service.browser.refresh = AsyncMock(return_value=False)
    for _ in range(2):
        result = await service.refresh_account(row['id'])
        assert result['state'] == 'valid'
        assert vault.secret(result)['cookie'] == 'session=old'
    assert service.browser.refresh.await_count == 1
    vault.close()


@pytest.mark.asyncio
async def test_cookie_only_accounts_and_outages_do_not_start_browser(tmp_path):
    vault = Vault(tmp_path, 'a-long-test-password')
    transport = SimpleNamespace(identity=AsyncMock(return_value=('expired', None)))
    service = Service(vault, transport)
    row = vault.upsert('owner', 'Owner', {'cookie': 'session=old'})
    service.browser.refresh = AsyncMock(return_value=False)
    await service.refresh_account(row['id'])
    service.browser.refresh.assert_not_awaited()
    row = vault.upsert('owner', 'Owner', {'cookie': 'session=old', 'storage_state': {'cookies': [], 'origins': []}})
    transport.identity.return_value = ('unavailable', None)
    await service.refresh_account(row['id'])
    service.browser.refresh.assert_not_awaited()
    assert vault.secret(vault.account(row['id']))['cookie'] == 'session=old'
    vault.close()


def qr_fixture():
    data = {'pageId': 'qr-login.ftl', 'qr': 'https://sso.mirea.ru/realms/mirea/qr-code-auth/scan?qrToken=dummy',
        'sessionId': 'session', 'tabId': 'tab', 'action': 'https://sso.mirea.ru/realms/mirea/login-actions/authenticate?execution=dummy', 'execution': 'dummy'}
    page = SimpleNamespace(evaluate=AsyncMock(return_value=data))
    response = SimpleNamespace(status=200, body=AsyncMock(return_value=json.dumps({'authenticated': True}).encode()))
    context = SimpleNamespace(request=SimpleNamespace(get=AsyncMock(return_value=response)))
    job = LoginJob('job', 'hash', 'Owner', None, time.time())
    browser = BrowserLogins(None, AsyncMock())
    return browser, page, context, job, data


@pytest.mark.asyncio
async def test_qr_approval_submits_once_and_hides_challenge():
    browser, page, context, job, data = qr_fixture()
    await browser.qr_step(context, page, job)
    assert job.status == 'finishing' and job.qr is None
    assert page.evaluate.await_count == 2
    submitted = page.evaluate.call_args.args[1]
    assert submitted == {'action': data['action'], 'execution': 'dummy'}
    await browser.qr_step(context, page, job)
    assert page.evaluate.await_count == 3  # Snapshot only; no second submission.
    assert context.request.get.await_count == 1


@pytest.mark.asyncio
async def test_qr_rejection_is_actionable_and_bad_action_never_submitted():
    browser, page, context, job, data = qr_fixture()
    context.request.get.return_value.body.return_value = json.dumps({'status': 'rejected'}).encode()
    with pytest.raises(ValueError, match='rejected'):
        await browser.qr_step(context, page, job)
    assert page.evaluate.await_count == 1
    context.request.get.return_value.body.return_value = json.dumps({'authenticated': True}).encode()
    data['action'] = 'https://untrusted.example/login'
    with pytest.raises(ValueError, match='completion form'):
        await browser.qr_step(context, page, job)
    assert page.evaluate.await_count == 2


@pytest.mark.asyncio
async def test_login_retries_profile_after_redirect_and_persists_sso(tmp_path):
    vault = Vault(tmp_path, 'a-long-test-password')
    browser = BrowserLogins(vault, AsyncMock())
    page = SimpleNamespace(url='https://pulse.mirea.ru/services', route=AsyncMock(), goto=AsyncMock())
    context = SimpleNamespace(new_page=AsyncMock(return_value=page), close=AsyncMock())
    browser._context = AsyncMock(return_value=context)
    credentials = {'cookie': 'session=confirmed', 'storage_state': {'cookies': [], 'origins': []}, 'renewed_at': time.time()}
    browser.capture = AsyncMock(side_effect=[RuntimeError('redirect in progress'), ('owner', credentials)])
    job = LoginJob('test', 'hash', 'Owner', None, time.time())
    from unittest.mock import patch
    with patch('app.pulse.asyncio.sleep', new=AsyncMock()):
        await browser._login(job)
    assert job.status == 'complete'
    assert vault.secret(vault.account(job.result_id)) == credentials
    assert browser.capture.await_count == 2
    context.close.assert_awaited_once()
    vault.close()
