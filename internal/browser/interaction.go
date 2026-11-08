package browser

import (
	"encoding/json"

	"net/url"
	"regexp"
	"strings"
	"time"

	pw "github.com/mxschmitt/playwright-go"
)

// Keep patterns in sync with both existing Python login workers.
var accept = regexp.MustCompile(`(?i)продолж|подтверд|разреш|войт|вход|accept|allow|authoriz|continu|confirm|proceed|grant|submit|ок(?:$|[^\pL\pN_])|ok\b`)
var reject = regexp.MustCompile(`(?i)отмен|отклон|запрет|назад|cancel|deny|reject|decline|\bback\b|logout|выход|выйти`)
var skip = regexp.MustCompile(`(?i)пропуст|не сейчас|позже|skip|later|not now`)

func reauthenticationRequired(page pw.Page) bool {
	raw, err := page.Evaluate(qrScript)
	if err != nil {
		return false
	}
	data, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	switch data["pageId"] {
	case "login.ftl", "login-password.ftl", "login-username.ftl", "login-otp.ftl":
		return true
	}
	qr, _ := data["qr"].(string)
	return qr != ""
}

func click(target pw.Locator) bool {
	count, err := target.Count()
	if err != nil || count == 0 {
		return false
	}
	return target.First().Click(pw.LocatorClickOptions{Timeout: pw.Float(3000)}) == nil
}
func advance(page pw.Page, identity bool) {
	deadline := time.Time{}
	if !identity {
		deadline = time.Now().Add(15 * time.Second)
	}
	remaining := func(limit float64) float64 {
		if deadline.IsZero() {
			return limit
		}
		left := float64(time.Until(deadline).Milliseconds())
		if left < 1 {
			return 0
		}
		if left < limit {
			return left
		}
		return limit
	}
	press := func(target pw.Locator) bool {
		if remaining(3000) == 0 {
			return false
		}
		count, err := target.Count()
		if err != nil || count == 0 {
			return false
		}
		timeout := remaining(3000)
		if timeout == 0 {
			return false
		}
		return target.First().Click(pw.LocatorClickOptions{Timeout: pw.Float(timeout)}) == nil
	}
	if strings.Contains(page.URL(), "/login-actions/required-action") {
		for _, selector := range []string{"[name=cancel-aia]", "[name*=skip i]", "[id*=skip i]"} {
			if press(page.Locator(selector)) {
				return
			}
		}
		selector := "a, button, input[type=submit], [role=button]"
		if !identity {
			selector += ", input[type=button]"
		}
		if press(page.Locator(selector).Filter(pw.LocatorFilterOptions{HasText: skip})) {
			return
		}
		if !identity {
			controls := page.Locator("input[type=submit], input[type=button]")
			count, _ := controls.Count()
			for i := 0; i < count; i++ {
				timeout := remaining(5000)
				if timeout == 0 {
					return
				}
				value, _ := controls.Nth(i).GetAttribute("value", pw.LocatorGetAttributeOptions{Timeout: pw.Float(timeout)})
				if skip.MatchString(value) && press(controls.Nth(i)) {
					return
				}
			}
		}
		return
	}
	controls := page.Locator("button:visible, input[type=submit]:visible, input[type=button]:visible, a[role=button]:visible")
	count, err := controls.Count()
	if err != nil {
		return
	}
	if count > 25 {
		count = 25
	}
	var fallback pw.Locator
	for i := 0; i < count; i++ {
		timeout := remaining(5000)
		if timeout == 0 {
			return
		}
		control := controls.Nth(i)
		label, err := control.GetAttribute("value", pw.LocatorGetAttributeOptions{Timeout: pw.Float(timeout)})
		if err != nil {
			continue
		}
		if label == "" {
			timeout = remaining(5000)
			if timeout == 0 {
				return
			}
			label, err = control.InnerText(pw.LocatorInnerTextOptions{Timeout: pw.Float(timeout)})
			if err != nil {
				continue
			}
		}
		if label == "" && !identity {
			timeout = remaining(5000)
			if timeout == 0 {
				return
			}
			label, _ = control.GetAttribute("aria-label", pw.LocatorGetAttributeOptions{Timeout: pw.Float(timeout)})
		}
		label = strings.TrimSpace(label)
		if reject.MatchString(label) {
			continue
		}
		if accept.MatchString(label) {
			press(control)
			return
		}
		if fallback == nil {
			fallback = control
		}
	}
	if fallback != nil && count == 1 {
		press(fallback)
	}
}
func qrStep(c pw.BrowserContext, page pw.Page, update func(Update), identity bool, traffic ...*Traffic) (bool, error) {
	raw, err := page.Evaluate(qrScript)
	if err != nil {
		return false, nil
	}
	data, ok := raw.(map[string]any)
	if !ok {
		return false, nil
	}
	if data["error"] == true || data["pageId"] == "error.ftl" {
		if identity {
			return false, &UserError{"МИРЭА отклонил вход. Начните заново."}
		}
		return false, &UserError{"MIREA rejected the login; start a fresh QR login."}
	}
	text := func(key string) string { value, _ := data[key].(string); return value }
	qr, err := url.Parse(text("qr"))
	if err != nil || qr.Scheme != "https" || qr.Hostname() != "sso.mirea.ru" || qr.Path != "/realms/mirea/qr-code-auth/scan" {
		click(page.Locator("a, button").Filter(pw.LocatorFilterOptions{HasText: regexp.MustCompile(`(?i)QR`)}))
		return false, nil
	}
	update(Update{Status: "login", QR: text("qr")})
	if text("sessionId") == "" || text("tabId") == "" {
		return false, nil
	}
	endpoint := "https://sso.mirea.ru/realms/mirea/qr-code-auth/check-status?" + url.Values{"sessionId": {text("sessionId")}, "tabId": {text("tabId")}}.Encode()
	response, err := c.Request().Get(endpoint, pw.APIRequestContextGetOptions{Timeout: pw.Float(15000), MaxRedirects: pw.Int(0)})
	if err != nil {
		return false, nil
	}
	reply, err := consume(response)
	if err == nil && len(traffic) > 0 {
		traffic[0].api(len(reply.Body))
	}
	if err != nil || reply.Status != 200 {
		return false, nil
	}
	var result map[string]any
	if json.Unmarshal(reply.Body, &result) != nil {
		return false, nil
	}
	switch result["status"] {
	case "timeout", "rejected", "session_not_found":
		if identity {
			return false, &UserError{"QR-код истёк или вход отклонён. Начните заново."}
		}
		return false, &UserError{"The QR approval expired or was rejected. Start a new login."}
	}
	if result["authenticated"] != true {
		return false, nil
	}
	action, err := url.Parse(text("action"))
	if err != nil || !AllowedURL(text("action")) || action.Hostname() != "sso.mirea.ru" || action.Path != "/realms/mirea/login-actions/authenticate" || text("execution") == "" {
		if identity {
			return false, &UserError{"МИРЭА изменил форму входа. Нужно обновить сервер."}
		}
		return false, &UserError{"MIREA changed its QR completion form. Update the server."}
	}
	update(Update{Status: "finishing"})
	_, _ = page.Evaluate(`data => {const form=document.createElement('form');form.method='post';form.action=data.action;const input=document.createElement('input');input.type='hidden';input.name='authenticationExecution';input.value=data.execution;form.appendChild(input);document.body.appendChild(form);HTMLFormElement.prototype.submit.call(form)}`, map[string]any{"action": text("action"), "execution": text("execution")})
	return true, nil
}
