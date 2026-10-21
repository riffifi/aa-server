import pytest
from app.pulse import BrowserLogins


class Control:
    def __init__(self, text='', value=None, aria=None):
        self._text, self._value, self._aria = text, value, aria
        self.clicked = False
    async def get_attribute(self, name):
        return {'value': self._value, 'aria-label': self._aria}.get(name)
    async def inner_text(self):
        return self._text
    async def click(self, timeout=None):
        self.clicked = True


class Locator:
    def __init__(self, controls):
        self.controls = controls
    async def count(self):
        return len(self.controls)
    def nth(self, index):
        return self.controls[index]


class Page:
    def __init__(self, controls):
        self._locator = Locator(controls)
    def locator(self, selector):
        return self._locator


def browser():
    return BrowserLogins.__new__(BrowserLogins)  # no playwright needed for advance()


@pytest.mark.asyncio
async def test_advance_presses_accept_control_in_russian():
    accept = Control(text='Продолжить')
    await browser().advance(Page([accept]))
    assert accept.clicked


@pytest.mark.asyncio
async def test_advance_never_presses_a_reject_control():
    reject, accept = Control(text='Отмена'), Control(value='Разрешить')
    await browser().advance(Page([reject, accept]))
    assert accept.clicked and not reject.clicked


@pytest.mark.asyncio
async def test_advance_presses_a_lone_unlabelled_control_but_not_ambiguous_pairs():
    lone = Control(text='Next')
    await browser().advance(Page([lone]))
    assert lone.clicked
    a, b = Control(text='Option A'), Control(text='Option B')
    await browser().advance(Page([a, b]))
    assert not a.clicked and not b.clicked


@pytest.mark.asyncio
async def test_advance_is_safe_when_page_has_no_locator():
    class Bare:
        pass
    await browser().advance(Bare())  # must not raise


def test_required_action_presses_skip_not_confirm():
    import asyncio
    from app.pulse import BrowserLogins
    clicked = []

    class Loc:
        def __init__(self, items): self.items = items
        @property
        def first(self): return Loc(self.items[:1])
        def filter(self, has_text):
            return Loc([i for i in self.items if has_text.search(i)])
        async def count(self): return len(self.items)
        async def click(self, timeout=None): clicked.append(self.items[0])
        async def all(self): return []

    class Page:
        url = 'https://sso.mirea.ru/realms/mirea/login-actions/required-action?execution=x'
        def locator(self, selector):
            if selector.startswith('['):
                return Loc([])
            return Loc(['Подтвердить через MAX', 'Пропустить'])

    logins = BrowserLogins.__new__(BrowserLogins)
    asyncio.run(logins.advance(Page()))
    assert clicked == ['Пропустить']
