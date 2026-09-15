package notify

import (
	"encoding/json"
	"io"
	"log"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/nextster/telegram-bridge/internal/apitoken"
	"github.com/nextster/telegram-bridge/internal/db"
)

// NewHTTPHandler accepts only personal notification tokens. The token decides
// the sending account; MCP and OAuth tokens are never accepted.
func NewHTTPHandler(service *Notifications, store *db.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		provided, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		var accountID int64
		if bearer && provided != "" {
			owner, ok, err := store.ResolveAPIToken(r.Context(), apitoken.Hash(provided), db.APITokenScopeNotify, time.Now())
			if err != nil {
				log.Printf("resolve notification token failed: %v", err)
				notificationError(w, http.StatusServiceUnavailable, "temporarily_unavailable")
				return
			}
			if ok {
				accountID = owner
			}
		}
		if accountID <= 0 {
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
		receipt, err := service.Send(r.Context(), accountID, input)
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
