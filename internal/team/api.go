package team

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/scimux/scimux-team/internal/registry"
)

const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 2 * time.Minute
	maxHeaderBytes    = 16 << 10
	shutdownGrace     = 5 * time.Second
	maxBodyBytes      = 16 << 20

	publicWarning = "warning: plain HTTP is for trusted networks or behind a TLS proxy"
	revokeNote    = "the seat's token stops working and its channels and memberships are removed"
)

var marshalJSON = json.Marshal

type registerReq struct {
	JoinCode       string `json:"join_code"`
	InstallationID string `json:"installation_id"`
	Name           string `json:"name"`
}

type serverJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type registerResp struct {
	Token  string     `json:"token"`
	Server serverJSON `json:"server"`
}

func newHandler(reg *registry.Registry) http.Handler {
	return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
}

func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{}
}

func cleanPath(p string) string { return "" }
