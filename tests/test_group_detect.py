from types import SimpleNamespace
from unittest.mock import AsyncMock
import pytest
from app import protocol
from app.main import Service
from app.vault import Vault


def vfield(number, value):
    return protocol.varint(number << 3) + protocol.varint(value)


def schedules_body(entries):
    # entries: list of (schedule_target, schedule_id, name, default_flag)
    payload = b''
    for target, sid, name, flag in entries:
        entry = protocol.field(1, vfield(1, target) + vfield(2, sid)) + protocol.field(2, name.encode())
        if flag:
            entry += vfield(4, 1)
        payload += protocol.field(1, entry)
    return protocol.frame(payload) + protocol.frame(b'grpc-status: 0\r\n', 128)


HEADERS = {'content-type': 'application/grpc-web+proto'}


def test_subscribed_group_extracts_id_and_name():
    body = schedules_body([(1, 5330, 'РВБО-02-25', 1)])
    assert protocol.subscribed_group(body, HEADERS) == (5330, 'РВБО-02-25')


def test_subscribed_group_prefers_group_over_teacher_and_default_flag():
    body = schedules_body([(2, 111, 'Teacher', 0), (1, 42, 'ИВБО-01-24', 0), (1, 5330, 'РВБО-02-25', 1)])
    assert protocol.subscribed_group(body, HEADERS) == (5330, 'РВБО-02-25')


def test_subscribed_group_none_when_no_group():
    body = schedules_body([(2, 111, 'Teacher', 1)])
    assert protocol.subscribed_group(body, HEADERS) == (None, None)


@pytest.mark.asyncio
async def test_refresh_account_auto_assigns_group_once(tmp_path):
    vault = Vault(tmp_path, 'a-long-enough-test-password')
    transport = SimpleNamespace(
        identity=AsyncMock(return_value=('valid', 'owner')),
        subscribed_group=AsyncMock(return_value=(5330, 'РВБО-02-25')))
    service = Service(vault, transport)
    row = vault.upsert('owner', 'Owner', {'cookie': 'session=owner'})
    assert row['group_id'] is None

    result = await service.refresh_account(row['id'])
    assert result['group_id'] == 5330 and result['group_name'] == 'РВБО-02-25'

    # A manual/existing group must not be overwritten on the next pass.
    transport.subscribed_group.return_value = (9999, 'OTHER')
    result = await service.refresh_account(row['id'])
    assert result['group_id'] == 5330
    vault.close()
