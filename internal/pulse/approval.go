package pulse

import (
	"fmt"
	"net/http"
)

var approvalReasons = map[uint64]string{
	1: "Неверный или просроченный QR-код", 2: "Ожидается подтверждение", 3: "Аккаунт не записан на занятие",
	4: "Журнал занятия в архиве", 5: "Присутствие в кампусе не подтверждено", 6: "Указано уважительное отсутствие", 7: "Указано нарушение распорядка",
}

func Approval(status int, headers http.Header, body []byte) (string, string) {
	switch status {
	case 401, 301, 302, 303, 307, 308:
		return "expired", "Сессия МИРЭА истекла"
	}
	if status != 200 {
		return "error", fmt.Sprintf("Пульс вернул HTTP %d", status)
	}
	message, grpcStatus, err := Message(body, headers)
	if err != nil {
		return "error", "Некорректный ответ Пульса"
	}
	if grpcStatus == 16 {
		return "expired", "Сессия МИРЭА истекла"
	}
	if grpcStatus != 0 || message == nil {
		return "error", fmt.Sprintf("Пульс отклонил запрос (gRPC %d)", grpcStatus)
	}
	values, err := Fields(message)
	if err == nil {
		if _, ok := values[2]; ok {
			return "approved", "Присутствие подтверждено"
		}
		if value, ok := values[1]; ok {
			var reason uint64
			if raw, ok := value.([]byte); ok {
				nested, err := Fields(raw)
				if err != nil {
					return "error", "Неизвестный ответ Пульса"
				}
				reason, _ = nested[1].(uint64)
			}
			message := approvalReasons[reason]
			if message == "" {
				message = "Пульс не подтвердил присутствие"
			}
			if reason == 2 {
				return "waiting", message
			}
			return "rejected", message
		}
	}
	return "error", "Неизвестный ответ Пульса"
}
