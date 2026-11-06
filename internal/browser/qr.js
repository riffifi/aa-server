(() => {
  if (location.origin !== 'https://sso.mirea.ru') return null;
  const c = typeof kcContext !== 'undefined' ? kcContext : window.kcContext;
  if (!c) return null;
  return {pageId: c.pageId, qr: c.QRauthToken,
    sessionId: c.sessionId || c.auth?.sessionId || c.login?.sessionId,
    tabId: c.tabId || new URLSearchParams(location.search).get('tab_id'),
    action: c.url?.loginAction, execution: c.QRauthExecId,
    error: c.message?.type === 'error'};
})()
