package team

import (
	"net/http"

	"github.com/scimux/scimux-team/internal/registry"
)

// revokeSeatFn is the registry call behind DELETE /v1/seat. A test replaces
// it to force the failure branch a single registry will not produce.
var revokeSeatFn = func(reg *registry.Registry, id string) (registry.Code, error) {
	return reg.RevokeSeat(id)
}

func (s *server) putSeat(w http.ResponseWriter, r *http.Request) {}

func (s *server) deleteSeat(w http.ResponseWriter, r *http.Request) {}

func (s *server) deleteMembership(w http.ResponseWriter, r *http.Request) {}
