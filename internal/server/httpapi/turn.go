package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"

	"termcall/internal/server/access"
	turncredentials "termcall/internal/server/turn"
)

func RegisterTURN(mux *http.ServeMux, gate *access.Gate, issuer *turncredentials.Issuer) {
	if issuer == nil {
		return
	}
	mux.HandleFunc("GET /v1/turn-credentials", func(writer http.ResponseWriter, request *http.Request) {
		if !gate.Authorize(request) {
			writeError(writer, http.StatusUnauthorized, "valid bearer access key is required")
			return
		}
		subjectBytes := make([]byte, 18)
		if _, err := rand.Read(subjectBytes); err != nil {
			writeError(writer, http.StatusInternalServerError, "cannot issue credentials")
			return
		}
		writeJSON(writer, http.StatusOK, issuer.Issue(base64.RawURLEncoding.EncodeToString(subjectBytes)))
	})
}
