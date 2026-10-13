'use strict';
const base = location.pathname.replace(/\/$/, '');
async function poll() {
  try {
    const response = await fetch(base + '/state', { cache: 'no-store' });
    if (!response.ok) throw Error('This enrollment link has expired. Ask for a new one.');
    const result = await response.json();
    document.getElementById('label').textContent = result.label;
    document.getElementById('status').textContent = result.error || ({ waiting: 'Preparing your login…', login: 'Scan with another device, or open the confirmation link below in your signed-in MIREA browser.', finishing: 'Approval received. Confirming the Pulse session…', complete: 'Connected. The server has your session, and authorized phones will refresh it automatically.', expired: 'Login expired. Ask for a new enrollment link.' }[result.status] || result.status);
    const image = document.getElementById('qr'), link = document.getElementById('qr-link');
    if (result.qr) { image.src = base + '/qr'; image.hidden = false; link.href = result.qr; link.hidden = false; }
    if (!result.qr) { image.hidden = true; link.hidden = true; }
    if (['complete', 'expired', 'failed', 'cancelled'].includes(result.status)) { image.hidden = true; link.hidden = true; return; }
    setTimeout(poll, 2000);
  } catch (error) { document.getElementById('status').textContent = error.message; }
}
poll();
