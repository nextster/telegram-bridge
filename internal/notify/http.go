package notify

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
)

// The notification API has its own credential and never accepts the MCP token.
func NewHTTPHandler(service *Notifications, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		provided, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || !bearer || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="telegram-bridge-notifications"`)
			notificationError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			notificationError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			notificationError(w, http.StatusUnsupportedMediaType, "application_json_required")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var input NotificationInput
		if err := decoder.Decode(&input); err != nil {
			notificationError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			notificationError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		receipt, err := service.Send(r.Context(), input)
		if err != nil {
			// Do not expose Telegram errors, session details, database paths or payloads.
			notificationError(w, http.StatusUnprocessableEntity, "notification_rejected_check_destination_event_and_receipt")
			return
		}
		_ = json.NewEncoder(w).Encode(receipt)
	})
}

func notificationError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}
