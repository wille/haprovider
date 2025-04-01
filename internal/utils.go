package internal

import (
	"net"
	"net/http"
)

func addXfwdHeaders(r *http.Request, w http.ResponseWriter) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)

	if r.Header.Get("X-Forwarded-For") != "" {
		w.Header().Set("X-Forwarded-For", r.Header.Get("X-Forwarded-For")+","+host)
	} else {
		w.Header().Set("X-Forwarded-For", host)
	}
}
