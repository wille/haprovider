package internal

import (
	"fmt"
	"net"
	"net/http"
)

// getRequestID returns an identifier for the request.
func getRequestID(r *http.Request) string {
	header := r.Header.Get("X-Request-ID")

	if header != "" {
		if len(header) > 48 {
			header = header[:48]
		}
		return fmt.Sprintf("%s[%s]", r.RemoteAddr, header)
	}

	return header
}

func addXfwdHeaders(r *http.Request, w http.ResponseWriter) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)

	if r.Header.Get("X-Forwarded-For") != "" {
		w.Header().Set("X-Forwarded-For", r.Header.Get("X-Forwarded-For")+","+host)
	} else {
		w.Header().Set("X-Forwarded-For", host)
	}
}
